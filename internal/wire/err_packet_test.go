package wire

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckERRPacket(t *testing.T) {
	t.Parallel()
	t.Run("nil payload", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, CheckERRPacket(nil))
	})

	t.Run("empty payload", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, CheckERRPacket([]byte{}))
	})

	t.Run("non-ERR payload", func(t *testing.T) {
		t.Parallel()
		assert.NoError(t, CheckERRPacket([]byte("deadbeef refs/heads/main")))
	})

	t.Run("three-byte ERR without trailing space", func(t *testing.T) {
		t.Parallel()
		// [pkt-line.c:509-510] matches the literal four bytes `ERR `.
		// A payload of just `ERR` is not a server error packet.
		//
		// [pkt-line.c:509-510]: https://github.com/git/git/blob/v2.54.0/pkt-line.c#L509-L510
		assert.NoError(t, CheckERRPacket([]byte("ERR")))
	})

	t.Run("ERRX without trailing space", func(t *testing.T) {
		t.Parallel()
		// `ERRX...` does not begin with the four-byte literal `ERR `.
		assert.NoError(t, CheckERRPacket([]byte("ERRX boom")))
	})

	t.Run("exact ERR prefix with empty message", func(t *testing.T) {
		t.Parallel()
		err := CheckERRPacket([]byte("ERR "))
		require.Error(t, err)
		require.ErrorIs(t, err, ErrServerRefused)
		assert.Equal(t, "wire: server refused: ", err.Error())
	})

	t.Run("ERR followed by message", func(t *testing.T) {
		t.Parallel()
		err := CheckERRPacket([]byte("ERR access denied"))
		require.Error(t, err)
		require.ErrorIs(t, err, ErrServerRefused)
		assert.Contains(t, err.Error(), "access denied")
	})

	t.Run("ERR with trailing LF stripped", func(t *testing.T) {
		t.Parallel()
		err := CheckERRPacket([]byte("ERR boom\n"))
		require.Error(t, err)
		require.ErrorIs(t, err, ErrServerRefused)
		// The single trailing LF on the payload is stripped per the
		// canonical-Git producer at [pkt-line.c:699].
		//
		// [pkt-line.c:699]: https://github.com/git/git/blob/v2.54.0/pkt-line.c#L699
		assert.Equal(t, "wire: server refused: boom", err.Error())
	})

	t.Run("ERR with only LF after prefix", func(t *testing.T) {
		t.Parallel()
		err := CheckERRPacket([]byte("ERR \n"))
		require.Error(t, err)
		require.ErrorIs(t, err, ErrServerRefused)
		assert.Equal(t, "wire: server refused: ", err.Error())
	})

	t.Run("ERR with leading double space inside message", func(t *testing.T) {
		t.Parallel()
		// First space belongs to the prefix; second space is the first
		// byte of the message and must be preserved verbatim.
		err := CheckERRPacket([]byte("ERR  doubled"))
		require.Error(t, err)
		require.ErrorIs(t, err, ErrServerRefused)
		assert.Equal(t, "wire: server refused:  doubled", err.Error())
	})

	t.Run("ERR with embedded NUL bytes in message", func(t *testing.T) {
		t.Parallel()
		payload := []byte("ERR access\x00denied")
		err := CheckERRPacket(payload)
		require.Error(t, err)
		require.ErrorIs(t, err, ErrServerRefused)
		// NULs are preserved verbatim — `%s` formatting includes raw
		// bytes without escaping.
		assert.Contains(t, err.Error(), "access")
		assert.Contains(t, err.Error(), "denied")
		assert.Contains(t, err.Error(), "access\x00denied",
			"raw NUL byte must round-trip into the wrapped message")
	})

	t.Run("only LF strips a single trailing newline", func(t *testing.T) {
		t.Parallel()
		// Two trailing LFs — only the last is stripped (matches
		// `bytes.TrimSuffix` with a one-byte suffix).
		err := CheckERRPacket([]byte("ERR boom\n\n"))
		require.Error(t, err)
		require.ErrorIs(t, err, ErrServerRefused)
		assert.Equal(t, "wire: server refused: boom\n", err.Error())
	})
}
