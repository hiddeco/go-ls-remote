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

	// fromPacked is true when the entry's OID came from `packed-refs`,
	// false when a loose ref file under `refs/` overrode the packed
	// entry (or registered a name absent from `packed-refs`). The
	// distinction matters for the file-wide `fully-peeled` trait: the
	// trait makes "no peel line" authoritative for entries the trait
	// applies to (the `packed-refs` body), but says nothing about a
	// loose override whose OID was never in the file.
	fromPacked bool
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
//   - sorted: the ref entries are sorted by name. The parser verifies
//     the trait on-the-fly during the body walk and clears it on the
//     first out-of-order pair, mirroring `sort_snapshot` in
//     `refs/packed-backend.c:380`. A surviving `sorted` is therefore
//     trustworthy — consumers that need ordered iteration may stream
//     straight from the file rather than buffering the full set.
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
//     space-separated tokens populate [packedTraits]. The traits header
//     is pinned to line 1 (canonical Git checks `*snapshot->buf == '#'`
//     in `refs/packed-backend.c:719` and consumes only one line). A
//     `# pack-refs with:` line later in the file is a body comment, not
//     a second traits header.
//   - Subsequent comment lines (`#` start) and blank lines are skipped.
//   - `<oid> <ref-name>` registers a ref entry. The OID width is
//     dictated by algo; a single space separates the columns.
//   - `^<oid>` immediately following a ref entry records the dereferenced
//     commit id of the previous ref. peelKnown is set to true. A
//     second `^<oid>` line for the same ref is rejected: canonical Git's
//     `next_record` (`refs/packed-backend.c:952`) consumes one peel per
//     record, and `parse_oid_hex_algop` then dies on the leading `^` of
//     the duplicate.
//
// Trailing whitespace and `\r\n` line endings are tolerated. Malformed
// lines (wrong hex length, no separator, `^` with no preceding ref,
// duplicate peel) surface as an error wrapping [ErrCorruptObject], with
// the offending line number and text included for diagnostics.
func parsePackedRefs(r io.Reader, algo objfmt.Algo) (packedRefs, error) {
	out := packedRefs{refs: make(map[string]packedEntry)}

	scanner := bufio.NewScanner(r)
	// Allow generously large lines; canonical Git imposes no fixed
	// limit, and a 1 MiB ceiling covers any realistic ref name while
	// still bounding pathological input.
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	hexLen := algo.Size() * 2

	var (
		lineNo  int
		seenAny bool   // true once any non-blank line has been observed
		lastRef string // most recently registered ref; "" before the first ref
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
			// Note: this is *more permissive* than canonical Git. The
			// record iterator at `refs/packed-backend.c:390` (and the
			// `iter->eof - p < hexsz + 2` length check at line 920)
			// dies on any line shorter than `<oid> <name>`, which
			// includes blank lines. Canonical writers never emit blank
			// lines so the divergence is invisible in practice; if v0
			// gains write support this should tighten to canonical
			// strictness so writer bugs surface as test-time failures.
			continue
		}

		if line[0] == '#' {
			// Only the first non-blank line is eligible for the
			// traits header. Canonical Git checks `*snapshot->buf ==
			// '#'` at the very start of the file
			// (`refs/packed-backend.c:719`); a `#` line anywhere
			// later is a body comment. A leading blank line is a
			// Go-side lenience canonical Git's writer never produces.
			if !seenAny {
				out.traits = parsePackedRefTraits(line)
			}
			seenAny = true
			continue
		}
		seenAny = true

		if line[0] == '^' {
			if lastRef == "" {
				return packedRefs{}, fmt.Errorf(
					"objstore: packed-refs line %d: peel without preceding ref %q: %w",
					lineNo, raw, ErrCorruptObject)
			}
			entry := out.refs[lastRef]
			if entry.peelKnown {
				return packedRefs{}, fmt.Errorf(
					"objstore: packed-refs line %d: duplicate peel for %q: %q: %w",
					lineNo, lastRef, raw, ErrCorruptObject)
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
		//
		// Note: this is *stricter* than canonical Git, which accepts any
		// `isspace` byte between the OID and the refname
		// (`refs/packed-backend.c:922` checks `!isspace(*p++)`).
		// Canonical Git's writer emits exactly one ASCII space, so the
		// divergence is invisible against canonical-Git-produced files.
		// A future need to read non-canonical packed-refs would have to
		// loosen this gate to match canonical's `isspace` rule.
		if !ok || name == "" || name[0] == ' ' || name[0] == '\t' || name[0] == '^' {
			return packedRefs{}, fmt.Errorf(
				"objstore: packed-refs line %d: missing separator %q: %w",
				lineNo, raw, ErrCorruptObject)
		}
		// Validate the refname against canonical Git's format rules
		// (`refs.c:320` `check_refname_format` with
		// `REFNAME_ALLOW_ONELEVEL`, the same flag the iterator uses at
		// `refs/packed-backend.c:938`). Canonical Git would mark a
		// non-conforming entry `REF_BAD_NAME | REF_ISBROKEN` and zero
		// the OID; for a read-only library that surfaces refs to
		// downstream serializers, refusing the file at parse time is
		// safer — embedded NUL bytes or control characters in a
		// "valid" ref would be invisible to the caller otherwise.
		if !checkRefnameFormat(name) {
			return packedRefs{}, fmt.Errorf(
				"objstore: packed-refs line %d: invalid refname %q: %w",
				lineNo, name, ErrCorruptObject)
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
		// Verify the `sorted` trait on-the-fly. Canonical Git's
		// `sort_snapshot` (`refs/packed-backend.c:380`) walks the
		// records during iteration and clears the trait on the first
		// out-of-order pair; a corrupt or hostile file claiming
		// `sorted` must not be allowed to mislead downstream
		// short-circuits that rely on the order. Equal names are not a
		// violation — canonical Git treats them as in-order — so the
		// comparison is strictly less-than.
		if out.traits.sorted && lastRef != "" && name < lastRef {
			out.traits.sorted = false
		}
		out.refs[name] = packedEntry{oid: oid, fromPacked: true}
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
