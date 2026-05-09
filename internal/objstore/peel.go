package objstore

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// errPeelDepthExceeded is the unexported in-band signal [Store.peelChain]
// uses to tell [Store.Peel] that the chain blew past [maxPeelDepth].
// It never escapes the package: [Store.Peel] catches it, returns the
// "not peelable" shape to the caller, AND skips the cache write so a
// future bump of [maxPeelDepth] takes effect on a still-running Store.
// A genuine error (corrupt body, I/O failure) flows through unchanged
// and is also not cached.
var errPeelDepthExceeded = errors.New("objstore: peel depth exceeded")

// maxPeelDepth bounds the number of tag-of-tag dereferences [Store.Peel]
// is willing to chase. Matches canonical Git's `MAXIMUM_PEEL_REFCHAIN`
// constant in `revision.c::peel_to_object`: a chain longer than this is
// almost certainly a mis-authored cycle (Git itself never produces one),
// and the bound keeps the stack and the cache from growing without
// limit on adversarial input. Per-link cost is one loose-object read.
const maxPeelDepth = 16

// peelEntry is one cached decision in [Store.peelCache]. Both shapes
// — "this OID is a tag whose terminal target is `peeled`" and "this
// OID is not a peelable tag" — are cached so the second call on the
// same OID short-circuits the object-body read in either direction.
type peelEntry struct {
	// peeled is the dereferenced non-tag OID when ok is true. The zero
	// hash when ok is false (and intentionally so: a zero peeled hash
	// alongside ok=false is the canonical "not peelable" shape).
	peeled objfmt.Hash

	// ok is true when the input OID resolved cleanly to a non-tag
	// terminal target; false when the input was not a tag, the chain
	// exceeded [maxPeelDepth], or the dereferenced OID could not be
	// found in this store.
	ok bool
}

// Peel resolves an annotated tag to the non-tag object it points at.
//
// The returned ok is true when oid identifies an annotated tag (or a
// chain of annotated tags) whose terminal target is something other
// than another tag, in which case peeled carries that target's OID.
// ok is false when oid is not a tag, when the chain exceeds the
// 16-deep bound (matching canonical Git's `MAXIMUM_PEEL_REFCHAIN` in
// `revision.c::peel_to_object`), or when the chain dereferences to an
// OID this store cannot resolve. Per-call cost is O(1) for cached
// entries and one loose-object read per uncached chain link.
//
// The error slot reports genuine failures only: a malformed tag body
// (no `object` line, invalid hex, missing `type` line) wraps
// [ErrCorruptObject]; backend I/O errors propagate from the loose-
// object reader. A non-tag input is NOT an error — it is the normal
// "this ref does not point at a tag" shape callers depend on.
//
// # Cache
//
// Both ok=true and ok=false outcomes are memoised on the receiver
// under a [sync.Mutex]; subsequent calls on the same oid skip the
// object-body read entirely. Depth-overrun results are NOT cached so
// a future change to [maxPeelDepth] takes effect without a
// store-restart. Errors are not cached either: a transient I/O
// failure should not shadow a future successful read.
//
// # Pack-tag and alternates limitations
//
// Peel reads tag bodies through this Store's [looseObjects.Find] only.
// Two classes of OID currently surface as ok=false (canonical Git's
// "not a tag" shape) even though a more thorough resolver would peel
// them:
//
//   - Tags that live exclusively inside a `.pack` file. The
//     `internal/objfmt` public surface does not yet expose pack-object
//     body reading; revisit when a `Pack.ReadObject` (or similar)
//     method lands.
//   - Tags that live in an alternate object store reachable through
//     `objects/info/alternates`. Peel does not walk [Store.alternates]
//     yet; the upcoming object-resolver work is the natural place to
//     add the fan-out so a single contract handles loose, packed, and
//     alternate-borne tags uniformly.
//
// # `fully-peeled` short-circuit
//
// Callers that hold the source ref name AND want to skip the loose-
// object read entirely for refs that the `packed-refs` header tagged
// `fully-peeled` should consult `looseRefs.peelHint` and
// `looseRefs.refTraits` directly before calling Peel. The OID-based
// API here cannot perform that short-circuit because the trait
// belongs to the source ref, not to the value the ref carries.
func (s *Store) Peel(oid objfmt.Hash) (peeled objfmt.Hash, ok bool, err error) {
	// Fast path: a previous call on this OID has already decided.
	s.peelMu.Lock()
	if entry, hit := s.peelCache[oid]; hit {
		s.peelMu.Unlock()
		return entry.peeled, entry.ok, nil
	}
	s.peelMu.Unlock()

	peeled, ok, err = s.peelChain(oid, 0)
	if errors.Is(err, errPeelDepthExceeded) {
		// Depth overrun is reported to the caller as the "not peelable"
		// shape (matching canonical Git's `peel_to_object` returning
		// NULL), but is intentionally NOT cached: a future bump of
		// [maxPeelDepth] should make a previously-overrunning chain
		// resolvable on the next call without a Store restart.
		return objfmt.Hash{}, false, nil
	}
	if err != nil {
		// Genuine failure: do not poison the cache with a transient
		// I/O error. The next caller retries the read.
		return objfmt.Hash{}, false, err
	}

	s.peelMu.Lock()
	s.peelCache[oid] = peelEntry{peeled: peeled, ok: ok}
	s.peelMu.Unlock()
	return peeled, ok, nil
}

