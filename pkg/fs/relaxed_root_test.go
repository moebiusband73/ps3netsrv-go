package fs_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	pkgfs "github.com/xakep666/ps3netsrv-go/pkg/fs"
)

func TestRelaxedSystemRootTraversal(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "ps3")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "PS3ISO"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "PS3ISO", "game.iso"), []byte("x"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "ps3_private"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ps3_private", "secret"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "outside"), []byte("x"), 0o644))

	r := pkgfs.NewRelaxedSystemRoot(root)

	for _, name := range []string{"/", "", "/PS3ISO", "/PS3ISO/game.iso", "/PS3ISO/../PS3ISO/game.iso"} {
		_, err := r.Stat(filepath.FromSlash(name))
		require.NoError(t, err, "path %q must be accessible", name)
	}

	for _, name := range []string{"../ps3_private/secret", "/../ps3_private/secret", "../outside", "/PS3ISO/../../outside", ".."} {
		_, err := r.Stat(filepath.FromSlash(name))
		require.True(t, errors.Is(err, os.ErrNotExist), "path %q must not escape root, got %v", name, err)
	}
}
