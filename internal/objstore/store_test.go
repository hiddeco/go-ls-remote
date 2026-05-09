package objstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpen_EmptyRepo(t *testing.T) {
	// A brand-new sha1+files repo: no commits, no packs, no
	// `packed-refs`. The opener must succeed, default to SHA-1, and
	// surface a usable [Store].
	root := materializeFixture(t, "empty")

	s, err := Open(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	assert.Equal(t, objfmt.SHA1, s.Algo())
	assert.NotNil(t, s.refs)
	assert.NotNil(t, s.loose)
	assert.NotNil(t, s.packs)
}

func TestOpen_SHA256Repo(t *testing.T) {
	// `extensions.objectFormat = sha256` must propagate through
	// `readGitConfig` into [Store.Algo].
	root := materializeFixture(t, "sha256")

	s, err := Open(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	assert.Equal(t, objfmt.SHA256, s.Algo())
}

func TestOpen_ReftableRepo(t *testing.T) {
	// `extensions.refStorage = reftable` must select the reftable ref
	// backend. The `with-reftable-content` fixture carries a populated
	// stack (HEAD plus refs/heads/main) — the empty-stack `with-reftable`
	// fixture would fail HEAD resolution, since the backend treats a
	// missing HEAD record as corruption (canonical Git always writes one
	// at `git init`). Backend-internal behaviour is exercised by
	// `reftable_backend_test.go`; this test only confirms wiring.
	root := materializeFixture(t, "with-reftable-content")

	s, err := Open(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, ok := s.refs.(*reftableBackend)
	assert.True(t, ok, "want reftable backend, got %T", s.refs)
}

func TestOpen_MissingPathReturnsErrNotARepo(t *testing.T) {
	// A path that does not exist must surface [ErrNotARepo] verbatim
	// via [errors.Is]; opener errors must not paper over the resolver
	// distinction the caller relies on.
	missing := filepath.Join(t.TempDir(), "nope")

	_, err := Open(missing)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNotARepo), "expected ErrNotARepo, got %v", err)
}

func TestOpen_UnknownRefStorageReturnsErrUnsupportedFormat(t *testing.T) {
	// Build a minimal repo on the fly carrying a config the parser
	// rejects. The opener must propagate [ErrUnsupportedFormat] without
	// wrapping it into something callers cannot unwrap.
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "objects"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "refs"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config"),
		[]byte("[extensions]\n\trefStorage = packed\n"), 0o644))

	_, err := Open(dir)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrUnsupportedFormat),
		"expected ErrUnsupportedFormat, got %v", err)
}

func TestWithoutCRCCheck_FlipsConfig(t *testing.T) {
	// The default is verifyCRC = true; `WithoutCRCCheck` flips it to
	// false. Probed via the unexported config field rather than a
	// public surface so the test stays in lockstep with intent.
	root := materializeFixture(t, "empty")

	s, err := Open(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	assert.True(t, s.cfg.verifyCRC, "default must be true")

	s2, err := Open(root, WithoutCRCCheck())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })
	assert.False(t, s2.cfg.verifyCRC, "WithoutCRCCheck must flip to false")
}

func TestStore_CloseIsIdempotent(t *testing.T) {
	// `Close` must be safe to call repeatedly; subsequent calls return
	// the joined error (here, nil) without panicking.
	root := materializeFixture(t, "empty")

	s, err := Open(root)
	require.NoError(t, err)

	assert.NoError(t, s.Close())
	assert.NoError(t, s.Close(), "second Close must not panic and must return nil")
}

func TestStore_AlgoDelegatesToConfig(t *testing.T) {
	// Focused assertion that [Store.Algo] mirrors the parsed config —
	// implied by the SHA-1 / SHA-256 cases above but cheap to lock in
	// directly so a future refactor that drops the field is caught.
	root := materializeFixture(t, "sha256")

	s, err := Open(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	assert.Equal(t, objfmt.SHA256, s.Algo())
	assert.Equal(t, s.cfg.algo, s.Algo())
}

func TestOpen_SelectsMidxWhenPresent(t *testing.T) {
	// A `multi-pack-index` file under `objects/pack/` flips the pack
	// backend selector to the midx variant. `midx-with-siblings/`
	// carries a real midx body (plus its packs and one sibling pack)
	// so the selector and the constructor exercise the same shape end
	// to end.
	root := materializeFixture(t, "midx-with-siblings")

	s, err := Open(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, ok := s.packs.(*midxBackend)
	assert.True(t, ok, "want midxBackend, got %T", s.packs)
}

func TestOpen_SelectsIdxCatalogByDefault(t *testing.T) {
	// Without a `multi-pack-index`, the opener falls back to the
	// per-`.idx` catalogue. Pairs with the midx case to lock in the
	// selector logic on both branches.
	root := materializeFixture(t, "empty")

	s, err := Open(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	_, ok := s.packs.(*idxCatalog)
	assert.True(t, ok, "want idxCatalog, got %T", s.packs)
}
