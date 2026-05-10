package server

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
)

// writeV2Advertisement emits the discovery-time v2 capability
// advertisement: a `version 2\n` data packet, one data packet per
// advertised capability (each terminated by a single `\n`), and a
// trailing flush. The shape matches `gitprotocol-v2.adoc`
// §"Capability Advertisement" and the framing matches
// `serve.c::protocol_v2_advertise_capabilities` lines 186-216.
//
// The capability set is a strict subset of canonical Git's, in
// canonical Git's emission order from `serve.c:140-185`'s
// `capabilities[]` array — `agent`, `ls-refs`, `object-format`,
// `object-info`. The unsupported capabilities (`fetch`,
// `server-option`, `session-id`, `bundle-uri`, `promisor-remote`)
// are skipped because the emulator is read-only and
// metadata-only. Order matters: a downstream goal is byte-for-byte
// equivalence with `git upload-pack`'s advertisement against the
// same fixture, so the order tracks `serve.c` rather than any
// alternative ordering a reader might find in surrounding design
// notes.
//
//   - `agent=<value>` — opts.Agent when non-empty, otherwise
//     [wire.DefaultUserAgent]. Canonical reference:
//     `serve.c::agent_advertise` lines 25-31.
//   - `ls-refs=unborn` — the emulator implements the `unborn`
//     feature, so it is always advertised. Canonical reference:
//     `ls-refs.c::ls_refs_advertise` lines 218-223.
//   - `object-format=<algo>` — the repository's hash algorithm
//     name (`sha1` or `sha256`). Canonical reference:
//     `serve.c::object_format_advertise` lines 53-58.
//   - `object-info` — boolean, no `=value`. Canonical reference:
//     `serve.c::object_info_advertise` lines 92-101.
func writeV2Advertisement(w *pktline.Writer, store *objstore.Store, opts Options) error {
	if err := w.WritePacket([]byte("version 2\n")); err != nil {
		return fmt.Errorf("server: write v2 version line: %w", err)
	}

	agent := opts.Agent
	if agent == "" {
		agent = wire.DefaultUserAgent
	}
	caps := []string{
		"agent=" + agent,
		"ls-refs=unborn",
		"object-format=" + store.Algo().String(),
		"object-info",
	}
	for _, c := range caps {
		if err := w.WritePacket([]byte(c + "\n")); err != nil {
			return fmt.Errorf("server: write v2 capability %q: %w", c, err)
		}
	}

	if err := w.WriteFlush(); err != nil {
		return fmt.Errorf("server: write v2 advertisement flush: %w", err)
	}
	return nil
}

