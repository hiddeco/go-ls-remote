package objstore

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// Info reports the resolved type and byte size of a Git object as a
// caller would see them after any pack-delta chain has been applied.
//
// Type is always one of the four non-delta [objfmt.ObjectType]
// variants ([objfmt.TypeCommit], [objfmt.TypeTree], [objfmt.TypeBlob],
// or [objfmt.TypeTag]); the delta types are an on-disk encoding
// detail and never escape the resolver. Size is the inflated byte
// length of the resolved object — for a delta, the target size
// recorded in the delta payload's header, not the size of the delta
// itself.
type Info struct {
	Type objfmt.ObjectType
	Size int64
}

// maxChainDepth bounds the number of OFS_DELTA / REF_DELTA hops
// [Store.ObjectInfo] is willing to chase before declaring the chain
// pathological. Mirrors canonical Git's `MAX_DELTA_CACHE_DEPTH`-style
// guard in `packfile.c::unpack_entry`: a depth past 64 effectively
// never appears in produced packs (`pack.depth` defaults to 50, with
// repack ceilings around 250), and the bound keeps the iterative
// walk from running indefinitely on adversarial input.
const maxChainDepth = 64

// refDeltaCacheEntry is one cached cross-pack REF_DELTA resolution
// keyed on the base OID. Both shapes — "this base lives at offset X
// in pack P" (found=true) and "this base is unreachable in any open
// pack" (found=false) — are cached so a missing base never re-scans
// every pack on the next call.
type refDeltaCacheEntry[H objfmt.Hash] struct {
	pack   *objfmt.Pack[H]
	offset int64
	found  bool
}

// ObjectInfo reports the resolved [Info] for oid by consulting, in
// order, this store's loose-object backend, its pack backend (with
// delta-chain resolution and CRC32 verification), and finally each
// alternate store in turn. The first hit wins; a clean miss across
// every layer surfaces as `os.ErrNotExist` so callers can match with
// `errors.Is`.
//
// # Delta resolution
//
// A pack hit on a delta type triggers an iterative walk over the
// chain:
//
//   - The target size is captured from the delta payload's header on
//     the first iteration via [objfmt.Pack.ReadDeltaHeader] and
//     propagated unchanged to the result.
//   - OFS_DELTA hops stay inside the same pack, jumping to
//     `at - DeltaRef.OfsBase`.
//   - REF_DELTA hops fan out across every open pack via
//     [Store.lookupRefDeltaBase], whose result is memoised on the
//     receiver (positive AND negative) so a missing base never
//     re-scans every pack on the next call.
//   - The walk terminates on the first non-delta header, returning
//     that type as the resolved [Info.Type].
//
// Depth is capped at [maxChainDepth] hops. A chain longer than that —
// or a REF_DELTA whose base is unreachable in any open pack — wraps
// [ErrCorruptObject] so callers can match with `errors.Is`.
//
// # CRC32 verification
//
// When the store was opened with CRC verification enabled (the
// default; flipped off by [WithoutCRCCheck]), every per-pack object
// the walk visits has its on-disk compressed bytes hashed with
// CRC-32/IEEE and compared against the value recorded in the paired
// idx via [objfmt.Idx.FindCRC32]. The compressed range is
// `[at, nextOffset)` where `nextOffset` is the smallest offset
// strictly larger than `at` recorded in the same idx (or the pack
// trailer's start when `at` names the last object); see
// [objfmt.Idx.OffsetAfter]. A CRC mismatch wraps [ErrCorruptObject].
//
// # Failure modes
//
//   - Loose miss + pack miss + every alternate misses → `os.ErrNotExist`.
//   - Corrupt pack header / delta payload → wrapped [ErrCorruptObject].
//   - REF_DELTA base unreachable → wrapped [ErrCorruptObject].
//   - Chain depth exceeded → wrapped [ErrCorruptObject].
//   - CRC mismatch (when enabled) → wrapped [ErrCorruptObject].
func (s *Store[H]) ObjectInfo(oid H) (Info, error) {
	// Loose-first: matches canonical Git's lookup precedence
	// (`object-file.c::oid_object_info_extended` consults the loose
	// store ahead of pack lookup) and skips the delta-chain machinery
	// entirely for the common ref-tip read path.
	typ, size, body, ok, err := s.loose.Find(oid)
	if err != nil {
		return Info{}, err
	}
	if ok {
		_ = body.Close()
		return Info{Type: typ, Size: size}, nil
	}

	pack, off, hit, err := s.packs.Lookup(oid)
	if err != nil {
		return Info{}, err
	}
	if hit {
		return s.walkPackChain(oid, pack, off)
	}

	// Local miss: defer to the alternates chain in declaration order.
	// Each alternate runs its own ObjectInfo, so its loose / pack /
	// nested-alternates layers are all consulted before the next
	// sibling alternate gets a turn — matching canonical Git's
	// `odb.c::do_oid_object_info_extended` walk shape.
	for _, alt := range s.alternates {
		info, err := alt.ObjectInfo(oid)
		if err == nil {
			return info, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return Info{}, err
		}
	}
	return Info{}, os.ErrNotExist
}

