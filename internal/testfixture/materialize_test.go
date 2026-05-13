package testfixture_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/hiddeco/go-ls-remote/internal/testfixture"
)

// TestMaterializeRepo_RenamesDotGit pins the load-bearing rename: the
// committed `dotgit/` directory must surface as `.git/` after
// materialisation, and the returned path must point at it.
func TestMaterializeRepo_RenamesDotGit(t *testing.T) {
	t.Parallel()

	gitdir := testfixture.MaterializeRepo(t, "empty")

	assert.Equal(t, ".git", filepath.Base(gitdir))
	info, err := os.Stat(gitdir)
	require.NoError(t, err)
	assert.True(t, info.IsDir())

	// HEAD is the smallest fixture artefact every repo ships; its
	// presence confirms the walk copied file content, not just
	// directory shells.
	_, err = os.Stat(filepath.Join(gitdir, "HEAD"))
	assert.NoError(t, err)
}

// TestMaterializeRepoTree_MultiRepoLayout pins the alternates-chain
// shape: the fixture has no top-level `dotgit/`, so
// [MaterializeRepoTree] is required and the per-repo gitdirs surface
// under `a/.git`, `b/.git`, `c/.git`.
func TestMaterializeRepoTree_MultiRepoLayout(t *testing.T) {
	t.Parallel()

	root := testfixture.MaterializeRepoTree(t, "with-alternates-chain")

	for _, name := range []string{"a", "b", "c"} {
		gitdir := filepath.Join(root, name, ".git")
		info, err := os.Stat(gitdir)
		require.NoError(t, err, "expected %s to exist", gitdir)
		assert.True(t, info.IsDir())
	}
}