// writeV0Advertisement emits the discovery-time v0 reference
// advertisement: HEAD (when valid) carrying the cap list, the
// remaining refs in C-locale byte order with peeled lines folded in,
// and a trailing flush. The shape matches `gitprotocol-pack.adoc`
// §"Reference Discovery" lines 208-228 and the canonical advertise
// loop at `upload-pack.c:1416-1438` driving `write_v0_ref` at
// `upload-pack.c:1231-1275`.
//
// HEAD lands first when [objstore.Head] returns either a symbolic
// HEAD that resolves or a detached HEAD; an unborn HEAD is omitted
// because canonical Git's `mark_our_ref` filters the all-zero oid
// out of the advertisement. When HEAD is omitted and no other refs
// exist, the loop emits the canonical empty-repo placeholder
// (`upload-pack.c:1422-1428`): one `<zero-oid> capabilities^{}\0<caps>\n`
// pkt-line carrying the cap list.
//
// Capability emission order matches `write_v0_ref`'s format string
// (`upload-pack.c:1249-1262`) restricted to the discovery-only subset:
// `symref=HEAD:<target>` (only when HEAD is symbolic and resolved or
// unborn — `format_symref_info` formats from `data.symref`
// regardless of whether HEAD itself is advertised),
// `object-format=<algo>`, then `agent=<agent>`. The unsupported
// fetch-side caps (`multi_ack`, `side-band`, `thin-pack`, …) are
// skipped because the emulator does not service `fetch`.
//
// Peeled lines are emitted from [objstore.RefEntry.PeelKnown] when
// the ref backend can answer cheaply, falling through to
// [objstore.Store.Peel] otherwise. The peel is suppressed when the
// resolved peel hash is zero, matching `reference_get_peeled_oid`'s
// "no peel" return at `upload-pack.c:1268-1270`.
func writeV0Advertisement(w *pktline.Writer, store *objstore.Store, opts Options) error {
	algo := store.Algo()

	head, err := store.Head()
	if err != nil {
		return fmt.Errorf("server: resolve HEAD: %w", err)
	}

	caps := buildV0Caps(head, algo, opts)

	// Drain `IterRefs` into a sorted slice so the wire output honours
	// the C-locale byte ordering required by
	// `gitprotocol-pack.adoc:201-203`. The backend's iteration order
	// is unspecified per `internal/objstore.refBackend.IterRefs`.
	refs, err := collectV0Refs(store)
	if err != nil {
		return err
	}

	// HEAD-omitted, no-refs case: emit the canonical empty-repo
	// placeholder. `upload-pack.c:1422-1428` synthesises a
	// `capabilities^{}` ref with the null oid when
	// `data.sent_capabilities` stays zero after the ref walk.
	headValid := !head.Unborn && (head.Symref != "" || !head.OID.IsZero())
	if !headValid && len(refs) == 0 {
		var zero objfmt.Hash
		payload := zero.Hex(algo) + " capabilities^{}\x00" + caps + "\n"
		if err := w.WritePacket([]byte(payload)); err != nil {
			return fmt.Errorf("server: write v0 capabilities placeholder: %w", err)
		}
		if err := w.WriteFlush(); err != nil {
			return fmt.Errorf("server: write v0 advertisement flush: %w", err)
		}
		return nil
	}

	// Reused across every ref pkt-line and its optional peel line.
	// Appending into the slice and handing it directly to
	// [pktline.Writer.WritePacket] avoids the per-iteration
	// `strings.Builder` growth and the `[]byte(line.String())`
	// conversion the previous shape allocated; `WritePacket` copies
	// the payload into its own length-prefixed scratch
	// (`pkt-line.c:509`), so reusing this slice across calls is
	// safe.
	var line []byte

	capsEmitted := false
	if headValid {
		line = head.OID.AppendHex(line, algo)
		line = append(line, " HEAD\x00"...)
		line = append(line, caps...)
		line = append(line, '\n')
		if err := w.WritePacket(line); err != nil {
			return fmt.Errorf("server: write v0 HEAD ref: %w", err)
		}
		capsEmitted = true
	}

	for _, ref := range refs {
		line = line[:0]
		line = ref.OID.AppendHex(line, algo)
		line = append(line, ' ')
		line = append(line, ref.Name...)
		if !capsEmitted {
			line = append(line, '\x00')
			line = append(line, caps...)
			capsEmitted = true
		}
		line = append(line, '\n')
		if err := w.WritePacket(line); err != nil {
			return fmt.Errorf("server: write v0 ref %q: %w", ref.Name, err)
		}

		peeled, ok, err := refPeel(store, ref)
		if err != nil {
			return fmt.Errorf("server: peel ref %q: %w", ref.Name, err)
		}
		if ok && !peeled.IsZero() {
			line = line[:0]
			line = peeled.AppendHex(line, algo)
			line = append(line, ' ')
			line = append(line, ref.Name...)
			line = append(line, "^{}\n"...)
			if err := w.WritePacket(line); err != nil {
				return fmt.Errorf("server: write v0 peel for %q: %w", ref.Name, err)
			}
		}
	}

	if err := w.WriteFlush(); err != nil {
		return fmt.Errorf("server: write v0 advertisement flush: %w", err)
	}
	return nil
}

// buildV0Caps assembles the space-separated cap list for the v0
// advertisement. The caller frames it into the first ref's pkt-line.
// Order matches `write_v0_ref` at `upload-pack.c:1249-1262` reduced
// to the emulator's discovery-only subset.
func buildV0Caps(head objstore.Head, algo objfmt.Algo, opts Options) string {
	var b strings.Builder
	if head.Symref != "" {
		b.WriteString("symref=HEAD:")
		b.WriteString(head.Symref)
		b.WriteByte(' ')
	}
	b.WriteString("object-format=")
	b.WriteString(algo.String())
	b.WriteByte(' ')
	b.WriteString("agent=")
	if opts.Agent != "" {
		b.WriteString(opts.Agent)
	} else {
		b.WriteString(wire.DefaultUserAgent)
	}
	return b.String()
}

// collectV0Refs drains the backend's ref iterator into a slice sorted
// by name in C-locale byte order. `gitprotocol-pack.adoc:201-203`
// requires the wire output to be C-sorted; canonical Git delivers
// this through `for_each_namespaced_ref_1`'s merged sorted view.
func collectV0Refs(store *objstore.Store) ([]objstore.RefEntry, error) {
	var refs []objstore.RefEntry
	for entry, err := range store.IterRefs() {
		if err != nil {
			return nil, fmt.Errorf("server: iterate refs: %w", err)
		}
		refs = append(refs, entry)
	}
	slices.SortFunc(refs, func(a, b objstore.RefEntry) int {
		return cmp.Compare(a.Name, b.Name)
	})
	return refs, nil
}

// refPeel returns the peel of ref, preferring the cheap
// [objstore.RefEntry.PeelKnown] hint and falling through to
// [objstore.Store.Peel] when the backend cannot answer without
// reading the object body. The bool follows
// [objstore.Store.Peel]'s convention: false means "no peel known"
// (the caller skips emitting `^{}`).
func refPeel(store *objstore.Store, ref objstore.RefEntry) (objfmt.Hash, bool, error) {
	if ref.PeelKnown {
		return ref.Peeled, !ref.Peeled.IsZero(), nil
	}
	return store.Peel(ref.OID)
}
