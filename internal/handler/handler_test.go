package handler_test

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xakep666/ps3netsrv-go/internal/handler"
	"github.com/xakep666/ps3netsrv-go/internal/ioutil"
	"github.com/xakep666/ps3netsrv-go/pkg/client"
	pkgfs "github.com/xakep666/ps3netsrv-go/pkg/fs"
	"github.com/xakep666/ps3netsrv-go/pkg/server"
)

func startServer(t *testing.T, root string, allowWrite bool) *client.Client {
	t.Helper()
	return startServerWithFS(t, pkgfs.NewFS(pkgfs.NewRelaxedSystemRoot(root), nil, nil), allowWrite)
}

func startServerWithFS(t *testing.T, fsys *pkgfs.FS, allowWrite bool) *client.Client {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv := &server.Server[handler.State]{
		Handler: &handler.Handler{
			Fs:         fsys,
			Copier:     ioutil.NewPooledCopier(4096),
			AllowWrite: allowWrite,
		},
		Logger: slog.New(slog.DiscardHandler),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c, err := client.NewClient(t.Context(), ioutil.NewCopier(), ln.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	return c
}

func timeoutCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestReadFile(t *testing.T) {
	root := t.TempDir()
	content := bytes.Repeat([]byte("0123456789"), 1000)
	require.NoError(t, os.WriteFile(root+"/file.bin", content, 0o644))

	c := startServer(t, root, false)

	res, err := c.OpenFile(timeoutCtx(t), "/file.bin")
	require.NoError(t, err)
	require.EqualValues(t, len(content), res.FileSize)

	// huge limit must be clamped to file size
	var buf bytes.Buffer
	require.NoError(t, c.ReadFile(timeoutCtx(t), 0xFFFFFFFF, 10, &buf))
	require.Equal(t, content[10:], buf.Bytes())

	// read at EOF returns zero-length response, it must not hang the connection
	for _, offset := range []uint64{uint64(len(content)), 1 << 63} {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		err = c.ReadFile(ctx, 100, offset, &buf)
		cancel()
		require.ErrorContains(t, err, "unsuccessful response", "offset %d", offset)
	}

	buf.Reset()
	require.NoError(t, c.ReadFile(timeoutCtx(t), 5, 3, &buf))
	require.Equal(t, content[3:8], buf.Bytes())
}

func TestWriteForbiddenKeepsConnectionUsable(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(root+"/file.bin", []byte("data"), 0o644))

	c := startServer(t, root, false)

	require.Error(t, c.WriteFile(timeoutCtx(t), 16, bytes.NewReader(bytes.Repeat([]byte{1}, 100))))

	st, err := c.StatFile(timeoutCtx(t), "/file.bin")
	require.NoError(t, err)
	require.EqualValues(t, 4, st.FileSize)
}
