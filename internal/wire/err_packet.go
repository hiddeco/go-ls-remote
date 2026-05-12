package wire

import (
	"bytes"
	"fmt"
)

// errPacketPrefix is the four-byte literal that marks a server `ERR`
// pkt-line per [pkt-line.c:509-510]. The trailing space is part of the
// match — a payload of just `ERR` (three bytes) is NOT a server error
// packet; canonical Git uses `starts_with(buffer, "ERR ")`.
//
// [pkt-line.c:509-510]: https://github.com/git/git/blob/v2.54.0/pkt-line.c#L509-L510
var errPacketPrefix = []byte("ERR ")

// CheckERRPacket inspects a data pkt-line's payload for the literal
// `ERR ` prefix per [pkt-line.c:509-510] and returns a non-nil error
// wrapping [ErrServerRefused] when the prefix is present. The wrapped
// error carries the message text that follows the prefix, with a
// single trailing LF stripped if present (the canonical producer at
// [pkt-line.c:699] writes the message without a newline, but
// callers may have already pulled the payload from a framing layer
// that left one in place).
//
// Callers can match the sentinel via `errors.Is(err, ErrServerRefused)`.
// The message bytes may contain embedded NULs — they are preserved in
// the wrapped error's text without escaping.
//
// payload is the pkt-line's data section (typically `pkt.Data`); the
// caller is expected to have already discarded the four-byte length
// prefix that pkt-line framing strips.
//
// [pkt-line.c:509-510]: https://github.com/git/git/blob/v2.54.0/pkt-line.c#L509-L510
// [pkt-line.c:699]: https://github.com/git/git/blob/v2.54.0/pkt-line.c#L699
func CheckERRPacket(payload []byte) error {
	msg, ok := bytes.CutPrefix(payload, errPacketPrefix)
	if !ok {
		return nil
	}
	msg = bytes.TrimSuffix(msg, []byte{'\n'})
	return fmt.Errorf("%w: %s", ErrServerRefused, msg)
}
