package server

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
)

// handleLSRefs services a v2 `ls-refs` command request. The dispatch
// loop has already consumed the `command=ls-refs` line, the cap echoes,
// and the trailing delim of the capability section; the handler reads
// the command-args section up to the terminating flush and writes the
// ref list followed by a flush. Canonical reference: `ls-refs.c::ls_refs`
// lines 161-216 and `gitprotocol-v2.adoc` §"ls-refs".
//
// The output grammar is from `gitprotocol-v2.adoc` lines 230-239:
//
//	output = *ref flush-pkt
//	ref = PKT-LINE(obj-id-or-unborn SP refname *(SP ref-attribute) LF)
//	ref-attribute = (symref | peeled)
//
// Divergence from canonical Git: the `symref-target:` attribute is only
// ever emitted for `HEAD`. Canonical Git also surfaces it for any
// `refs/...` symref (`ls-refs.c:95-109`); our [objstore.Store.IterRefs]
// resolves non-HEAD symrefs to their terminal OID before yielding
// `RefEntry`s, so a non-HEAD symref appears as a regular value ref to
// this handler. The behaviour is consistent across backends: tests will
// see `symref-target:` only on `HEAD`.
//
// An unrecognised argument surfaces as a wrapped [wire.ErrServerRefused]
// after emitting an `ERR ls-refs: unknown argument "<line>"\n` data
// pkt-line + flush. Canonical's handler `die()`s at `ls-refs.c:188`; the
// structured error lets the dispatcher's error path surface to callers
// without re-decoding the response stream.
func handleLSRefs(r *argsReader, w *pktline.Writer,
	store *objstore.Store, opts Options) error {
	_ = opts

	args, err := parseLSRefsArgs(r, w)
	if err != nil {
		return err
	}

	if err := writeLSRefsResponse(w, store, args); err != nil {
		return err
	}

	if err := w.WriteFlush(); err != nil {
		return fmt.Errorf("server: ls-refs: write flush: %w", err)
	}
	return nil
}

// parseLSRefsArgs reads pkt-lines from r until the terminating flush of
// the v2 command-args section and returns the parsed [wire.RefsArgs].
// The accepted arguments mirror `ls-refs.c::ls_refs` lines 173-189:
// `peel`, `symrefs`, `unborn`, and `ref-prefix <p>`. Any other line
// triggers the canonical "unexpected line" path: the handler emits a
// structured ERR pkt-line + flush on w and returns a wrapped
// [wire.ErrServerRefused].
//
// Mid-args EOF is reported as [io.ErrUnexpectedEOF] so callers can
// distinguish a truncated request from a clean stream close.
func parseLSRefsArgs(r *argsReader, w *pktline.Writer) (wire.RefsArgs, error) {
	var args wire.RefsArgs
	for {
		pkt, err := r.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return wire.RefsArgs{}, io.ErrUnexpectedEOF
			}
			return wire.RefsArgs{}, fmt.Errorf("server: ls-refs: read arg: %w", err)
		}
		if pkt.Kind == pktline.Flush {
			return args, nil
		}
		if pkt.Kind != pktline.Data {
			// Delim/ResponseEnd are not part of the canonical command-args
			// grammar (`ls-refs.c:173` reads only NORMAL packets). Treat
			// them as unknown arguments and refuse the request.
			return wire.RefsArgs{}, refuseUnknownArg(w, fmt.Sprintf("<pkt-kind=%d>", pkt.Kind))
		}
		line := string(trimTrailingLF(pkt.Data))
		switch line {
		case "peel":
			args.Peel = true
		case "symrefs":
			args.Symrefs = true
		case "unborn":
			args.Unborn = true
		default:
			if rest, ok := strings.CutPrefix(line, "ref-prefix "); ok {
				args.Prefixes = append(args.Prefixes, rest)
				continue
			}
			return wire.RefsArgs{}, refuseUnknownArg(w, line)
		}
	}
}

