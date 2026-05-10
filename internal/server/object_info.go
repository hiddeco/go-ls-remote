package server

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
)

// handleObjectInfo services a v2 `object-info` command request. The
// dispatch loop has already consumed the `command=object-info` line,
// the cap echoes, and the trailing delim of the capability section;
// the handler reads the command-args section up to the terminating
// flush and writes the per-OID metadata response followed by a flush.
// Canonical reference: `protocol-caps.c::cap_object_info` lines 79-114
// and `gitprotocol-v2.adoc` §"object-info" lines 556-585.
//
// The output grammar is:
//
//	output   = info flush-pkt
//	info     = PKT-LINE(attrs LF) *PKT-LINE(obj-info LF)
//	attrs    = attr | attrs SP attrs
//	attr     = "size"
//	obj-info = obj-id SP obj-size
//
// Argument parsing follows `protocol-caps.c::cap_object_info` lines
// 87-99: each pkt-line in the command-args section is one of `size`,
// `oid <hex>`, or an unrecognised line. An unrecognised line emits a
// single `ERR object-info: unexpected line: '<line>'\n` data pkt-line
// (no flush) and the parser CONTINUES — this differs from
// `ls-refs.c:188`'s `die()` behaviour. Mid-args EOF surfaces as
// `io.ErrUnexpectedEOF` so callers can distinguish a truncated request
// from a clean stream close.
//
// Response emission follows `protocol-caps.c::send_info` lines 37-77:
//
//   - An empty OID list yields no attrs line and no obj-info lines.
//     The handler emits just the trailing flush (`send_info:44-45`).
//   - With OIDs and `size`, the attrs line `size\n` precedes the
//     obj-info lines (`send_info:47-48`).
//   - With OIDs and no `size`, no attrs line is emitted; per-OID lines
//     are just `<oid>\n` with no size column (`send_info:65` gates the
//     entire size-column block on `info->size`).
//   - A malformed OID hex on any input line emits `ERR object-info:
//     protocol error, expected to get oid, not '<hex>'\n` MID-STREAM
//     and the iteration CONTINUES; the bad OID does not contribute a
//     row (`send_info:55-61`).
//
// # Divergence from canonical Git on missing objects
//
// Canonical Git does not omit missing OIDs from the response: per
// `protocol-caps.c:66-67`, it emits `<oid> ` (literal trailing space,
// no size value) when `odb_read_object_info` fails. We follow
// canonical for byte equivalence — the `<oid> \n` empty-size form
// lands on the wire when `size` is set, and just `<oid>\n` (no
// trailing space) when `size` is absent. The wire decoder
// ([wire.DecodeObjectInfo]) drops these rows so callers see
// "missing" semantics, but the bytes match canonical exactly.
//
// # Divergence from canonical Git on corruption
//
// Canonical Git conflates `os.ErrNotExist`-shaped misses with corrupt
// or unresolvable objects: both surface as `<oid> ` in `send_info`. Our
// scope distinguishes the two. A clean miss takes the empty-size form
// described above; a corrupt or unresolvable object — anything wrapping
// [objstore.ErrCorruptObject] or any other non-`os.ErrNotExist` store
// error — is fatal. The handler emits a structured `ERR objstore:
// object corrupt or unresolvable: <wrapped>\n` data pkt-line followed
// by a flush, then returns a wrapped [wire.ErrServerRefused] so the
// dispatcher terminates the v2 session.
func handleObjectInfo[H objfmt.Hash](r *argsReader, w *pktline.Writer,
	store *objstore.Store[H], opts Options) error {
	_ = opts

	args, oids, err := parseObjectInfoArgs(r, w)
	if err != nil {
		return err
	}

	if err := writeObjectInfoResponse(w, store, args, oids); err != nil {
		return err
	}

	if err := w.WriteFlush(); err != nil {
		return fmt.Errorf("server: object-info: write flush: %w", err)
	}
	return nil
}

