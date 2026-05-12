package wire

import (
	"bytes"
	"errors"
	"testing"
)

// FuzzCheckERRPacket exercises [CheckERRPacket] with arbitrary byte
// payloads. The contract under test is that the function is total —
// it never panics — and that its returned error is constrained to two
// shapes per [pkt-line.c:505-514]: either nil (no `ERR ` prefix) or an
// error wrapping [ErrServerRefused] (the four-byte literal `ERR ` was
// matched). No third concrete error is permitted. Seeds cover the
// boundary cases the unit suite does not enumerate exhaustively:
// the three-byte `ERR` non-match, an empty post-prefix message, an
// embedded NUL in the message, a normal payload that happens to begin
// with `ERR ` (still a match — canonical Git uses `starts_with`), and
// a 4 KiB payload to push the trim path.
//
// [pkt-line.c:505-514]: https://github.com/git/git/blob/v2.54.0/pkt-line.c#L505-L514
func FuzzCheckERRPacket(f *testing.F) {
	for _, seed := range checkERRPacketFuzzSeeds() {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, payload []byte) {
		err := CheckERRPacket(payload)
		if err == nil {
			return
		}
		if !errors.Is(err, ErrServerRefused) {
			t.Fatalf("CheckERRPacket returned unexpected error: %v", err)
		}
	})
}

// checkERRPacketFuzzSeeds returns the seed corpus for
// [FuzzCheckERRPacket].
func checkERRPacketFuzzSeeds() [][]byte {
	return [][]byte{
		// Empty input.
		nil,
		// Three-byte `ERR` without the trailing space — must not match
		// per [pkt-line.c:509-510].
		//
		// [pkt-line.c:509-510]: https://github.com/git/git/blob/v2.54.0/pkt-line.c#L509-L510
		[]byte("ERR"),
		// Canonical `ERR <message>` shape, no trailing LF.
		[]byte("ERR access denied"),
		// Canonical shape with trailing LF — the producer at
		// [pkt-line.c:699] does not append one, but framing layers
		// occasionally leave one in place.
		//
		// [pkt-line.c:699]: https://github.com/git/git/blob/v2.54.0/pkt-line.c#L699
		[]byte("ERR access denied\n"),
		// `ERR ` prefix followed by an empty message.
		[]byte("ERR "),
		// `ERR ` followed by a message containing an embedded NUL.
		[]byte("ERR access\x00denied"),
		// A normal payload that happens to begin with `ERR ` — still a
		// match per canonical Git's `starts_with(buffer, "ERR ")`.
		[]byte("ERR refs/heads/main abcdef0123456789"),
		// A 4 KiB payload after the prefix exercises the trim path on
		// a long message body.
		append([]byte("ERR "), bytes.Repeat([]byte{'a'}, 4096)...),
	}
}
