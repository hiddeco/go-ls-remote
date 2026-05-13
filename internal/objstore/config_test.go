package objstore

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/objfmt"
)

// writeConfig drops body into <commonDir>/config and returns commonDir.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	commonDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(commonDir, "config"), []byte(body), 0o644))
	return commonDir
}

func TestReadGitConfig_NoConfigFile(t *testing.T) {
	t.Parallel()
	// A bare common dir with no `config` file is treated as
	// all-defaults: SHA-1 objects, files-backed refs.
	commonDir := t.TempDir()

	cfg, err := readGitConfig(commonDir)
	require.NoError(t, err)
	assert.Equal(t, objfmt.SHA1, cfg.algo)
	assert.Equal(t, "files", cfg.refStorage.format)
	assert.Empty(t, cfg.refStorage.location)
}

func TestReadGitConfig_NoExtensionsSection(t *testing.T) {
	t.Parallel()
	// Config file present but missing the `[extensions]` section. The
	// reader must consult only that section and return defaults
	// otherwise.
	commonDir := writeConfig(t, "[core]\n\trepositoryformatversion = 0\n")

	cfg, err := readGitConfig(commonDir)
	require.NoError(t, err)
	assert.Equal(t, objfmt.SHA1, cfg.algo)
	assert.Equal(t, "files", cfg.refStorage.format)
}

func TestReadGitConfig_EmptyExtensionsSection(t *testing.T) {
	t.Parallel()
	// `[extensions]` header with no keys: same as absent — defaults.
	commonDir := writeConfig(t, "[extensions]\n")

	cfg, err := readGitConfig(commonDir)
	require.NoError(t, err)
	assert.Equal(t, objfmt.SHA1, cfg.algo)
	assert.Equal(t, "files", cfg.refStorage.format)
}

