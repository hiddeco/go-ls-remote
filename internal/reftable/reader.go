package reftable

import (
	"bytes"
	"fmt"
	"iter"

	"golang.org/x/exp/mmap"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// RefRecord is the public, read-only view of one reference stored in a
// reftable file.
//
// Three shapes are observable: a value record (Value populated, Peeled
// and TargetRef zero); a peeled-tag record (Value and Peeled populated,
// TargetRef zero); and a symbolic-ref record (TargetRef populated,
// Value and Peeled zero). Tombstones — the on-disk markers used to
// shadow a name in a later table — are never returned to callers; the
// reader filters them out so [Reader.IterRefs] and [Reader.FindRef]
// only ever yield observable refs.
//
// The hash type parameter `H` carries the on-disk OID width: a SHA-1
// reftable yields `RefRecord[objfmt.SHA1Hash]`, a SHA-256 file
// `RefRecord[objfmt.SHA256Hash]`. Mismatched-algorithm files surface as
// [ErrMixedHashAlgo] at [OpenReader] time, before any record is
// decoded. The [Reader.HashAlgo] return value carries the same
// identity as an [objfmt.Algo] interface for capability emission and
// consistency checks.
//
// The on-disk update_index is intentionally not exposed: ls-remote
// consumers only need the merged-view value, and the merged-view
// surface in [Stack] hides update_index by construction. A future
// caller (status, reflog) that needs the field should add it here
// rather than re-deriving it.
type RefRecord[H objfmt.Hash] struct {
	Name      string
	Value     H
	TargetRef string
	Peeled    H
}

// Reader is a read-only view of a single reftable file.
//
// A Reader opens its file via `golang.org/x/exp/mmap` at construction.
// The current implementation reads the file's bytes into a heap buffer
// up front and walks that buffer; the mmap handle is retained so
// [Reader.Close] releases the underlying OS resource in the right
// order. A random-access mmap path is a potential follow-up.
//
// Reader does not merge across tables — that is the job of [Stack].
// A single Reader sees only the records present in the file it was
// opened on, including any tombstones (which are filtered out before
// the public surface).
//
// # Concurrency
//
// After [OpenReader] returns, the read methods ([Reader.HashAlgo],
// [Reader.IterRefs], [Reader.FindRef]) are safe for concurrent use by
// any number of goroutines: every field they touch is set once at
// construction and never written again. [Reader.Close] is NOT safe to
// call concurrently with read methods or with itself; callers must
// drain in-flight reads before closing and serialize Close calls. Once
// drained, Close is idempotent — a second call returns nil without
// touching the OS.
type Reader[H objfmt.Hash] struct {
	mmap   *mmap.ReaderAt
	file   []byte
	header header
}

// OpenReader opens the reftable at path, validates its header and
// trailer, and returns a [*Reader] ready for [Reader.IterRefs] and
// [Reader.FindRef] calls.
//
// The file is opened via `golang.org/x/exp/mmap` and read into a heap
// buffer; the mmap handle is retained for ordered close. The header is
// parsed up-front so HashAlgo and update-index bounds are available
// without further I/O; the trailer is verified via its CRC-32 so
// corrupted files surface as [ErrTrailerChecksum] before any record is
// decoded. Errors from the underlying [mmap.Open] propagate unwrapped
// (e.g. fs.ErrNotExist for a missing path).
//
// The on-disk hash algorithm is checked against the type parameter `H`:
// opening a SHA-256 reftable as `Reader[objfmt.SHA1Hash]` (or vice
// versa) fails with [ErrMixedHashAlgo] before any record is decoded.
// This is the type-system safety net that replaces the runtime
// `algo.Size()` arithmetic the untyped decoder threaded through.
//
// On any error after the open succeeds, the mapping is closed before
// the error is returned; callers do not need to call [Reader.Close]
// on a failed open.
func OpenReader[H objfmt.Hash](path string) (*Reader[H], error) {
	rdr, err := mmap.Open(path)
	if err != nil {
		return nil, err
	}

	size := rdr.Len()
	if size < headerSizeV1 {
		rdr.Close()
		return nil, fmt.Errorf("reftable: have %d bytes, need %d for header: %w", size, headerSizeV1, ErrShortFile)
	}

	file := make([]byte, size)
	if _, err := rdr.ReadAt(file, 0); err != nil {
		rdr.Close()
		return nil, fmt.Errorf("reftable: read %s: %w", path, err)
	}

	h, err := parseHeader(file)
	if err != nil {
		rdr.Close()
		return nil, err
	}
	var zero H
	if h.algo.Size() != len(zero) {
		rdr.Close()
		return nil, fmt.Errorf("reftable: file %s declares %s, reader instantiated for %d-byte hash: %w",
			path, h.algo, len(zero), ErrMixedHashAlgo)
	}
	if err := verifyTrailer(file, h); err != nil {
		rdr.Close()
		return nil, err
	}

	return &Reader[H]{mmap: rdr, file: file, header: h}, nil
}

// Close releases the memory mapping that backs r.
//
// Close is idempotent: a second call on an already-closed Reader
// returns nil without touching the OS. Close is NOT safe to call
// concurrently with read methods or with itself; see the [Reader]
// concurrency contract. Calling any read method after Close is
// undefined.
func (r *Reader[H]) Close() error {
	if r.mmap == nil {
		return nil
	}
	m := r.mmap
	r.mmap = nil
	r.file = nil
	return m.Close()
}

// HashAlgo returns the hash algorithm declared by the file header.
//
// The result is [objfmt.SHA1] for v1 files and either [objfmt.SHA1]
// or [objfmt.SHA256] for v2 files (decoded from the on-disk hash_id
// tag). Under the typed reader, the return value is redundant with the
// type parameter `H` and is preserved as the identity-only accessor
// used at capability emission sites where the static type has been
// erased.
func (r *Reader[H]) HashAlgo() objfmt.Algo {
	return r.header.algo
}

// IterRefs returns an iterator that yields every observable ref in the
// file in on-disk (sorted) order.
//
// The iterator walks each ref block from the start of the file in
// turn, decoding records sequentially and yielding non-tombstone
// records as [RefRecord] values. Tombstone records (value_type=0) are
// elided: they have no observable meaning in a single file and exist
// only as shadowing markers within a stack.
//
// Errors short-circuit the walk: on a decode failure the iterator
// yields one (RefRecord{}, err) pair and stops. Successful walks emit
// only (rec, nil) pairs and terminate by exhausting the ref blocks.
//
// Breaking out of the range loop is supported and leaks no goroutines:
// the iterator is a pure stack-based [iter.Seq2] that returns control
// when the consumer's yield returns false.
func (r *Reader[H]) IterRefs() iter.Seq2[RefRecord[H], error] {
	return func(yield func(RefRecord[H], error) bool) {
		for rec, err := range r.iterAllRefs() {
			if err != nil {
				yield(RefRecord[H]{}, err)
				return
			}
			if rec.ValueType == refValueDeletion {
				continue
			}
			if !yield(liftRefRecord(rec), nil) {
				return
			}
		}
	}
}

// iterAllRefs walks every ref_record in the file in on-disk (sorted)
// order and yields the raw, internal [refRecord] — including tombstones
// (value_type=0). It is the shared engine behind [Reader.IterRefs] and
// [Stack]'s merged-view construction; the public iterator filters
// tombstones, the stack uses them to delete shadowed entries.
//
// The walker scans ref blocks sequentially from the file header
// onwards, stopping at the first non-ref block (obj/index/footer).
// Block advancement uses blockLen, with the first block's length
// folding in the file header preamble; thereafter pos rounds up to
// blockSize when set.
//
// The block-walk shape (first-block firstByteOffset, blockSize round-up,
// stop-on-non-ref) is the same one [seekLinear] uses for the no-index
// fallback. The two are intentionally not factored together: this
// walker yields every record, while [seekLinear] returns the single
// leaf block that should contain a probe — different signatures, and
// the duplication is a handful of lines.
func (r *Reader[H]) iterAllRefs() iter.Seq2[refRecord[H], error] {
	return func(yield func(refRecord[H], error) bool) {
		headerLen := uint32(r.header.size())

		pos := uint32(0)
		first := true
		for pos < uint32(len(r.file)) {
			firstByteOffset := uint32(0)
			var slice []byte
			if first {
				firstByteOffset = headerLen
				slice = r.file
			} else {
				slice = r.file[pos:]
			}

			if uint32(len(slice)) <= firstByteOffset {
				return
			}
			if slice[firstByteOffset] != blockTypeRef {
				return
			}

			blk, err := parseBlock(slice, firstByteOffset)
			if err != nil {
				yield(refRecord[H]{}, fmt.Errorf("reftable: parse ref block at %d: %w", pos, err))
				return
			}

			recordsEnd := blk.header.blockLen - 2 - 3*uint32(blk.header.restartCount)
			cur := firstByteOffset + 4
			var prevKey []byte
			for cur < recordsEnd {
				rec, key, n, err := decodeRefRecord[H](blk.bytes[cur:recordsEnd], prevKey, r.header.minUpdateIndex)
				if err != nil {
					yield(refRecord[H]{}, fmt.Errorf("reftable: decode ref_record at %d: %w", pos+cur, err))
					return
				}
				if !yield(rec, nil) {
					return
				}
				prevKey = key
				cur += uint32(n)
			}

			// For the first ref block, blockLen folds in the file
			// header preamble (firstByteOffset bytes), so adding it to
			// pos=0 lands precisely at the second block's start. For
			// every later block, blockLen excludes the file header and
			// adds atop pos directly.
			nextPos := pos + blk.header.blockLen
			if r.header.blockSize > 0 {
				nextPos = roundUp(nextPos, r.header.blockSize)
			}
			if nextPos <= pos {
				yield(refRecord[H]{}, fmt.Errorf("reftable: block at %d does not advance: %w", pos, ErrTruncatedBlock))
				return
			}
			pos = nextPos
			first = false
		}
	}
}

// FindRef looks up name in the file and returns the matching record.
//
// FindRef descends the ref index when the file carries one (a
// `ref_index_position` of zero in the footer means "no index", in
// which case ref blocks are walked linearly), then linearly scans the
// resulting leaf block for an exact-name match.
//
// The boolean return distinguishes "no match" (false, nil) from
// "match" (true, nil) and "lookup error" (false, non-nil err). A
// tombstone hit is reported as no-match: tombstones shadow records
// across a stack but, considered alone, leave the name unobservable.
func (r *Reader[H]) FindRef(name string) (RefRecord[H], bool, error) {
	probe := []byte(name)
	indexRoot := readRefIndexPosition(r.file, r.header)

	leaf, firstByteOffset, err := seekToLeaf(r.file, r.header, indexRoot, probe, nil)
	if err != nil {
		return RefRecord[H]{}, false, err
	}

	blk, err := parseBlock(leaf, firstByteOffset)
	if err != nil {
		return RefRecord[H]{}, false, err
	}
	if blk.header.blockType != blockTypeRef {
		return RefRecord[H]{}, false, fmt.Errorf("reftable: expected ref block, got %q: %w", blk.header.blockType, ErrBadBlockType)
	}

	recordsEnd := blk.header.blockLen - 2 - 3*uint32(blk.header.restartCount)
	cur := firstByteOffset + 4
	var prevKey []byte
	for cur < recordsEnd {
		rec, key, n, err := decodeRefRecord[H](blk.bytes[cur:recordsEnd], prevKey, r.header.minUpdateIndex)
		if err != nil {
			return RefRecord[H]{}, false, fmt.Errorf("reftable: decode ref_record at %d: %w", cur, err)
		}
		switch cmp := bytes.Compare(key, probe); {
		case cmp == 0:
			if rec.ValueType == refValueDeletion {
				return RefRecord[H]{}, false, nil
			}
			return liftRefRecord(rec), true, nil
		case cmp > 0:
			// Records are sorted; the probe cannot appear later in the
			// block.
			return RefRecord[H]{}, false, nil
		}
		prevKey = key
		cur += uint32(n)
	}
	return RefRecord[H]{}, false, nil
}

// liftRefRecord converts the decoder's internal refRecord into the
// public [RefRecord]. Per-value-type fields are populated only for the
// types that carry them; tombstones (value_type=0) lift to the zero
// RefRecord and are filtered upstream rather than surfaced as records.
func liftRefRecord[H objfmt.Hash](r refRecord[H]) RefRecord[H] {
	out := RefRecord[H]{Name: r.Name}
	switch r.ValueType {
	case refValueSingle:
		out.Value = r.Value
	case refValuePeeled:
		out.Value = r.Value
		out.Peeled = r.Peeled
	case refValueSymref:
		out.TargetRef = r.Target
	}
	return out
}
