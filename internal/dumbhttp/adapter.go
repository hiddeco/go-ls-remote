// Package dumbhttp synthesises a v0-shaped pkt-line stream from the
// `info/refs` body of a Git "dumb" HTTP server. The body is the
// UNIX-formatted text file described in [gitprotocol-http.adoc
// lines 158-200]: one record per ref, fields separated by HTAB,
// terminated by LF, with annotated tags peeled onto a second line
// suffixed `^{}`.
//
// The adapter exists so that the wire layer's v0/v1 advertisement
// parser can consume a uniform stream regardless of whether the
// server spoke smart or dumb HTTP. The synthesised stream has the
// shape canonical Git's [connect.c::discover_version lines 143-181]
// expects from a v0 advertisement, except that no capability text is
// emitted on the first ref line — [gitprotocol-pack.adoc
// lines 219-228] allow an empty cap list, and any cap we'd synthesise
// would be either fetch-only or misleading.
//
// # Streaming model
//
// [NewAdapter] returns a [pktline.Reader] backed by a custom
// [io.Reader] that reads the dumb body line-by-line and emits one
// pkt-line at a time on demand. The implementation is
// single-threaded so that read errors from the underlying transport
// surface verbatim through the [io.Reader] contract; an [io.Pipe]
// design would have required a goroutine that the consumer cannot
// signal to stop, and an eager [bytes.Buffer] design would have
// silently swallowed mid-stream errors.
//
// # Hash format
//
// The adapter is hash-format agnostic. It does not validate that an
// object id is exactly 40 (SHA-1) or 64 (SHA-256) hex characters —
// whatever the server emitted is forwarded to the wire layer, which
// owns validation. The empty-repo placeholder uses 40 zero hex
// characters (SHA-1) because the dumb HTTP body carries no
// `object-format=` capability and the wire layer treats v0 as SHA-1
// unless `object-format=sha256` is explicitly advertised.
//
// [gitprotocol-http.adoc lines 158-200]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-http.adoc?plain=1#L158-L200
// [connect.c::discover_version lines 143-181]: https://github.com/git/git/blob/v2.54.0/connect.c#L143-L181
// [gitprotocol-pack.adoc lines 219-228]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-pack.adoc?plain=1#L219-L228
package dumbhttp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hiddeco/go-ls-remote/pktline"
)

// ErrMalformedRefLine is returned (wrapped) when the dumb HTTP
// `info/refs` body contains a line that is neither blank, a `#`
// comment, nor a well-formed `<oid> <refname>` record. Callers match
// against it with [errors.Is].
var ErrMalformedRefLine = errors.New("dumbhttp: malformed ref line")

// ErrRefLineTooLarge is returned (wrapped) when a ref record is too
// long to fit into a single pkt-line under [pktline.MaxPayload].
// Refusing the line at synthesis time avoids wrapping the 4-byte
// hex length prefix, which would misalign every byte downstream
// before the wire layer's own size check could fire. Callers match
// against it with [errors.Is].
var ErrRefLineTooLarge = errors.New("dumbhttp: ref line exceeds maximum pkt-line payload")

// maxRefLineBytes caps the synthesised payload size at one byte less
// than the worst-case suffix the adapter appends. The first ref
// pkt-line carries a `\x00\n` (2-byte) NUL/cap-list trailer; later
// refs carry only `\n`. Reserving 2 bytes covers both shapes without
// branching, leaving `pktline.MaxPayload - 2` (= 65514) bytes for
// `<oid> <SP> <refname>`.
const maxRefLineBytes = pktline.MaxPayload - 2

// emptyRepoPlaceholder is the synthetic pkt-line payload used when
// the dumb body carries zero refs. Its shape mirrors the
// `<zero-oid> capabilities^{}` line canonical Git's
// [connect.c::process_dummy_ref lines 260-274] recognises and
// skips, so the wire layer parses an empty advertisement without
// error. The leading 40 zeros encode SHA-1 — see the package doc
// for why we do not synthesise a SHA-256 variant.
//
// [connect.c::process_dummy_ref lines 260-274]: https://github.com/git/git/blob/v2.54.0/connect.c#L260-L274
const emptyRepoPlaceholder = "0000000000000000000000000000000000000000 capabilities^{}\x00\n"

// pktLengthPrefix is the size of a pkt-line length prefix in bytes.
const pktLengthPrefix = 4

// hexDigits is the alphabet used by [encodePktLine] when writing the
// 4-byte ASCII hex length prefix.
const hexDigits = "0123456789abcdef"

