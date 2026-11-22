package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMakeListenerUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix sockets with permissions are not supported on windows")
	}

	dir := t.TempDir()

	t.Run("regular file is not removed", func(t *testing.T) {
		path := filepath.Join(dir, "regular")
		require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

		_, err := makeListener("unix:" + path)
		require.Error(t, err)

		content, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "data", string(content))
	})

	t.Run("stale socket is replaced", func(t *testing.T) {
		path := filepath.Join(dir, "s.sock")

		lis, err := makeListener("unix:" + path + ",600")
		require.NoError(t, err)
		st, err := os.Stat(path)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), st.Mode().Perm())

		// emulate unclean shutdown: socket file is left behind
		if ul, ok := lis.(interface{ SetUnlinkOnClose(bool) }); ok {
			ul.SetUnlinkOnClose(false)
		}
		require.NoError(t, lis.Close())

		lis, err = makeListener("unix:" + path)
		require.NoError(t, err)
		require.NoError(t, lis.Close())
	})

	t.Run("invalid permissions", func(t *testing.T) {
		_, err := makeListener("unix:" + filepath.Join(dir, "p.sock") + ",rw")
		require.Error(t, err)
	})
}
