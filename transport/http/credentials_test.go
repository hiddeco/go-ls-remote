package httpt

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBasic(t *testing.T) {
	t.Parallel()

	creds := Basic("alice", "s3cr3t")
	require.NotNil(t, creds)

	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/", http.NoBody)
	require.NoError(t, err)
	require.NoError(t, creds.Apply(r))

	got := r.Header.Get("Authorization")
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:s3cr3t"))
	assert.Equal(t, want, got, "Authorization header should be base64-encoded user:pass per RFC 7617")

	user, pass, ok := r.BasicAuth()
	require.True(t, ok)
	assert.Equal(t, "alice", user)
	assert.Equal(t, "s3cr3t", pass)
}

func TestBearer(t *testing.T) {
	t.Parallel()

	creds := Bearer("tok-abc")
	require.NotNil(t, creds)

	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/", http.NoBody)
	require.NoError(t, err)
	require.NoError(t, creds.Apply(r))

	assert.Equal(t, "Bearer tok-abc", r.Header.Get("Authorization"),
		"Bearer scheme per RFC 6750 §2.1")
}

func TestStatic_Resolve(t *testing.T) {
	t.Parallel()

	want := Basic("alice", "s3cr3t")
	resolver := Static(want)
	require.NotNil(t, resolver)

	got, err := resolver.Resolve(t.Context(), &url.URL{Scheme: "https", Host: "example.com"})
	require.NoError(t, err)
	assert.Same(t, want, got, "Static must return the wrapped Credentials verbatim")
}

func TestStatic_Resolve_Nil(t *testing.T) {
	t.Parallel()

	resolver := Static(nil)
	require.NotNil(t, resolver)

	got, err := resolver.Resolve(t.Context(), &url.URL{Scheme: "https", Host: "example.com"})
	require.NoError(t, err)
	assert.Nil(t, got, "Static(nil) means 'no credential available'")
}

// netrcAtFunc is the test seam matching the unexported constructor in
// credentials.go. The exported Netrc() reads ~/.netrc; tests use a
// path-injecting helper to stay hermetic.
//
// The seam is implemented via netrcResolver.path field; tests build
// the resolver directly.

func TestNetrc_Resolve_MachineMatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".netrc")
	body := strings.Join([]string{
		"machine example.com login alice password s3cr3t",
		"machine other.example.org login bob password hunter2",
		"",
	}, "\n")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	resolver := newNetrcResolver(path, nil)

	got, err := resolver.Resolve(t.Context(), &url.URL{Scheme: "https", Host: "example.com"})
	require.NoError(t, err)
	require.NotNil(t, got)

	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/", http.NoBody)
	require.NoError(t, err)
	require.NoError(t, got.Apply(r))
	user, pass, ok := r.BasicAuth()
	require.True(t, ok)
	assert.Equal(t, "alice", user)
	assert.Equal(t, "s3cr3t", pass)
}

func TestNetrc_Resolve_FirstMatchWins(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".netrc")
	body := strings.Join([]string{
		"machine example.com login first password one",
		"machine example.com login second password two",
		"",
	}, "\n")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	resolver := newNetrcResolver(path, nil)

	got, err := resolver.Resolve(t.Context(), &url.URL{Scheme: "https", Host: "example.com"})
	require.NoError(t, err)
	require.NotNil(t, got)

	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.com/", http.NoBody)
	require.NoError(t, err)
	require.NoError(t, got.Apply(r))
	user, _, _ := r.BasicAuth()
	assert.Equal(t, "first", user)
}

func TestNetrc_Resolve_DefaultFallthrough(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".netrc")
	body := strings.Join([]string{
		"machine other.example.org login bob password hunter2",
		"default login anon password public",
		"",
	}, "\n")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	resolver := newNetrcResolver(path, nil)

	got, err := resolver.Resolve(t.Context(), &url.URL{Scheme: "https", Host: "unknown.example.com"})
	require.NoError(t, err)
	require.NotNil(t, got)

	r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://unknown.example.com/", http.NoBody)
	require.NoError(t, err)
	require.NoError(t, got.Apply(r))
	user, pass, ok := r.BasicAuth()
	require.True(t, ok)
	assert.Equal(t, "anon", user)
	assert.Equal(t, "public", pass)
}

func TestNetrc_Resolve_NoMatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".netrc")
	body := "machine other.example.org login bob password hunter2\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	resolver := newNetrcResolver(path, nil)

	got, err := resolver.Resolve(t.Context(), &url.URL{Scheme: "https", Host: "unmatched.example.com"})
	require.NoError(t, err)
	assert.Nil(t, got, "no match and no default should return (nil, nil)")
}