// NewAdapter returns a [pktline.Reader] whose source is a synthetic
// v0-shaped pkt-line stream produced from the dumb HTTP `info/refs`
// body in src. The first ref pkt-line carries a NUL marker with an
// empty capability list (`<oid> <refname>\x00\n`); subsequent ref
// lines, including peel annotations, are emitted as plain
// `<oid> <refname>\n` data packets; a flush packet terminates the
// stream. An empty body produces the empty-repo placeholder followed
// by a flush.
//
// opts are forwarded to the internal [pktline.NewReader] call so a
// caller wiring a tracer (typically via
// [pktline.WithReaderTracerURL]) sees [trace.PacketEvent] values for
// the synthesised stream rather than the raw HTTP body. That matches
// what the wire layer would observe had the server spoken smart-v0
// directly: tracer events reflect the synthetic v0 shape this adapter
// produces, not the dumb body's `<oid> HTAB <refname>` text.
func NewAdapter(src io.Reader, opts ...pktline.ReaderOption) *pktline.Reader {
	return pktline.NewReader(newSynth(src), opts...)
}

// state names the adapter's progress through the synthesised stream.
type state uint8

const (
	// stateStart is the initial state: the next [synth.Read] call
	// peeks the first ref line (or detects an empty body) and emits
	// either the empty-repo placeholder or the first NUL-marked ref
	// pkt-line.
	stateStart state = iota

	// stateStreaming covers every ref pkt-line after the first.
	stateStreaming

	// stateFlush waits to emit the terminating flush packet.
	stateFlush

	// stateDone reports clean end-of-stream; subsequent Read calls
	// return [io.EOF].
	stateDone
)

// synth implements [io.Reader] by emitting a v0-shaped pkt-line
// stream synthesised from a dumb HTTP `info/refs` body. It buffers
// at most one pkt-line at a time in pending so that a sticky read
// error from src — captured via firstErr — can be surfaced after
// the bytes already buffered have been delivered.
type synth struct {
	src      *bufio.Scanner
	pending  []byte
	st       state
	firstErr error
}

// newSynth constructs a [synth] over src. The 64 KiB default
// scanner buffer easily accommodates well-formed refnames, which
// are bounded by `MAX_PATH` on disk and in practice well under
// 1 KiB; we do not bump it.
func newSynth(src io.Reader) *synth {
	s := bufio.NewScanner(src)
	return &synth{src: s, st: stateStart}
}

// Read implements [io.Reader]. It produces the next bytes of the
// synthesised pkt-line stream, advancing the state machine across
// pkt-line boundaries. A malformed ref line is reported as a wrapped
// [ErrMalformedRefLine]; underlying read errors propagate verbatim
// after any already-buffered bytes are returned.
func (s *synth) Read(p []byte) (int, error) {
	for {
		if len(s.pending) > 0 {
			n := copy(p, s.pending)
			s.pending = s.pending[n:]
			return n, nil
		}

		switch s.st {
		case stateStart:
			if err := s.fillFirst(); err != nil {
				return 0, err
			}
		case stateStreaming:
			if err := s.fillNext(); err != nil {
				return 0, err
			}
		case stateFlush:
			s.pending = []byte("0000")
			s.st = stateDone
		case stateDone:
			return 0, io.EOF
		}
	}
}

// fillFirst reads ahead until either a real ref line or end-of-body
// is reached, then primes pending with either the first NUL-marked
// ref pkt-line or the empty-repo placeholder pkt-line.
func (s *synth) fillFirst() error {
	line, ok, err := s.nextRefLine()
	if err != nil {
		return err
	}
	if !ok {
		// No ref records at all — synthesise the empty-repo placeholder
		// so the wire layer parses zero refs without error.
		s.pending = encodePktLine(emptyRepoPlaceholder)
		s.st = stateFlush
		return nil
	}
	oid, name, err := splitRefLine(line)
	if err != nil {
		return err
	}
	// `<oid> <refname>\x00\n` — NUL marks the (empty) capability list
	// per [gitprotocol-pack.adoc lines 219-228].
	//
	// [gitprotocol-pack.adoc lines 219-228]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-pack.adoc?plain=1#L219-L228
	payload := oid + " " + name + "\x00\n"
	s.pending = encodePktLine(payload)
	s.st = stateStreaming
	return nil
}

// fillNext primes pending with the next plain ref pkt-line, or
// switches to [stateFlush] when the body is exhausted.
func (s *synth) fillNext() error {
	line, ok, err := s.nextRefLine()
	if err != nil {
		return err
	}
	if !ok {
		s.st = stateFlush
		return nil
	}
	oid, name, err := splitRefLine(line)
	if err != nil {
		return err
	}
	// Subsequent refs (including peel annotations) are emitted plain;
	// they correspond to `other-tip` and `other-peeled` productions in
	// [gitprotocol-pack.adoc lines 219-228].
	//
	// [gitprotocol-pack.adoc lines 219-228]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-pack.adoc?plain=1#L219-L228
	payload := oid + " " + name + "\n"
	s.pending = encodePktLine(payload)
	return nil
}