// parseObjectInfoArgs reads pkt-lines from r until the terminating
// flush of the v2 command-args section and returns the parsed
// [wire.ObjectInfoArgs] together with the caller-ordered list of
// requested OID hex strings. The accepted arguments mirror
// `protocol-caps.c::cap_object_info` lines 87-99: `size`, `oid <hex>`,
// or an unrecognised line.
//
// An unrecognised line emits an inline ERR data pkt-line on w via
// [writeObjectInfoErr] and the parser continues. This differs from
// `parseLSRefsArgs`, which refuses the request on the first unknown
// argument: the divergence is intentional and matches canonical Git's
// per-handler choice (`protocol-caps.c:96-99` continues; `ls-refs.c:188`
// die()s).
//
// Mid-args EOF is reported as [io.ErrUnexpectedEOF] so callers can
// distinguish a truncated request from a clean stream close.
func parseObjectInfoArgs(r *argsReader, w *pktline.Writer) (wire.ObjectInfoArgs, []string, error) {
	var (
		args wire.ObjectInfoArgs
		oids []string
	)
	for {
		pkt, err := r.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return wire.ObjectInfoArgs{}, nil, io.ErrUnexpectedEOF
			}
			return wire.ObjectInfoArgs{}, nil, fmt.Errorf("server: object-info: read arg: %w", err)
		}
		if pkt.Kind == pktline.Flush {
			return args, oids, nil
		}
		if pkt.Kind != pktline.Data {
			// Delim/ResponseEnd are not part of the canonical command-args
			// grammar (`protocol-caps.c:87` reads only NORMAL packets);
			// surface them as the same "unexpected line" shape canonical
			// uses, with a synthetic placeholder for the line text.
			if err := writeObjectInfoErr(w,
				fmt.Sprintf("object-info: unexpected line: '<pkt-kind=%d>'", pkt.Kind)); err != nil {
				return wire.ObjectInfoArgs{}, nil, err
			}
			continue
		}
		line := string(trimTrailingLF(pkt.Data))
		switch line {
		case "size":
			args.Size = true
		default:
			if rest, ok := strings.CutPrefix(line, "oid "); ok {
				oids = append(oids, rest)
				continue
			}
			// Canonical: emit one ERR pkt-line per unknown line and
			// CONTINUE. No flush here — the response body that follows
			// terminates the section with a single trailing flush.
			if err := writeObjectInfoErr(w,
				fmt.Sprintf("object-info: unexpected line: '%s'", line)); err != nil {
				return wire.ObjectInfoArgs{}, nil, err
			}
		}
	}
}

// writeObjectInfoResponse emits the per-OID metadata section for the
// parsed args. The trailing flush is written by [handleObjectInfo].
//
// The branch structure mirrors `protocol-caps.c::send_info` exactly:
//
//   - Empty OID list → no output (the caller's flush is the response's
//     sole pkt-line). Canonical: `send_info:44-45`.
//   - Non-empty OID list with `size` → emit the `size\n` attrs line
//     first (`send_info:47-48`), then one obj-info line per OID.
//   - Non-empty OID list without `size` → no attrs line, one
//     `<oid>\n` line per OID (`send_info:65` gates the size column).
//
// Per-OID emission delegates to [emitObjectInfoLine], which is also
// where the parse-failure / miss / corrupt-object branches live.
func writeObjectInfoResponse[H objfmt.Hash](w *pktline.Writer, store *objstore.Store[H],
	args wire.ObjectInfoArgs, oids []string) error {
	if len(oids) == 0 {
		// `send_info:44-45`: no OIDs ⇒ no attrs line and no obj-info
		// lines, even when `size` was set.
		return nil
	}
	if args.Size {
		if err := w.WritePacket([]byte("size\n")); err != nil {
			return fmt.Errorf("server: object-info: write attrs: %w", err)
		}
	}
	for _, oid := range oids {
		if err := emitObjectInfoLine(w, store, oid, args.Size); err != nil {
			return err
		}
	}
	return nil
}