// refuseUnknownArg emits the structured ERR pkt-line + flush and returns
// a wrapped [wire.ErrServerRefused] so the dispatcher's error path
// surfaces the refusal to callers. The on-wire shape mirrors canonical
// Git's `die("unexpected line: '%s'", arg)` at `ls-refs.c:188` reframed
// as a protocol-level refusal: the client sees an `ERR` data pkt-line
// it can decode via [wire.CheckERRPacket].
func refuseUnknownArg(w *pktline.Writer, arg string) error {
	msg := fmt.Sprintf(`ls-refs: unknown argument %q`, arg)
	if err := writeERRPacket(w, msg); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", wire.ErrServerRefused, msg)
}

// writeLSRefsResponse emits the per-ref pkt-lines for the parsed args.
// HEAD is emitted first when the prefix filter admits it (`HEAD` is
// not a prefix of `refs/heads/...`, so a request that bounds the
// response with `ref-prefix refs/heads/` will not see HEAD); the
// remaining refs follow in C-locale byte order. The terminating flush
// is written by [handleLSRefs].
//
// The HEAD-emission rules mirror `ls-refs.c::send_possibly_unborn_head`
// at lines 123-145:
//
//   - Resolved HEAD (`!head.Unborn && !head.OID.IsZero()`): emit as a
//     normal ref, with `symref-target:` when [wire.RefsArgs.Symrefs]
//     is set and `head.Symref` is non-empty (a detached HEAD never
//     gets `symref-target:`).
//   - Unborn HEAD (`head.Unborn`): emit only when both
//     [wire.RefsArgs.Unborn] and [wire.RefsArgs.Symrefs] are set
//     (`ls-refs.c:135-136`). The OID slot is the literal token
//     `unborn`, not a zero OID — `ls-refs.c:91-94` writes
//     `"unborn %s"` whenever `ref->oid == NULL`.
//   - Bad/missing HEAD: skip silently (canonical's
//     `send_possibly_unborn_head:131-132` returns early on resolve
//     failure). [objstore.Store.Head] surfaces this as either a
//     non-error zero `Head` (no Symref, no OID, not Unborn) or a
//     wrapped error — the latter propagates.
//
// HEAD's peel is computed via [objstore.Store.Peel] when
// [wire.RefsArgs.Peel] is set; the loose-refs HEAD does not carry a
// packed peel hint, so the cheap `RefEntry.PeelKnown` short-circuit
// does not apply.
func writeLSRefsResponse(w *pktline.Writer, store *objstore.Store, args wire.RefsArgs) error {
	algo := store.Algo()

	head, err := store.Head()
	if err != nil {
		return fmt.Errorf("server: ls-refs: resolve HEAD: %w", err)
	}

	if err := emitHead(w, store, algo, head, args); err != nil {
		return err
	}

	refs, err := collectLSRefsRefs(store, args.Prefixes)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		line, err := formatRefLine(store, algo, ref, args)
		if err != nil {
			return err
		}
		if err := w.WritePacket([]byte(line)); err != nil {
			return fmt.Errorf("server: ls-refs: write ref %q: %w", ref.Name, err)
		}
	}
	return nil
}