func TestReadGitConfig_ParseTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		body         string
		wantAlgo     objfmt.Algo
		wantFormat   string
		wantLocation string
	}{
		{
			name:       "objectFormat sha1",
			body:       "[extensions]\n\tobjectFormat = sha1\n",
			wantAlgo:   objfmt.SHA1,
			wantFormat: "files",
		},
		{
			name:       "objectFormat sha256",
			body:       "[extensions]\n\tobjectFormat = sha256\n",
			wantAlgo:   objfmt.SHA256,
			wantFormat: "files",
		},
		{
			name:       "objectFormat case-insensitive",
			body:       "[extensions]\n\tobjectFormat = SHA256\n",
			wantAlgo:   objfmt.SHA256,
			wantFormat: "files",
		},
		{
			name:       "refStorage files explicit",
			body:       "[extensions]\n\trefStorage = files\n",
			wantAlgo:   objfmt.SHA1,
			wantFormat: "files",
		},
		{
			name:       "refStorage reftable",
			body:       "[extensions]\n\trefStorage = reftable\n",
			wantAlgo:   objfmt.SHA1,
			wantFormat: "reftable",
		},
		{
			name:       "refStorage case-insensitive",
			body:       "[extensions]\n\trefStorage = REFTABLE\n",
			wantAlgo:   objfmt.SHA1,
			wantFormat: "reftable",
		},
		{
			name:         "refStorage URI reftable relative",
			body:         "[extensions]\n\trefStorage = \"reftable://./reftable\"\n",
			wantAlgo:     objfmt.SHA1,
			wantFormat:   "reftable",
			wantLocation: "./reftable",
		},
		{
			name:         "refStorage URI files absolute",
			body:         "[extensions]\n\trefStorage = \"files:///abs/path\"\n",
			wantAlgo:     objfmt.SHA1,
			wantFormat:   "files",
			wantLocation: "/abs/path",
		},
		{
			name:         "refStorage URI unquoted",
			body:         "[extensions]\n\trefStorage = reftable:///srv/repo/reftable\n",
			wantAlgo:     objfmt.SHA1,
			wantFormat:   "reftable",
			wantLocation: "/srv/repo/reftable",
		},
		{
			name:       "quoted objectFormat value",
			body:       "[extensions]\n\tobjectFormat = \"sha256\"\n",
			wantAlgo:   objfmt.SHA256,
			wantFormat: "files",
		},
		{
			name: "mixed sections only extensions consumed",
			body: `[core]
	repositoryformatversion = 1
	bare = false
[remote "origin"]
	url = git@example.com:foo/bar
	fetch = +refs/heads/*:refs/remotes/origin/*
[user]
	name = Someone
[extensions]
	objectFormat = sha256
	refStorage = reftable
[branch "main"]
	remote = origin
`,
			wantAlgo:   objfmt.SHA256,
			wantFormat: "reftable",
		},
		{
			name: "comments mid-file",
			body: `# top-level comment
; semicolon comment
[extensions] # trailing comment after header
	; key comment line
	objectFormat = sha256 # trailing
	refStorage = reftable ; trailing semicolon
`,
			wantAlgo:   objfmt.SHA256,
			wantFormat: "reftable",
		},
		{
			name: "case-insensitive section and key",
			body: `[Extensions]
	ObjectFormat = sha256
	RefStorage = reftable
`,
			wantAlgo:   objfmt.SHA256,
			wantFormat: "reftable",
		},
		{
			name: "subsection header before extensions does not break parsing",
			body: `[remote "origin"]
	url = x
[extensions]
	objectFormat = sha256
`,
			wantAlgo:   objfmt.SHA256,
			wantFormat: "files",
		},
		{
			name: "last-write-wins within extensions",
			body: `[extensions]
	objectFormat = sha1
	objectFormat = sha256
`,
			wantAlgo:   objfmt.SHA256,
			wantFormat: "files",
		},
		{
			name: "extensions split across two headers (last value wins)",
			body: `[extensions]
	objectFormat = sha1
[core]
	repositoryformatversion = 1
[extensions]
	refStorage = reftable
`,
			wantAlgo:   objfmt.SHA1,
			wantFormat: "reftable",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			commonDir := writeConfig(t, tc.body)

			cfg, err := readGitConfig(commonDir)
			require.NoError(t, err)
			assert.Equal(t, tc.wantAlgo, cfg.algo, "algo")
			assert.Equal(t, tc.wantFormat, cfg.refStorage.format, "refStorage.format")
			assert.Equal(t, tc.wantLocation, cfg.refStorage.location, "refStorage.location")
		})
	}
}

func TestReadGitConfig_UnknownObjectFormat(t *testing.T) {
	t.Parallel()
	commonDir := writeConfig(t, "[extensions]\n\tobjectFormat = sha512\n")

	_, err := readGitConfig(commonDir)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnsupportedFormat,
		"expected ErrUnsupportedFormat, got %v", err)
	assert.Contains(t, err.Error(), "sha512")
	assert.Contains(t, err.Error(), "extensions.objectFormat")
}

func TestReadGitConfig_UnknownRefStorage(t *testing.T) {
	t.Parallel()
	commonDir := writeConfig(t, "[extensions]\n\trefStorage = packed\n")

	_, err := readGitConfig(commonDir)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnsupportedFormat,
		"expected ErrUnsupportedFormat, got %v", err)
	assert.Contains(t, err.Error(), "packed")
	assert.Contains(t, err.Error(), "extensions.refStorage")
}

func TestReadGitConfig_UnknownURIFormat(t *testing.T) {
	t.Parallel()
	commonDir := writeConfig(t, "[extensions]\n\trefStorage = packed://./somewhere\n")

	_, err := readGitConfig(commonDir)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrUnsupportedFormat,
		"expected ErrUnsupportedFormat, got %v", err)
	assert.Contains(t, err.Error(), "packed://./somewhere")
}

func TestReadGitConfig_ReadErrorWrapped(t *testing.T) {
	t.Parallel()
	// A directory at <commonDir>/config makes the read fail with a
	// non-NotExist error. The wrapper must surface the path.
	commonDir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(commonDir, "config"), 0o755))

	_, err := readGitConfig(commonDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "objstore: read ",
		"want wrapped read error, got %v", err)
	assert.Contains(t, err.Error(), filepath.Join(commonDir, "config"))
}