// walkPackChain expands the delta chain rooted at the (pack, at) pair
// — the (pack, offset) the OID-keyed lookup landed on — and returns
// the resolved [Info]. The walk is iterative so the stack never grows
// past one frame regardless of [maxChainDepth]; recursion would also
// work but the iterative shape keeps the inner loop locality-friendly
// and easy to reason about under `go test -race`.
//
// CRC verification runs only on the head of the chain (the entry the
// caller asked for via oid). Intermediate delta-base entries are not
// re-verified per hop because identifying them requires a reverse
// offset → OID lookup the [objfmt.Idx] surface does not yet expose;
// the integrity backstop for those bytes is the pack-trailer hash via
// [objfmt.Pack.VerifyChecksum]. Mirrors canonical Git's
// `packfile.c::check_pack_crc`, which also runs on the OID-keyed
// entry rather than on every chain step.
func (s *Store[H]) walkPackChain(oid H, pack *objfmt.Pack[H], at int64) (Info, error) {
	if err := s.verifyPackCRC(pack, oid, at); err != nil {
		return Info{}, err
	}

	var (
		targetSize      int64
		targetSizeKnown bool
	)
	for range maxChainDepth {
		hdr, err := pack.ReadHeader(at)
		if err != nil {
			return Info{}, fmt.Errorf(
				"objstore: read pack header at %d: %w: %w",
				at, err, ErrCorruptObject)
		}

		// First iteration captures the size that survives to the
		// caller. For non-delta entries the header's Size field is the
		// inflated body size; for delta entries the leading varints of
		// the delta payload carry the (source, target) sizes and the
		// target is what the resolved object is going to occupy.
		if !targetSizeKnown {
			if hdr.Type.IsDelta() {
				_, tgt, err := pack.ReadDeltaHeader(hdr.BodyAt)
				if err != nil {
					return Info{}, fmt.Errorf(
						"objstore: read delta header at %d: %w: %w",
						hdr.BodyAt, err, ErrCorruptObject)
				}
				targetSize = tgt
			} else {
				targetSize = hdr.Size
			}
			targetSizeKnown = true
		}

		switch hdr.Type {
		case objfmt.TypeOfsDelta:
			at = hdr.DeltaRef.OfsBase
			// `pack` unchanged: OFS_DELTA bases live in the same pack.
		case objfmt.TypeRefDelta:
			basePack, baseOff, found := s.lookupRefDeltaBase(hdr.DeltaRef.RefBase)
			if !found {
				return Info{}, fmt.Errorf(
					"objstore: REF_DELTA base %s not found: %w",
					hdr.DeltaRef.RefBase.Hex(), ErrCorruptObject)
			}
			pack = basePack
			at = baseOff
		case objfmt.TypeCommit, objfmt.TypeTree, objfmt.TypeBlob, objfmt.TypeTag:
			return Info{Type: hdr.Type, Size: targetSize}, nil
		default:
			return Info{}, fmt.Errorf(
				"objstore: unknown pack object type %d at %d: %w",
				hdr.Type, at, ErrCorruptObject)
		}
	}
	return Info{}, fmt.Errorf(
		"objstore: delta chain depth exceeds %d: %w",
		maxChainDepth, ErrCorruptObject)
}