// emitHead writes the HEAD pkt-line when the canonical
// `send_possibly_unborn_head` rules admit it, subject to the same
// prefix filter that applies to non-HEAD refs (`ls-refs.c:88`'s
// `ref_match` check fires before any other gate).
func emitHead(w *pktline.Writer, store *objstore.Store, algo objfmt.Algo,
	head objstore.Head, args wire.RefsArgs) error {
	if !refMatch(args.Prefixes, "HEAD") {
		return nil
	}

	switch {
	case !head.Unborn && !head.OID.IsZero():
		var b strings.Builder
		b.WriteString(head.OID.Hex(algo))
		b.WriteString(" HEAD")
		if args.Symrefs && head.Symref != "" {
			b.WriteString(" symref-target:")
			b.WriteString(head.Symref)
		}
		if args.Peel {
			peeled, ok, err := store.Peel(head.OID)
			if err != nil {
				return fmt.Errorf("server: ls-refs: peel HEAD: %w", err)
			}
			if ok && !peeled.IsZero() {
				b.WriteString(" peeled:")
				b.WriteString(peeled.Hex(algo))
			}
		}
		b.WriteByte('\n')
		if err := w.WritePacket([]byte(b.String())); err != nil {
			return fmt.Errorf("server: ls-refs: write HEAD: %w", err)
		}
	case head.Unborn && args.Unborn && args.Symrefs:
		// `ls-refs.c:91-94`: `ref->oid == NULL` ⇒ write `unborn` in
		// the OID slot. The unborn fallback only fires for symbolic
		// HEADs, and `head.Symref` is non-empty by construction here.
		line := "unborn HEAD symref-target:" + head.Symref + "\n"
		if err := w.WritePacket([]byte(line)); err != nil {
			return fmt.Errorf("server: ls-refs: write unborn HEAD: %w", err)
		}
	default:
		// Detached/zero/unborn HEAD that fails the gates above is
		// silently skipped, mirroring `send_possibly_unborn_head:131-132`
		// (bad ref) and the inverted condition at `ls-refs.c:135-136`.
	}
	return nil
}

// formatRefLine builds the per-ref data pkt-line payload for a non-HEAD
// ref. The line is `<oid> <refname>` plus optional ` peeled:<oid>`
// (canonical Git also emits `symref-target:` for non-HEAD symrefs; our
// [objstore.Store.IterRefs] resolves them to their terminal OID, so we
// never reach this code path with `RefEntry`s flagged as symbolic and
// the attribute is unreachable in practice — see the doc comment on
// [handleLSRefs]).
func formatRefLine(store *objstore.Store, algo objfmt.Algo,
	ref objstore.RefEntry, args wire.RefsArgs) (string, error) {
	var b strings.Builder
	b.WriteString(ref.OID.Hex(algo))
	b.WriteByte(' ')
	b.WriteString(ref.Name)
	if args.Peel {
		peeled, ok, err := refPeel(store, ref)
		if err != nil {
			return "", fmt.Errorf("server: ls-refs: peel %q: %w", ref.Name, err)
		}
		if ok && !peeled.IsZero() {
			b.WriteString(" peeled:")
			b.WriteString(peeled.Hex(algo))
		}
	}
	b.WriteByte('\n')
	return b.String(), nil
}

// collectLSRefsRefs drains the backend's ref iterator into a slice
// sorted by name in C-locale byte order, mirroring
// [collectV0Refs]. `gitprotocol-v2.adoc` does not mandate ordering for
// the v2 grammar, but canonical Git emits refs in the merged sorted
// order of `for_each_namespaced_ref_1` and we match that for byte
// equivalence with `git upload-pack` against the same fixture.
//
// When prefixes is non-empty, refs whose name does not begin with any
// of the listed prefixes are dropped during iteration rather than
// after the slice is built. Canonical Git collapses the same filter
// into the per-ref callback at `ls-refs.c::send_ref` line 88; we do
// it at collection time so a request with a bounded prefix set
// does not allocate a slice scaled to the entire ref namespace.
func collectLSRefsRefs(store *objstore.Store, prefixes []string) ([]objstore.RefEntry, error) {
	var refs []objstore.RefEntry
	for entry, err := range store.IterRefs() {
		if err != nil {
			return nil, fmt.Errorf("server: ls-refs: iterate refs: %w", err)
		}
		if !refMatch(prefixes, entry.Name) {
			continue
		}
		refs = append(refs, entry)
	}
	slices.SortFunc(refs, func(a, b objstore.RefEntry) int {
		return cmp.Compare(a.Name, b.Name)
	})
	return refs, nil
}

// refMatch returns true when refname is admitted by the prefix filter.
// An empty prefix list admits every ref (`ls-refs.c::ref_match`
// lines 54-67 returns 1 with no restriction). Otherwise, refname
// matches when at least one prefix is a string-prefix of refname.
func refMatch(prefixes []string, refname string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(refname, p) {
			return true
		}
	}
	return false
}
