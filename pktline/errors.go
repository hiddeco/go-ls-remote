package pktline

import "errors"

// Sentinel errors returned by [Reader] and [Writer]. Callers match
// against these with [errors.Is]; the wrapping `fmt.Errorf("%w: ...")`
// adds the offending value (length, byte, payload size) for
// diagnostics.
//
// I/O errors from the underlying [io.Reader] or [io.Writer] propagate
// unwrapped; in particular, end-of-stream returns [io.EOF] and a
// truncated header or payload returns [io.ErrUnexpectedEOF]. The
// errors below cover only the codec-level violations.
var (
	// ErrInvalidHex is returned when a length-prefix byte is not a
	// hexadecimal digit. Canonical Git's `pkt-line.c:381` accepts
	// either case (`hexval`).
	ErrInvalidHex = errors.New("pktline: invalid hex byte in length prefix")

	// ErrInvalidLength is returned when the length prefix decodes to a
	// value of 3 — the only sub-header value not assigned to a control
	// packet. `pkt-line.c:446` rejects the same range.
	ErrInvalidLength = errors.New("pktline: invalid length prefix")

	// ErrPayloadTooLarge is returned when the announced payload length
	// (length prefix minus four) exceeds [MaxPayload], or when a writer
	// is asked to emit a payload larger than [MaxPayload].
	ErrPayloadTooLarge = errors.New("pktline: payload exceeds MaxPayload")
)
