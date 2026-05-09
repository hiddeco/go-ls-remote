package objstore

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// packedEntry is a single record in the in-memory packed-refs map. The
// peelKnown bit distinguishes "this ref has no peel because it is not
// peelable" from "this ref might be peelable but the file did not record
// the peel"; consumers must consult [packedTraits.fullyPeeled] to decide
// whether the absence of a `^peel` line is authoritative.
//
// The same shape is reused for loose refs that override packed entries —
// loose ref files never carry peel information, so peelKnown is always
// false in that case and peeled is the zero hash.
type packedEntry struct {
	// oid is the resolved object id of the ref's terminal target.
	oid objfmt.Hash

	// peeled is the dereferenced commit id when the entry was followed
	// by a `^<oid>` line in `packed-refs`. The zero hash otherwise.
	peeled objfmt.Hash

	// peelKnown is true when a `^<oid>` line followed this ref in
	// `packed-refs`. Loose ref overrides set it to false because loose
	// ref files do not encode peel state.
	peelKnown bool
}

// packedTraits captures the optional traits advertised by the
// `# pack-refs with: <traits>` header line at the top of `packed-refs`.
// The three documented traits — `peeled`, `fully-peeled`, `sorted` —
// affect how downstream code may short-circuit ref operations:
//
//   - peeled: every peelable ref under `refs/tags/` carries its
//     `^<oid>` line in the file. Consumers may skip on-disk peel
//     resolution for tags they find pre-peeled.
//   - fullyPeeled: every peelable ref (anywhere, not just tags) carries
//     its `^<oid>` line. Combined with [packedEntry.peelKnown], a
//     missing peel becomes definitive: the ref is non-peelable.
//   - sorted: the ref entries are sorted by name. Consumers that need
//     ordered iteration may stream straight from the file rather than
//     buffering the full set.
//
// Unknown trait tokens in the header are tolerated and ignored so a
// future trait the parser has not been taught about does not blow up
// the file. The known set follows
// `refs/packed-backend.c::parse_packed_ref_traits` in canonical Git.
type packedTraits struct {
	peeled      bool
	fullyPeeled bool
	sorted      bool
}

// packedRefs holds the parsed `packed-refs` file contents: the per-ref
// map keyed by fully-qualified ref name, plus the header-derived
// [packedTraits]. The zero value is the canonical "empty packed-refs"
// shape returned when the file is absent.
type packedRefs struct {
	refs   map[string]packedEntry
	traits packedTraits
}

