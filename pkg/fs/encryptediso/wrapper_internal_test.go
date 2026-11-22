package encryptediso

import (
	"bytes"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xakep666/ps3netsrv-go/internal/osutil/filesystem"
	pkgfs "github.com/xakep666/ps3netsrv-go/pkg/fs"
)

// TestRedumpKeyLookup checks that .dkey placed next to the image or in REDKEY directory
// is found for both relaxed and strict roots with absolute root path.
func TestRedumpKeyLookup(t *testing.T) {
	data1 := bytes.Repeat([]byte{0x3c}, encryptionKeySize)
	plain, encrypted := makeEncryptedImage(t, data1)

	for _, keyDir := range []string{"PS3ISO", "REDKEY"} {
		dir := t.TempDir()
		require.True(t, filepath.IsAbs(dir))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "PS3ISO"), 0o755))
		require.NoError(t, os.MkdirAll(filepath.Join(dir, keyDir), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "PS3ISO", "game.iso"), encrypted, 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(dir, keyDir, "game.dkey"), []byte(hex.EncodeToString(data1)), 0o644))

		osRoot, err := os.OpenRoot(dir)
		require.NoError(t, err)
		t.Cleanup(func() { _ = osRoot.Close() })

		for name, root := range map[string]pkgfs.SystemRoot{
			"relaxed": pkgfs.NewRelaxedSystemRoot(dir),
			"strict":  filesystem.NewStrictSystemRoot(osRoot),
		} {
			t.Run(keyDir+"/"+name, func(t *testing.T) {
				fsys := pkgfs.NewFS(root, nil, []pkgfs.FileWrapper{FileWrapper{}})

				f, err := fsys.Open(t.Context(), filepath.Join("PS3ISO", "game.iso"))
				require.NoError(t, err)
				t.Cleanup(func() { _ = f.Close() })

				got, err := io.ReadAll(f)
				require.NoError(t, err)
				require.True(t, bytes.Equal(plain, got), "image must be decrypted")
			})
		}
	}
}