// peelChain dereferences oid one link at a time, bounded by depth.
//
// The recursion is expressed as an iterative loop so the stack stays
// flat regardless of how the canonical [maxPeelDepth] bound moves;
// each iteration reads the loose object, classifies it, and either
// returns the resolved OID (non-tag), advances to the dereferenced
// OID (tag-of-tag), or returns the "not peelable" shape. The cache
// in [Store.Peel] sits in front of the first iteration; intermediate
// links along a chain are NOT cached individually here because their
// caching would require a second pass over the chain (the recursive
// caller does not learn each link's terminal target until after the
// chain finishes), and the cost is amortised by the cache on the
// chain's head.
func (s *Store) peelChain(oid objfmt.Hash, depth int) (objfmt.Hash, bool, error) {
	cur := oid
	for d := depth; d < maxPeelDepth; d++ {
		typ, body, ok, err := s.readLooseTag(cur)
		if err != nil {
			return objfmt.Hash{}, false, err
		}
		if !ok {
			// Either the OID is not in the loose-object backend at all
			// (packed-only tag, unknown OID) or it is something other
			// than a tag. Both shapes are "not peelable" per the doc
			// contract.
			return objfmt.Hash{}, false, nil
		}
		if typ != objfmt.TypeTag {
			// Defence in depth: readLooseTag only returns ok=true for
			// tag types, but if that ever changes, treat anything else
			// as "not peelable" rather than mis-identify the body.
			return objfmt.Hash{}, false, nil
		}

		next, nextType, err := parseTagBody(body, s.algo)
		if err != nil {
			return objfmt.Hash{}, false, err
		}
		if nextType != "tag" {
			// Terminal: dereferenced to a commit/tree/blob.
			return next, true, nil
		}
		// Another tag. Loop to dereference at the next depth.
		cur = next
	}
	// Depth overrun. The internal sentinel routes around the
	// [Store.Peel] cache write; the public surface still sees the
	// "not peelable" shape (matching canonical Git's `peel_to_object`
	// returning NULL).
	return objfmt.Hash{}, false, errPeelDepthExceeded
}

