package seekablezstd_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	seekable "github.com/SaveTheRbtz/zstd-seekable-format-go/pkg"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"

	"github.com/xakep666/ps3netsrv-go/internal/handler"
	pkgfs "github.com/xakep666/ps3netsrv-go/pkg/fs"
	"github.com/xakep666/ps3netsrv-go/pkg/fs/seekablezstd"
)

func TestOpenReadClose(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte("seekable zstd content "), 1000)

	var buf bytes.Buffer
	enc, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	w, err := seekable.NewWriter(&buf, enc)
	require.NoError(t, err)
	_, err = w.Write(content)
	require.NoError(t, err)
	require.NoError(t, w.Close())
	require.NoError(t, os.WriteFile(filepath.Join(dir, "game.zst"), buf.Bytes(), 0o644))

	fsys := pkgfs.NewFS(pkgfs.NewRelaxedSystemRoot(dir), []pkgfs.FileOpener{seekablezstd.Opener{}}, nil)

	f, err := fsys.Open(t.Context(), "game.zst.iso")
	require.NoError(t, err)

	got, err := io.ReadAll(f)
	require.NoError(t, err)
	require.Equal(t, content, got)

	underlying, ok := handler.FileAsType[*pkgfs.OSFile](f)
	require.True(t, ok)

	require.NoError(t, f.Close())

	// underlying file must be closed too
	_, err = underlying.Read(make([]byte, 1))
	require.True(t, errors.Is(err, os.ErrClosed), "got %v", err)
}