func TestNetrc_Resolve_MissingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "does-not-exist")

	resolver := newNetrcResolver(path, nil)

	got, err := resolver.Resolve(t.Context(), &url.URL{Scheme: "https", Host: "example.com"})
	require.NoError(t, err, "missing file is not an error")
	assert.Nil(t, got)
}

func TestNetrc_Resolve_Comments(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".netrc")
	body := strings.Join([]string{
		"# top of file comment",
		"machine example.com login alice password s3cr3t",
		"# trailing comment",
		"",
	}, "\n")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	resolver := newNetrcResolver(path, nil)

	got, err := resolver.Resolve(t.Context(), &url.URL{Scheme: "https", Host: "example.com"})
	require.NoError(t, err)
	require.NotNil(t, got)
}

func TestNetrc_Resolve_Malformed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".netrc")
	// `machine` with no following token is malformed.
	body := "machine\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	resolver := newNetrcResolver(path, nil)

	got, err := resolver.Resolve(t.Context(), &url.URL{Scheme: "https", Host: "example.com"})
	require.Error(t, err)
	assert.Nil(t, got)
}

func TestNetrc_Resolve_WorldReadableWarns(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".netrc")
	body := "machine example.com login alice password s3cr3t\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	// os.WriteFile may be intersected with the process umask. Force the
	// world-readable bits explicitly.
	require.NoError(t, os.Chmod(path, 0o644))

	var warn bytes.Buffer
	resolver := newNetrcResolver(path, &warn)

	got, err := resolver.Resolve(t.Context(), &url.URL{Scheme: "https", Host: "example.com"})
	require.NoError(t, err, "world-readable file is still parsed; warning only")
	require.NotNil(t, got, "credential is still returned")

	assert.Contains(t, warn.String(), "readable or writable by group or world",
		"the warning must use the new wording covering read and write loosening")
	assert.Contains(t, warn.String(), path)
}

func TestNetrc_Resolve_RestrictiveModeNoWarn(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".netrc")
	body := "machine example.com login alice password s3cr3t\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))

	var warn bytes.Buffer
	resolver := newNetrcResolver(path, &warn)

	_, err := resolver.Resolve(t.Context(), &url.URL{Scheme: "https", Host: "example.com"})
	require.NoError(t, err)
	assert.Empty(t, warn.String(), "0600 must not warn")
}

func TestNetrc_PublicConstructor(t *testing.T) {
	// Smoke-test the exported Netrc() constructor: it should resolve to
	// nil when there is no `~/.netrc` for the test user.
	t.Setenv("HOME", t.TempDir())
	// Some platforms also consult USERPROFILE.
	t.Setenv("USERPROFILE", t.TempDir())

	resolver := Netrc()
	require.NotNil(t, resolver)

	got, err := resolver.Resolve(t.Context(), &url.URL{Scheme: "https", Host: "example.com"})
	require.NoError(t, err)
	assert.Nil(t, got)
}

// Ensure errors from a malformed file pass errors.Is on the package's
// netrc-error sentinel, so callers can tell parse errors from I/O
// errors.
func TestNetrc_ParseError_Is(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, ".netrc")
	require.NoError(t, os.WriteFile(path, []byte("machine\n"), 0o600))

	resolver := newNetrcResolver(path, nil)
	_, err := resolver.Resolve(t.Context(), &url.URL{Scheme: "https", Host: "example.com"})
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrNetrcParse, "parse errors must wrap ErrNetrcParse")
}

// TestNetrc_Resolve_WorldWritableWarns pins that a netrc whose mode
// grants group or world write access emits the loose-permissions
// warning. A world-writable netrc is more dangerous than a
// world-readable one: any local user can substitute credentials.
func TestNetrc_Resolve_WorldWritableWarns(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, ".netrc")
	require.NoError(t, os.WriteFile(path, []byte("machine example.com login a password b\n"), 0o600))
	// 0o622: owner rw, group w, other w — write bits only, no read bits.
	// This bypasses the old 0o044 read-only mask.
	require.NoError(t, os.Chmod(path, 0o622))

	var buf bytes.Buffer
	resolver := newNetrcResolver(path, &buf)

	u, err := url.Parse("https://example.com/repo")
	require.NoError(t, err)
	_, err = resolver.Resolve(t.Context(), u)
	require.NoError(t, err)

	assert.Contains(t, buf.String(), "readable or writable by group or world",
		"a world-writable netrc must emit the loose-permissions warning")
}