// readLooseTag is a thin wrapper over [looseObjects.Find] that
// returns the type, the fully-buffered body, and the ok flag. The
// body is read via [io.ReadAll] because tag payloads are tiny
// (typically a few hundred bytes) and buffering up front lets the
// caller close the file handle before parsing — the parser in
// [parseTagBody] reads in two passes (first line, second line) and
// holding the file open across them serves no purpose.
//
// On ok=false the body is nil and the typ is the zero value; the
// caller treats both "no such object" and "object is not a tag" as
// "not peelable".
func (s *Store) readLooseTag(oid objfmt.Hash) (objfmt.ObjectType, []byte, bool, error) {
	typ, _, body, ok, err := s.loose.Find(oid)
	if err != nil {
		return 0, nil, false, err
	}
	if !ok {
		return 0, nil, false, nil
	}
	defer body.Close()
	if typ != objfmt.TypeTag {
		// Drain not strictly required — Close releases the zlib
		// decoder regardless — but keeping the read shape uniform
		// (always pull bytes off the body when ok) means a future
		// shift to a streaming parser does not need to special-case
		// the non-tag path.
		return typ, nil, true, nil
	}
	raw, err := io.ReadAll(body)
	if err != nil {
		return 0, nil, false, fmt.Errorf("objstore: read tag body for %s: %w: %w",
			oid.Hex(s.algo), err, ErrCorruptObject)
	}
	return typ, raw, true, nil
}

// parseTagBody extracts the dereferenced OID and type name from the
// header lines of an annotated-tag object body.
//
// The body shape per `Documentation/gitformat-signature.adoc` and
// `object-file.c::parse_tag_buffer`:
//
//	object <hex>\n
//	type <name>\n
//	tag <name>\n
//	tagger <ident>\n
//	\n
//	<message>
//
// Only the first two lines matter for peeling. Anything past `type`
// is ignored. A missing `object` or `type` line, or a malformed OID,
// surfaces as an error wrapping [ErrCorruptObject] so callers can
// distinguish "this tag is broken" from "this is not a tag".
func parseTagBody(body []byte, algo objfmt.Algo) (objfmt.Hash, string, error) {
	scanner := bufio.NewScanner(bytes.NewReader(body))
	// Tag header lines are short; the default 64 KiB buffer is
	// already generous, but pin it explicitly so a future change to
	// the bufio defaults cannot silently truncate the second line.
	scanner.Buffer(make([]byte, 0, 8*1024), 64*1024)

	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return objfmt.Hash{}, "", fmt.Errorf(
				"objstore: read tag object line: %w: %w", err, ErrCorruptObject)
		}
		return objfmt.Hash{}, "", fmt.Errorf(
			"objstore: tag body missing object line: %w", ErrCorruptObject)
	}
	objHex, ok := strings.CutPrefix(scanner.Text(), "object ")
	if !ok {
		return objfmt.Hash{}, "", fmt.Errorf(
			"objstore: tag body first line %q lacks `object ` prefix: %w",
			scanner.Text(), ErrCorruptObject)
	}
	target, err := objfmt.ParseHex(objHex, algo)
	if err != nil {
		return objfmt.Hash{}, "", fmt.Errorf(
			"objstore: tag body object hex %q: %w: %w", objHex, err, ErrCorruptObject)
	}

	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return objfmt.Hash{}, "", fmt.Errorf(
				"objstore: read tag type line: %w: %w", err, ErrCorruptObject)
		}
		return objfmt.Hash{}, "", fmt.Errorf(
			"objstore: tag body missing type line: %w", ErrCorruptObject)
	}
	typeName, ok := strings.CutPrefix(scanner.Text(), "type ")
	if !ok {
		return objfmt.Hash{}, "", fmt.Errorf(
			"objstore: tag body second line %q lacks `type ` prefix: %w",
			scanner.Text(), ErrCorruptObject)
	}
	switch typeName {
	case "commit", "tree", "blob", "tag":
		// known
	default:
		return objfmt.Hash{}, "", fmt.Errorf(
			"objstore: tag body has unknown type %q: %w", typeName, ErrCorruptObject)
	}
	return target, typeName, nil
}