// parsePackedRefs consumes a `packed-refs` stream and returns the
// parsed [packedRefs]. The grammar follows
// `refs/packed-backend.c::parse_packed_refs_line`:
//
//   - Optional first line `# pack-refs with: <traits>` whose
//     space-separated tokens populate [packedTraits]. Unknown tokens are
//     ignored.
//   - Subsequent comment lines (`#` start) and blank lines are skipped.
//   - `<oid> <ref-name>` registers a ref entry. The OID width is
//     dictated by algo; a single space separates the columns.
//   - `^<oid>` immediately following a ref entry records the dereferenced
//     commit id of the previous ref. peelKnown is set to true.
//
// Trailing whitespace and `\r\n` line endings are tolerated. Malformed
// lines (wrong hex length, no separator, `^` with no preceding ref)
// surface as an error wrapping [ErrCorruptObject], with the offending
// line number and text included for diagnostics.
func parsePackedRefs(r io.Reader, algo objfmt.Algo) (packedRefs, error) {
	out := packedRefs{refs: make(map[string]packedEntry)}

	scanner := bufio.NewScanner(r)
	// Allow generously large lines; canonical Git imposes no fixed
	// limit, and a 1 MiB ceiling covers any realistic ref name while
	// still bounding pathological input.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	hexLen := algo.Size() * 2

	var (
		lineNo     int
		headerSeen bool
		lastRef    string // most recently registered ref; "" before the first ref
	)
	for scanner.Scan() {
		lineNo++
		raw := strings.TrimRight(scanner.Text(), "\r\n")
		// Tolerate trailing whitespace per canonical Git's lenient
		// reader: stripping right-side whitespace only avoids eating
		// significant leading spaces in the (admittedly unlikely)
		// future where ref names grow indentation rules.
		line := strings.TrimRight(raw, " \t")
		if line == "" {
			continue
		}

		if line[0] == '#' {
			if !headerSeen {
				out.traits = parsePackedRefTraits(line)
				headerSeen = true
				continue
			}
			// Body comments are tolerated and skipped; canonical Git
			// only writes a header comment, but a manually-edited file
			// might carry annotations.
			continue
		}

		if line[0] == '^' {
			if lastRef == "" {
				return packedRefs{}, fmt.Errorf(
					"objstore: packed-refs line %d: peel without preceding ref %q: %w",
					lineNo, raw, ErrCorruptObject)
			}
			peelHex := line[1:]
			if len(peelHex) != hexLen {
				return packedRefs{}, fmt.Errorf(
					"objstore: packed-refs line %d: peel hex length %d, want %d %q: %w",
					lineNo, len(peelHex), hexLen, raw, ErrCorruptObject)
			}
			peeled, err := objfmt.ParseHex(peelHex, algo)
			if err != nil {
				return packedRefs{}, fmt.Errorf(
					"objstore: packed-refs line %d: parse peel %q: %w",
					lineNo, raw, ErrCorruptObject)
			}
			entry := out.refs[lastRef]
			entry.peeled = peeled
			entry.peelKnown = true
			out.refs[lastRef] = entry
			continue
		}

		oidHex, name, ok := strings.Cut(line, " ")
		// Reject double-spaces, leading whitespace inside the name, and
		// `^`-prefixed names so a corrupt file cannot stamp a stray peel
		// onto the wrong ref. Canonical `git update-ref` writes a single
		// ASCII space and no other whitespace.
		if !ok || name == "" || name[0] == ' ' || name[0] == '\t' || name[0] == '^' {
			return packedRefs{}, fmt.Errorf(
				"objstore: packed-refs line %d: missing separator %q: %w",
				lineNo, raw, ErrCorruptObject)
		}
		if len(oidHex) != hexLen {
			return packedRefs{}, fmt.Errorf(
				"objstore: packed-refs line %d: oid hex length %d, want %d %q: %w",
				lineNo, len(oidHex), hexLen, raw, ErrCorruptObject)
		}
		oid, err := objfmt.ParseHex(oidHex, algo)
		if err != nil {
			return packedRefs{}, fmt.Errorf(
				"objstore: packed-refs line %d: parse oid %q: %w",
				lineNo, raw, ErrCorruptObject)
		}
		out.refs[name] = packedEntry{oid: oid}
		lastRef = name
	}
	if err := scanner.Err(); err != nil {
		return packedRefs{}, fmt.Errorf("objstore: read packed-refs: %w", err)
	}
	return out, nil
}

// parsePackedRefTraits reads the `# pack-refs with: <traits>` header
// line and returns the parsed [packedTraits]. The header is tolerant:
// whitespace around the `with:` separator is ignored, unknown tokens
// are silently dropped, and a missing or malformed `with:` clause
// yields the zero value rather than an error. Canonical reference:
// `refs/packed-backend.c::parse_packed_ref_traits`.
func parsePackedRefTraits(line string) packedTraits {
	// Strip the leading `#` and any whitespace, then look for the
	// `pack-refs with:` lead-in. Anything else — a plain comment,
	// a malformed header — leaves the traits at their zero value.
	body := strings.TrimLeft(line, "#")
	body = strings.TrimSpace(body)
	rest, ok := strings.CutPrefix(body, "pack-refs with:")
	if !ok {
		return packedTraits{}
	}

	var traits packedTraits
	for tok := range strings.FieldsSeq(rest) {
		switch tok {
		case "peeled":
			traits.peeled = true
		case "fully-peeled":
			traits.fullyPeeled = true
		case "sorted":
			traits.sorted = true
		default:
			// Unknown trait — tolerated. A future trait the parser has
			// not been taught about must not blow up the file.
		}
	}
	return traits
}
