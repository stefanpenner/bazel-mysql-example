package copy

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopyDatadirIsPrivate(t *testing.T) {
	src := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(src, "mysql"), 0o750))
	payload := []byte("ibdata")
	row := filepath.Join(src, "mysql", "row")
	require.NoError(t, os.WriteFile(row, payload, 0o640))
	require.NoError(t, os.Chmod(row, 0o444))
	require.NoError(t, os.Symlink(row, filepath.Join(src, "rowlink")))

	linkDir := t.TempDir()
	require.NoError(t, os.Symlink(src, filepath.Join(linkDir, "datadir")))

	dst := filepath.Join(t.TempDir(), "data")
	require.NoError(t, CopyDatadir(filepath.Join(linkDir, "datadir"), dst))

	copied := filepath.Join(dst, "mysql", "row")
	got, err := os.ReadFile(copied)
	require.NoError(t, err)
	assert.Equal(t, payload, got)
	writable, err := os.OpenFile(copied, os.O_WRONLY, 0)
	require.NoError(t, err)
	writable.Close()

	srcInfo, err := os.Stat(row)
	require.NoError(t, err)
	dstInfo, err := os.Stat(filepath.Join(dst, "mysql", "row"))
	require.NoError(t, err)
	assert.False(t, os.SameFile(srcInfo, dstInfo))

	require.NoError(t, os.WriteFile(filepath.Join(dst, "mysql", "row"), []byte("changed"), 0o640))
	still, err := os.ReadFile(row)
	require.NoError(t, err)
	assert.Equal(t, payload, still)

	linked, err := os.ReadFile(filepath.Join(dst, "rowlink"))
	require.NoError(t, err)
	assert.Equal(t, payload, linked)
	linkInfo, err := os.Lstat(filepath.Join(dst, "rowlink"))
	require.NoError(t, err)
	assert.Zero(t, linkInfo.Mode()&os.ModeSymlink)
}

func TestCopyDatadirClonesWhenTheDiskCan(t *testing.T) {
	src := t.TempDir()
	row := filepath.Join(src, "row")
	payload := bytes.Repeat([]byte("a"), 8192)
	require.NoError(t, os.WriteFile(row, payload, 0o644))
	dst := filepath.Join(t.TempDir(), "data")
	require.NoError(t, CopyDatadir(src, dst))
	copied := filepath.Join(dst, "row")
	if sharesClone(row, copied) {
		return
	}
	require.False(t, diskClones(src), "the disk can clone and the copy did not share storage")
}

func TestRunCopies(t *testing.T) {
	src := t.TempDir()
	payload := []byte("ibdata")
	require.NoError(t, os.WriteFile(filepath.Join(src, "ibdata1"), payload, 0o644))
	dst := filepath.Join(t.TempDir(), "data")
	require.NoError(t, Run([]string{src, dst}))
	got, err := os.ReadFile(filepath.Join(dst, "ibdata1"))
	require.NoError(t, err)
	assert.Equal(t, payload, got)
}

func TestRunNeedsTwoPaths(t *testing.T) {
	err := Run([]string{"only-one"})
	require.Error(t, err)
}
