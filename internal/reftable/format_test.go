package reftable

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixtureRoot resolves the repo-root testdata/reftable directory.
// Tests run from the package directory, so we walk up two levels.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	// internal/reftable -> repo root.
	return filepath.Join(wd, "..", "..", "testdata", "reftable")
}

func readFixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixtureRoot(t), rel))
	require.NoError(t, err, "read fixture %s", rel)
	return b
}

func Test_parseHeader(t *testing.T) {
	t.Run("v1_sha1_happy", func(t *testing.T) {
		buf := readFixture(t, "single-sha1/0001-0001-aaaaaaaa.ref")
		h, err := parseHeader(buf)
		require.NoError(t, err)
		assert.Equal(t, uint8(1), h.version)
		assert.Equal(t, objfmt.SHA1, h.algo)
		assert.Equal(t, uint32(4096), h.blockSize)
		assert.Equal(t, uint64(1), h.minUpdateIndex)
		assert.Equal(t, uint64(2), h.maxUpdateIndex)
	})

	t.Run("v2_sha256_happy", func(t *testing.T) {
		buf := readFixture(t, "single-sha256/0001-0001-aaaaaaaa.ref")
		h, err := parseHeader(buf)
		require.NoError(t, err)
		assert.Equal(t, uint8(2), h.version)
		assert.Equal(t, objfmt.SHA256, h.algo)
		assert.Equal(t, uint32(4096), h.blockSize)
		assert.Equal(t, uint64(1), h.minUpdateIndex)
		assert.Equal(t, uint64(2), h.maxUpdateIndex)
	})

	t.Run("short_input", func(t *testing.T) {
		_, err := parseHeader([]byte("REFT"))
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrShortFile), "want ErrShortFile, got %v", err)
	})

	t.Run("bad_magic", func(t *testing.T) {
		// Start from a valid v1 header and corrupt the magic.
		buf := append([]byte{}, readFixture(t, "single-sha1/0001-0001-aaaaaaaa.ref")[:headerSizeV1]...)
		copy(buf[:4], "XXXX")
		_, err := parseHeader(buf)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrBadMagic), "want ErrBadMagic, got %v", err)
	})

	t.Run("bad_version", func(t *testing.T) {
		buf := append([]byte{}, readFixture(t, "single-sha1/0001-0001-aaaaaaaa.ref")[:headerSizeV1]...)
		buf[4] = 99
		_, err := parseHeader(buf)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrUnsupportedVersion), "want ErrUnsupportedVersion, got %v", err)
	})

	t.Run("v2_bad_hash_id", func(t *testing.T) {
		buf := append([]byte{}, readFixture(t, "single-sha256/0001-0001-aaaaaaaa.ref")[:headerSizeV2]...)
		// Overwrite the trailing 4-byte hash_id with junk.
		copy(buf[24:28], []byte{'J', 'U', 'N', 'K'})
		_, err := parseHeader(buf)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrBadHashID), "want ErrBadHashID, got %v", err)
	})
}

func Test_verifyTrailer(t *testing.T) {
	t.Run("single_sha1_passes", func(t *testing.T) {
		buf := readFixture(t, "single-sha1/0001-0001-aaaaaaaa.ref")
		h, err := parseHeader(buf)
		require.NoError(t, err)
		assert.NoError(t, verifyTrailer(buf, h))
	})

	t.Run("single_sha256_passes", func(t *testing.T) {
		buf := readFixture(t, "single-sha256/0001-0001-aaaaaaaa.ref")
		h, err := parseHeader(buf)
		require.NoError(t, err)
		assert.NoError(t, verifyTrailer(buf, h))
	})

	t.Run("corrupt_trailer_fails", func(t *testing.T) {
		buf := readFixture(t, "corrupt-trailer-sha1.ref")
		h, err := parseHeader(buf)
		require.NoError(t, err)
		err = verifyTrailer(buf, h)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrTrailerChecksum), "want ErrTrailerChecksum, got %v", err)
	})

	t.Run("truncated_fails", func(t *testing.T) {
		// truncated-sha1.ref is shorter than headerSizeV1 + footerSizeV1
		// (24 + 68 = 92), so verifyTrailer's length guard fires.
		// parseHeader still succeeds because the fixture preserves the
		// first 24 bytes of the original single-sha1 header.
		buf := readFixture(t, "truncated-sha1.ref")
		h, err := parseHeader(buf)
		require.NoError(t, err)
		err = verifyTrailer(buf, h)
		require.Error(t, err)
		assert.True(t, errors.Is(err, ErrShortFile), "want ErrShortFile, got %v", err)
	})
}