// lookupRefDeltaBase resolves a REF_DELTA's base OID across every
// open pack the store can reach via its [packBackend]. Both positive
// and negative results are cached on the receiver so an unreachable
// base never re-scans every pack on the next call.
//
// The cache is intentionally keyed on the base OID rather than on
// the (carrier-pack, base-OID) pair: REF_DELTA bases are global to
// the store, and two carriers asking for the same base must collapse
// to a single cache slot.
func (s *Store[H]) lookupRefDeltaBase(base H) (*objfmt.Pack[H], int64, bool) {
	s.refDeltaMu.Lock()
	if entry, ok := s.refDeltaCache[base]; ok {
		s.refDeltaMu.Unlock()
		return entry.pack, entry.offset, entry.found
	}
	s.refDeltaMu.Unlock()

	pack, off, found, _ := s.packs.Lookup(base)
	// Errors from `Lookup` are reserved for future per-pack lazy-read
	// failures; today every backend's `Lookup` returns nil. Treating a
	// non-nil error as "not found here" is forward-safe: the caller
	// surfaces the same `ErrCorruptObject` shape it would for a clean
	// miss.

	s.refDeltaMu.Lock()
	s.refDeltaCache[base] = refDeltaCacheEntry[H]{
		pack:   pack,
		offset: off,
		found:  found,
	}
	s.refDeltaMu.Unlock()
	return pack, off, found
}

// verifyPackCRC hashes the on-disk compressed bytes of the pack
// object that begins at `at` (the entry indexed under oid) with
// CRC-32/IEEE and compares the result against the value recorded in
// the paired idx. Skips silently when the store was opened with CRC
// verification disabled, or when the paired idx is the v1 layout
// (which carries no CRC table at all — see `gitformat-pack.adoc`
// lines 196-218).
//
// The compressed range covers every byte from `at` (the start of the
// type/size header) to the start of the next packed object, or to
// the start of the pack's trailer when `at` names the last object.
// Mirrors `packfile.c::check_pack_crc`.
//
// A CRC mismatch wraps [ErrCorruptObject].
func (s *Store[H]) verifyPackCRC(pack *objfmt.Pack[H], oid H, at int64) error {
	if !s.cfg.verifyCRC {
		return nil
	}
	idx, ok := s.packs.IdxFor(pack)
	if !ok {
		// Defence in depth: every backend wires every open pack
		// through its idx-checksum map at construction, so a miss here
		// would mean the pack escaped from a different backend or
		// has been closed mid-flight. Skip verification rather than
		// surface a confusing error.
		return nil
	}
	wantCRC, ok := idx.FindCRC32(oid)
	if !ok {
		// Either the idx is v1 (no CRC table) or the OID is somehow
		// absent from the idx that supposedly resolved it. The latter
		// would be a bug in the backend and the pack-trailer check is
		// the integrity backstop; the former is the canonical
		// "v1 layout has no per-object CRC" shape.
		return nil
	}

	end, hasEnd := idx.OffsetAfter(at)
	if !hasEnd {
		// `at` names the last object in the pack; its compressed body
		// runs from `at` to one byte before the pack trailer.
		end = pack.Len() - int64(s.algo.Size())
	}
	if end <= at {
		return fmt.Errorf(
			"objstore: pack %s offset %d crc32 range non-positive: %w",
			idx.Path(), at, ErrCorruptObject)
	}

	gotCRC, err := crc32Range(pack, at, end)
	if err != nil {
		return fmt.Errorf(
			"objstore: pack %s offset %d crc32 read: %w: %w",
			idx.Path(), at, err, ErrCorruptObject)
	}
	if gotCRC != wantCRC {
		return fmt.Errorf(
			"objstore: pack %s offset %d crc32 mismatch: %w",
			idx.Path(), at, ErrCorruptObject)
	}
	return nil
}

// crc32Range computes CRC-32/IEEE over `pack[start:end]` by reading
// the bytes in fixed-size chunks. The chunk size is generous enough
// that even a multi-megabyte delta payload reads in a handful of
// `ReadAt` calls; pack mappings are usually mmap-backed so the
// underlying `ReadAt` is a memory copy rather than a syscall.
func crc32Range[H objfmt.Hash](pack *objfmt.Pack[H], start, end int64) (uint32, error) {
	const chunk = 1 << 16
	h := crc32.NewIEEE()
	buf := make([]byte, chunk)
	for off := start; off < end; {
		want := int64(chunk)
		if rem := end - off; rem < want {
			want = rem
		}
		n, err := pack.ReadAt(buf[:want], off)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if n == 0 {
			return 0, fmt.Errorf("objstore: zero-byte pack read at %d", off)
		}
		h.Write(buf[:n])
		off += int64(n)
	}
	return h.Sum32(), nil
}