// emitObjectInfoLine writes a single per-OID line for the response, or
// emits the inline ERR for an OID-parse failure. The wantSize gate
// controls whether the size column (or, on a miss, the trailing-space
// empty-size column) is appended.
//
// Per-OID outcomes follow canonical's `send_info`:
//
//   - Hex-parse failure: emit `ERR object-info: protocol error,
//     expected to get oid, not '<hex>'\n` and return nil; iteration
//     continues from the caller (`send_info:55-61`).
//   - Hit with `wantSize`: emit `<oid> <size>\n`
//     (`send_info:69`).
//   - Hit without `wantSize`: emit `<oid>\n`
//     (`send_info:63` only).
//   - Miss (`os.ErrNotExist`) with `wantSize`: emit `<oid> \n` —
//     trailing space, no size value (`send_info:66-67`). The wire
//     decoder drops these so callers see "missing" semantics.
//   - Miss without `wantSize`: emit `<oid>\n` (`send_info:63` only;
//     the `if (info->size)` block does not fire so no trailing space).
//   - [objstore.ErrCorruptObject] or any other non-miss error:
//     diverges from canonical. Emit `ERR objstore: object corrupt or
//     unresolvable: <wrapped>\n` + flush, return wrapping
//     [wire.ErrServerRefused] so the dispatcher terminates the
//     session.
func emitObjectInfoLine[H objfmt.Hash](w *pktline.Writer, store *objstore.Store[H],
	oidHex string, wantSize bool) error {
	hash, err := objfmt.ParseHexAs[H](oidHex)
	if err != nil {
		// `protocol-caps.c:55-61`: malformed hex ⇒ inline ERR + continue.
		// The bad hex is echoed verbatim into the message so a client
		// trace shows the offending bytes.
		return writeObjectInfoErr(w, fmt.Sprintf(
			"object-info: protocol error, expected to get oid, not '%s'", oidHex))
	}

	info, err := store.ObjectInfo(hash)
	switch {
	case err == nil:
		// Hit. Build the line per canonical: `<oid>` then optional
		// ` <size>` when wantSize is set.
		var b strings.Builder
		b.WriteString(oidHex)
		if wantSize {
			fmt.Fprintf(&b, " %d", info.Size)
		}
		b.WriteByte('\n')
		if err := w.WritePacket([]byte(b.String())); err != nil {
			return fmt.Errorf("server: object-info: write %s: %w", oidHex, err)
		}
		return nil
	case errors.Is(err, os.ErrNotExist):
		// Empty-size form: `<oid> \n` when wantSize, just `<oid>\n`
		// otherwise. Per `send_info:63-71`.
		var b strings.Builder
		b.WriteString(oidHex)
		if wantSize {
			b.WriteByte(' ')
		}
		b.WriteByte('\n')
		if err := w.WritePacket([]byte(b.String())); err != nil {
			return fmt.Errorf("server: object-info: write missing %s: %w", oidHex, err)
		}
		return nil
	default:
		// Corruption or any other store error: fatal. Emit a structured
		// ERR pkt-line carrying the wrapped error message, write the
		// trailing flush, and surface a wrapped [wire.ErrServerRefused]
		// so the dispatcher terminates the session.
		msg := fmt.Sprintf("objstore: object corrupt or unresolvable: %s", err.Error())
		if werr := writeObjectInfoErr(w, msg); werr != nil {
			return werr
		}
		if werr := w.WriteFlush(); werr != nil {
			return fmt.Errorf("server: object-info: write fatal flush: %w", werr)
		}
		return fmt.Errorf("%w: %s", wire.ErrServerRefused, msg)
	}
}

// writeObjectInfoErr emits a single `ERR <msg>\n` data pkt-line with no
// trailing flush, mirroring canonical's `packet_writer_error`
// (`pkt-line.c:693-701`). It differs from [writeERRPacket] in
// `command_loop.go`: that helper writes ERR + flush as a complete
// response terminator for the unknown-command path, whereas this
// helper emits a single error pkt-line that is intended to interleave
// with subsequent response pkt-lines.
func writeObjectInfoErr(w *pktline.Writer, msg string) error {
	if err := w.WritePacket([]byte("ERR " + msg + "\n")); err != nil {
		return fmt.Errorf("server: object-info: write ERR: %w", err)
	}
	return nil
}
