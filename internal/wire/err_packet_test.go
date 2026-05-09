package wire

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckERRPacket(t *testing.T) {
	t.Run("nil payload", func(t *testing.T) {
		assert.NoError(t, CheckERRPacket(nil))
	})

	t.Run("empty payload", func(t *testing.T) {
		assert.NoError(t, CheckERRPacket([]byte{}))
	})

	t.Run("non-ERR payload", func(t *testing.T) {
		assert.NoError(t, CheckERRPacket([]byte("deadbeef refs/heads/main")))
	})

	t.Run("three-byte ERR without trailing space", func(t *testing.T) {
		// `pkt-line.c:509-510` matches the literal four bytes `ERR `.
		// A payload of just `ERR` is not a server error packet.
		assert.NoError(t, CheckERRPacket([]byte("ERR")))
	})

	t.Run("ERRX without trailing space", func(t *testing.T) {
		// `ERRX...` does not begin with the four-byte literal `ERR `.
		assert.NoError(t, CheckERRPacket([]byte("ERRX boom")))
	})

	t.Run("exact ERR prefix with empty message", func(t *testing.T) {
		err := CheckERRPacket([]byte("ERR "))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrServerRefused)
		assert.Equal(t, "wire: server refused: ", err.Error())
	})

	t.Run("ERR followed by message", func(t *testing.T) {
		err := CheckERRPacket([]byte("ERR access denied"))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrServerRefused)
		assert.Contains(t, err.Error(), "access denied")
	})

	t.Run("ERR with trailing LF stripped", func(t *testing.T) {
		err := CheckERRPacket([]byte("ERR boom\n"))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrServerRefused)
		// The single trailing LF on the payload is stripped per the
		// canonical-Git producer at `pkt-line.c:699`.
		assert.Equal(t, "wire: server refused: boom", err.Error())
	})

	t.Run("ERR with only LF after prefix", func(t *testing.T) {
		err := CheckERRPacket([]byte("ERR \n"))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrServerRefused)
		assert.Equal(t, "wire: server refused: ", err.Error())
	})

	t.Run("ERR with leading double space inside message", func(t *testing.T) {
		// First space belongs to the prefix; second space is the first
		// byte of the message and must be preserved verbatim.
		err := CheckERRPacket([]byte("ERR  doubled"))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrServerRefused)
		assert.Equal(t, "wire: server refused:  doubled", err.Error())
	})

	t.Run("ERR with embedded NUL bytes in message", func(t *testing.T) {
		payload := []byte("ERR access\x00denied")
		err := CheckERRPacket(payload)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrServerRefused)
		// NULs are preserved verbatim — `%s` formatting includes raw
		// bytes without escaping.
		assert.True(t, strings.Contains(err.Error(), "access"))
		assert.True(t, strings.Contains(err.Error(), "denied"))
		assert.True(t, strings.Contains(err.Error(), "access\x00denied"),
			"raw NUL byte must round-trip into the wrapped message")
	})

	t.Run("only LF strips a single trailing newline", func(t *testing.T) {
		// Two trailing LFs — only the last is stripped (matches
		// `bytes.TrimSuffix` with a one-byte suffix).
		err := CheckERRPacket([]byte("ERR boom\n\n"))
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrServerRefused)
		assert.Equal(t, "wire: server refused: boom\n", err.Error())
	})
}
