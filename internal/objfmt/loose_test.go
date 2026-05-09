package objfmt

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// zlibLoose builds an in-memory zlib-compressed loose object with the
// given header bytes (without trailing NUL) and body. The header is
// written verbatim so tests can exercise malformed headers too.
func zlibLoose(t *testing.T, header string, body []byte) []byte {
	t.Helper()
	var raw bytes.Buffer
	raw.WriteString(header)
	raw.WriteByte(0)
	raw.Write(body)

	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, err := zw.Write(raw.Bytes())
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func TestReadLooseHeader(t *testing.T) {
	t.Run("blob with random body round-trips", func(t *testing.T) {
		body := make([]byte, 10)
		_, err := rand.Read(body)
		require.NoError(t, err)

		in := zlibLoose(t, "blob 10", body)
		typ, size, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.NoError(t, err)
		assert.Equal(t, TypeBlob, typ)
		assert.Equal(t, int64(10), size)

		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		assert.Equal(t, body, got)
		require.NoError(t, rc.Close())
	})

	t.Run("commit round-trips", func(t *testing.T) {
		body := []byte("tree deadbeef\nauthor x\n\nmsg\n")
		in := zlibLoose(t, "commit 28", body)
		typ, size, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.NoError(t, err)
		assert.Equal(t, TypeCommit, typ)
		assert.Equal(t, int64(len(body)), size)

		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		assert.Equal(t, body, got)
		require.NoError(t, rc.Close())
	})

	t.Run("tree round-trips", func(t *testing.T) {
		body := []byte("100644 file\x00abcdefghij1234567890")
		in := zlibLoose(t, "tree 32", body)
		typ, size, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.NoError(t, err)
		assert.Equal(t, TypeTree, typ)
		assert.Equal(t, int64(len(body)), size)

		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		assert.Equal(t, body, got)
		require.NoError(t, rc.Close())
	})

	t.Run("tag round-trips", func(t *testing.T) {
		body := []byte("object deadbeef\ntype commit\ntag v1\n")
		in := zlibLoose(t, "tag 35", body)
		typ, size, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.NoError(t, err)
		assert.Equal(t, TypeTag, typ)
		assert.Equal(t, int64(len(body)), size)

		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		assert.Equal(t, body, got)
		require.NoError(t, rc.Close())
	})

	t.Run("empty blob has zero size", func(t *testing.T) {
		in := zlibLoose(t, "blob 0", nil)
		typ, size, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.NoError(t, err)
		assert.Equal(t, TypeBlob, typ)
		assert.Equal(t, int64(0), size)

		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		assert.Empty(t, got)
		require.NoError(t, rc.Close())
	})

	t.Run("truncated zlib stream errors with nil body", func(t *testing.T) {
		// Use an oversized body so we can truncate compressed bytes
		// well before the header NUL is decompressed; this forces
		// the header read itself to hit unexpected EOF.
		full := zlibLoose(t, "blob 1000", make([]byte, 1000))
		require.Greater(t, len(full), 6)
		truncated := full[:6]

		_, _, rc, err := ReadLooseHeader(bytes.NewReader(truncated))
		require.Error(t, err)
		assert.Nil(t, rc)
		assert.True(t,
			errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF),
			"want EOF/ErrUnexpectedEOF in error chain, got %v", err)
	})

	t.Run("unknown type name errors", func(t *testing.T) {
		in := zlibLoose(t, "garbage 0", nil)
		_, _, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.Error(t, err)
		assert.Nil(t, rc)
		assert.Contains(t, err.Error(), "garbage")
	})

	t.Run("non-numeric size errors", func(t *testing.T) {
		in := zlibLoose(t, "blob notanumber", nil)
		_, _, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.Error(t, err)
		assert.Nil(t, rc)
	})

	t.Run("negative size errors", func(t *testing.T) {
		in := zlibLoose(t, "blob -5", nil)
		_, _, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.Error(t, err)
		assert.Nil(t, rc)
	})

	t.Run("rejects leading-zero size", func(t *testing.T) {
		// Canonical decimal: `010` is not valid. See
		// `object-file.c:369-380` (`parse_loose_header`).
		in := zlibLoose(t, "blob 010", make([]byte, 10))
		_, _, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.Error(t, err)
		assert.Nil(t, rc)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "010")
	})

	t.Run("rejects leading-plus size", func(t *testing.T) {
		in := zlibLoose(t, "blob +10", make([]byte, 10))
		_, _, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.Error(t, err)
		assert.Nil(t, rc)
		assert.ErrorIs(t, err, ErrCorrupt)
	})

	t.Run("rejects leading-space size", func(t *testing.T) {
		// Two spaces between type and size means the size field begins
		// with a space; canonical Git's manual digit loop rejects it.
		in := zlibLoose(t, "blob  10", make([]byte, 10))
		_, _, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.Error(t, err)
		assert.Nil(t, rc)
		assert.ErrorIs(t, err, ErrCorrupt)
	})

	t.Run("rejects trailing-space size", func(t *testing.T) {
		in := zlibLoose(t, "blob 10 ", make([]byte, 10))
		_, _, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.Error(t, err)
		assert.Nil(t, rc)
		assert.ErrorIs(t, err, ErrCorrupt)
	})

	t.Run("accepts single zero size", func(t *testing.T) {
		in := zlibLoose(t, "blob 0", nil)
		typ, size, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.NoError(t, err)
		assert.Equal(t, TypeBlob, typ)
		assert.Equal(t, int64(0), size)
		require.NoError(t, rc.Close())
	})

	t.Run("rejects empty size field", func(t *testing.T) {
		in := zlibLoose(t, "blob ", nil)
		_, _, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.Error(t, err)
		assert.Nil(t, rc)
		assert.ErrorIs(t, err, ErrCorrupt)
	})

	t.Run("invalid zlib header errors", func(t *testing.T) {
		_, _, rc, err := ReadLooseHeader(bytes.NewReader([]byte("not zlib")))
		require.Error(t, err)
		assert.Nil(t, rc)
	})

	t.Run("body Close is idempotent and non-panicking", func(t *testing.T) {
		body := []byte("hello")
		in := zlibLoose(t, "blob 5", body)
		_, _, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.NoError(t, err)
		require.NoError(t, rc.Close())
		// Second close must not panic; zlib's Close is idempotent and
		// returns nil on a second call.
		assert.NotPanics(t, func() { _ = rc.Close() })
	})

	t.Run("closing body without reading does not panic", func(t *testing.T) {
		in := zlibLoose(t, "blob 10", make([]byte, 10))
		_, _, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.NoError(t, err)
		assert.NotPanics(t, func() { _ = rc.Close() })
	})

	t.Run("rejects trailing garbage after declared body", func(t *testing.T) {
		// Build a deflate stream whose inflated payload is
		// `blob 5\x00helloEXTRA` — the header declares 5 body bytes but
		// the stream produces 10. Canonical Git surfaces "garbage at
		// end of loose object" via `unpack_loose_rest` in
		// `object-file.c:282-328`. After draining exactly `size` bytes,
		// `Close` must surface the same corruption.
		in := zlibLoose(t, "blob 5", []byte("helloEXTRA"))
		_, size, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.NoError(t, err)
		require.Equal(t, int64(5), size)

		got := make([]byte, 5)
		_, err = io.ReadFull(rc, got)
		require.NoError(t, err)
		assert.Equal(t, []byte("hello"), got)

		err = rc.Close()
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "garbage")
	})

	t.Run("rejects trailing garbage drained via ReadAll", func(t *testing.T) {
		// Same corruption shape, but the caller reads with
		// [io.ReadAll] (one large read returning all inflated bytes).
		// The over-read must be observed during `Read` and surfaced
		// from `Close`, not silently absorbed.
		in := zlibLoose(t, "blob 5", []byte("helloEXTRA"))
		_, _, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.NoError(t, err)

		_, err = io.ReadAll(rc)
		require.NoError(t, err)

		err = rc.Close()
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrCorrupt)
		assert.Contains(t, err.Error(), "garbage")
	})

	t.Run("valid object has no trailer error", func(t *testing.T) {
		// Sanity: a well-formed loose object, fully drained and closed,
		// must not surface a trailing-garbage error.
		body := []byte("hello world")
		in := zlibLoose(t, "blob 11", body)
		_, _, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.NoError(t, err)
		got, err := io.ReadAll(rc)
		require.NoError(t, err)
		assert.Equal(t, body, got)
		assert.NoError(t, rc.Close())
	})

	t.Run("header-only read does not validate trailer", func(t *testing.T) {
		// Even with extra inflated bytes past the declared size, a
		// caller that reads only the header and closes without
		// draining the body must not see a corruption error. The
		// trailer check belongs in the body-fully-read path; lazy
		// callers pay nothing.
		in := zlibLoose(t, "blob 5", []byte("helloEXTRA"))
		_, _, rc, err := ReadLooseHeader(bytes.NewReader(in))
		require.NoError(t, err)
		assert.NoError(t, rc.Close())
	})
}
