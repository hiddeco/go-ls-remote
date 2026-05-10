package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hiddeco/go-ls-remote/internal/objstore"
	"github.com/hiddeco/go-ls-remote/internal/wire"
	"github.com/hiddeco/go-ls-remote/pktline"
	"github.com/hiddeco/go-ls-remote/trace"
)

// commandPrefix is the literal that marks a v2 `command=<name>`
// pkt-line per `gitprotocol-v2.adoc` §"Command Request" and the
// canonical parser at `serve.c::parse_command` lines 254-269.
const commandPrefix = "command="

// runV2CommandLoop drives the v2 command-request loop after the
// advertisement. It mirrors `serve.c::protocol_v2_serve_loop` lines
// 356-371: read zero or more command-requests, dispatching each to the
// matching handler, and exit on the first empty-request flush or a
// clean stream close.
//
// The function returns nil for clean termination (empty request,
// stream closed between requests, handler returned without error). It
// returns a wrapped [wire.ErrServerRefused] when an unknown command is
// dispatched after emitting the structured ERR pkt-line on the wire.
// All other errors propagate from the handler or the underlying I/O.
func runV2CommandLoop(ctx context.Context, r *pktline.Reader, w *pktline.Writer,
	store *objstore.Store, opts Options) error {
	_ = ctx
	for {
		cont, err := processV2Request(r, w, store, opts)
		if err != nil {
			return err
		}
		if !cont {
			return nil
		}
	}
}

// processV2Request reads exactly one v2 command-request from r and
// dispatches it. The bool return mirrors the inverted return-value
// convention of `serve.c::process_request` (lines 280-353): true
// means "loop again", false means "session terminated cleanly".
//
// The flow follows the canonical reference:
//
//   - Read the first packet. EOF before any byte → terminate cleanly.
//     The canonical code peeks here under `PACKET_READ_GENTLE_ON_EOF`
//     and returns 1; we surface `io.EOF` from `ReadPacket` and treat
//     it the same way.
//   - A bare flush before any `command=<name>` or capability echo is
//     the empty-request terminator (`serve.c:314-321`); terminate
//     cleanly.
//   - Each NORMAL pkt is either `command=<name>` or a capability
//     echo. Unknown lines die in canonical Git; here we accept any
//     non-`command=` line and rely on the per-command handlers to
//     enforce the advertised cap set (the relaxed reading is
//     documented at the call site).
//   - A delim packet ends the capability section and the command
//     section follows. The terminating flush of the args section is
//     read by the handler, not by this function — matching the
//     canonical comment at `serve.c:323-329`.
func processV2Request(r *pktline.Reader, w *pktline.Writer,
	store *objstore.Store, opts Options) (bool, error) {
	var (
		commandName  string
		seenCmdOrCap bool
	)

	// Read the first packet under the canonical "gentle on EOF" rule:
	// a clean stream-close before any byte of this request means the
	// client terminated the session.
	first, err := r.ReadPacket()
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("server: read v2 request: %w", err)
	}

	for pkt := first; ; pkt, err = r.ReadPacket() {
		if err != nil {
			// Mid-request EOF is a protocol error: the canonical code
			// `BUG()`s after disabling `PACKET_READ_GENTLE_ON_EOF` at
			// `serve.c:298`. We propagate the wrapped error instead of
			// crashing the process.
			return false, fmt.Errorf("server: read v2 request: %w", err)
		}
		switch pkt.Kind {
		case pktline.Flush:
			// Empty request: bare flush before any command/cap →
			// terminate the session (`serve.c:314-321`).
			if !seenCmdOrCap {
				return false, nil
			}
			// A flush after at least one normal pkt but before a
			// delim is malformed under the canonical grammar
			// (`gitprotocol-v2.adoc` §"Command Request" requires a
			// delim before command-args). Dispatch anyway with an
			// empty args section so the handler can decide whether
			// to enforce its own grammar; the handler reads the
			// terminating flush of its args section, and a flush
			// here looks identical to that.
			return true, dispatchV2(r, w, store, opts, commandName)
		case pktline.Delim:
			// End of capability section; command-args follow. The
			// handler is responsible for reading them up to the
			// terminating flush (`serve.c:323-329`).
			return true, dispatchV2(r, w, store, opts, commandName)
		case pktline.Data:
			seenCmdOrCap = true
			line := trimTrailingLF(pkt.Data)
			if name, ok := strings.CutPrefix(string(line), commandPrefix); ok {
				if commandName != "" {
					return false, fmt.Errorf(
						"server: command %q requested after command %q",
						name, commandName)
				}
				commandName = name
				continue
			}
			// Capability echo. Canonical Git validates each one
			// against the advertised set (`serve.c:241-252`); for the
			// dispatch shell we accept any line without a `command=`
			// prefix and rely on the per-command handlers to enforce
			// the advertised cap set on their side.
			continue
		case pktline.ResponseEnd:
			return false, fmt.Errorf(
				"server: unexpected response-end packet in v2 request")
		default:
			return false, fmt.Errorf(
				"server: unknown pkt-line kind %d in v2 request", pkt.Kind)
		}
	}
}