// nextRefLine pulls the next non-blank, non-comment line from the
// scanner. It returns ok=false when the input is exhausted with no
// further ref records, and surfaces a sticky scanner error if the
// underlying reader failed.
func (s *synth) nextRefLine() (string, bool, error) {
	if s.firstErr != nil {
		return "", false, s.firstErr
	}
	for s.src.Scan() {
		// Trim canonical whitespace per [gitprotocol-http.adoc
		// lines 158-200]: lines end with LF (already consumed by the
		// scanner), but we also accept CR-trailed lines (CRLF servers).
		//
		// [gitprotocol-http.adoc lines 158-200]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-http.adoc?plain=1#L158-L200
		raw := strings.TrimRight(s.src.Text(), " \t\r")
		if raw == "" {
			continue
		}
		if raw[0] == '#' {
			continue
		}
		return raw, true, nil
	}
	if err := s.src.Err(); err != nil {
		// `bufio.Scanner`'s default 64 KiB token cap fires before the
		// explicit `maxRefLineBytes` check on lines that exceed it.
		// Both refusal paths describe the same condition, so wrap the
		// scanner's surface error with the package sentinel callers can
		// match on with `errors.Is`.
		if errors.Is(err, bufio.ErrTooLong) {
			err = fmt.Errorf("dumbhttp: ref line exceeds scanner buffer: %w", ErrRefLineTooLarge)
		}
		s.firstErr = err
		return "", false, err
	}
	return "", false, nil
}

// splitRefLine splits a dumb-HTTP ref record into its OID and
// refname. The canonical separator is HTAB
// ([gitprotocol-http.adoc lines 158-200]); we tolerate runs of
// whitespace as well, matching real-world servers that emit a
// space. A line with fewer than two fields surfaces
// [ErrMalformedRefLine] wrapped with the offending text. A line
// whose `<oid> <SP> <refname>` size would exceed [maxRefLineBytes]
// surfaces [ErrRefLineTooLarge] instead — synthesising it would
// require a pkt-line length prefix wider than 4 hex nibbles.
//
// [gitprotocol-http.adoc lines 158-200]: https://github.com/git/git/blob/v2.54.0/Documentation/gitprotocol-http.adoc?plain=1#L158-L200
func splitRefLine(line string) (string, string, error) {
	oid, name, err := splitOIDAndName(line)
	if err != nil {
		return "", "", err
	}
	// `<oid> <SP> <refname>` plus the suffix the caller will append
	// must fit within [pktline.MaxPayload]; see [maxRefLineBytes].
	if n := len(oid) + 1 + len(name); n > maxRefLineBytes {
		return "", "", fmt.Errorf("dumbhttp: ref line %d bytes: %w", n, ErrRefLineTooLarge)
	}
	return oid, name, nil
}

// splitOIDAndName extracts the OID and refname fields without
// applying the size cap. It exists so [splitRefLine] can layer the
// [ErrRefLineTooLarge] check on top without duplicating field
// parsing.
func splitOIDAndName(line string) (string, string, error) {
	if oid, name, ok := strings.Cut(line, "\t"); ok {
		oid = strings.TrimSpace(oid)
		name = strings.TrimSpace(name)
		if oid == "" || name == "" {
			return "", "", fmt.Errorf("%w: %q", ErrMalformedRefLine, line)
		}
		return oid, name, nil
	}
	// No HTAB — fall back to whitespace splitting on the first run of
	// space/tab so a server using SP rather than HTAB still parses.
	idx := strings.IndexAny(line, " \t")
	if idx < 0 {
		return "", "", fmt.Errorf("%w: %q", ErrMalformedRefLine, line)
	}
	oid := strings.TrimSpace(line[:idx])
	name := strings.TrimSpace(line[idx+1:])
	if oid == "" || name == "" {
		return "", "", fmt.Errorf("%w: %q", ErrMalformedRefLine, line)
	}
	return oid, name, nil
}

// encodePktLine wraps payload in a pkt-line frame with a 4-byte
// lowercase ASCII hexadecimal length prefix, matching canonical
// Git's `pkt-line.c` output. Used in place of [pktline.Writer]
// because the synthesiser already owns its destination buffer
// (pending) and using the writer would require an intermediate
// destination [io.Writer].
//
// Callers MUST refuse oversized payloads upstream — see
// [splitRefLine] and [ErrRefLineTooLarge]. The panic below is a
// belt-and-braces guard against future regressions; it should
// never fire in practice.
func encodePktLine(payload string) []byte {
	total := pktLengthPrefix + len(payload)
	if total > 0xffff {
		panic("dumbhttp: payload exceeds pkt-line max — should have been refused upstream")
	}
	buf := make([]byte, total)
	buf[0] = hexDigits[(total>>12)&0xf]
	buf[1] = hexDigits[(total>>8)&0xf]
	buf[2] = hexDigits[(total>>4)&0xf]
	buf[3] = hexDigits[total&0xf]
	copy(buf[pktLengthPrefix:], payload)
	return buf
}