// dispatchV2 routes the parsed command name to its handler. An unknown
// command emits a structured ERR pkt-line + flush so the client can
// recognise the refusal via [wire.CheckERRPacket], and surfaces a
// wrapped [wire.ErrServerRefused] so callers and tests can detect the
// protocol-level error without re-decoding the response stream.
//
// The empty `commandName` case mirrors `serve.c:343` (`die("no command
// requested")`): the request had at least one capability echo but no
// `command=` line, which is malformed.
//
// Real command dispatches (`ls-refs`, `object-info`) are wrapped in
// [trace.CommandEvent] start/end emissions via [runCommand] so a
// configured [Options.Tracer] observes the handler's lifecycle and
// duration. The unknown-command path skips the tracer: the
// caller-visible wrapped [wire.ErrServerRefused] already encodes the
// refusal and a CommandEvent for `fetch` (or any other unimplemented
// name) would advertise behaviour the emulator does not actually
// implement.
func dispatchV2(r *pktline.Reader, w *pktline.Writer, store *objstore.Store,
	opts Options, commandName string) error {
	switch commandName {
	case "":
		return errors.New("server: v2 request had no command line")
	case "ls-refs":
		return runCommand(opts, commandName, func() error {
			return handleLSRefs(r, w, store, opts)
		})
	case "object-info":
		return runCommand(opts, commandName, func() error {
			return handleObjectInfo(r, w, store, opts)
		})
	default:
		if err := writeERRPacket(w, "command not supported"); err != nil {
			return err
		}
		return fmt.Errorf("%w: command not supported", wire.ErrServerRefused)
	}
}

// runCommand wraps a v2 command-handler invocation in
// [trace.CommandEvent] start/end emissions so a configured
// [Options.Tracer] sees the dispatcher's lifecycle around `fn`. The
// `Time` on the start event records the wall-clock instant the
// handler is about to be invoked; the `Time` on the end event records
// the instant it returned, and `Duration` is `time.Since(start)`. The
// handler's return value is propagated unchanged through `Err` on the
// end event and as the function's own return value.
//
// `URL` is left empty on both events: the in-process emulator has no
// remote URL to populate it with. The contract is documented on
// [Options.Tracer] so consumers know to expect the empty value rather
// than treating it as missing data.
//
// A nil [Options.Tracer] makes both [trace.Emit] calls no-ops via the
// helper's nil-receiver check at `trace/emit.go:24`. The two
// `time.Now` calls are unconditional but cheap; the cold-path
// command-dispatch boundary is not hot enough to justify gating them.
func runCommand(opts Options, name string, fn func() error) error {
	start := time.Now()
	trace.Emit(opts.Tracer, trace.CommandEvent{
		Time:  start,
		Name:  name,
		Phase: trace.CommandStart,
	})
	err := fn()
	trace.Emit(opts.Tracer, trace.CommandEvent{
		Time:     time.Now(),
		Name:     name,
		Phase:    trace.CommandEnd,
		Duration: time.Since(start),
		Err:      err,
	})
	return err
}

// writeERRPacket emits a single `ERR <msg>\n` data pkt-line followed
// by a flush, matching the canonical ERR pkt shape at
// `pkt-line.c:509-510`. The trailing flush keeps the client's framing
// symmetric with a normal command response.
func writeERRPacket(w *pktline.Writer, msg string) error {
	if err := w.WritePacket([]byte("ERR " + msg + "\n")); err != nil {
		return fmt.Errorf("server: write ERR packet: %w", err)
	}
	if err := w.WriteFlush(); err != nil {
		return fmt.Errorf("server: write ERR flush: %w", err)
	}
	return nil
}

// trimTrailingLF strips a single LF from the end of payload, if
// present. Canonical Git emits `command=<name>\n` and capability
// echoes with a trailing LF (`packet_write_fmt(... "\n")`), but the
// wire format does not require it; we accept either form when
// matching the prefix.
func trimTrailingLF(payload []byte) []byte {
	if len(payload) > 0 && payload[len(payload)-1] == '\n' {
		return payload[:len(payload)-1]
	}
	return payload
}
