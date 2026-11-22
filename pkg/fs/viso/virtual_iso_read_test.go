package viso_test

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xakep666/ps3netsrv-go/internal/osutil/filesystem"
	pkgfs "github.com/xakep666/ps3netsrv-go/pkg/fs"
	"github.com/xakep666/ps3netsrv-go/pkg/fs/viso"
)

func readAllWithBuf(t *testing.T, r io.Reader, bufSize int) []byte {
	t.Helper()

	var got []byte
	buf := make([]byte, bufSize)
	for {
		n, err := r.Read(buf)
		got = append(got, buf[:n]...)
		if errors.Is(err, io.EOF) {
			return got
		}
		require.NoError(t, err)
		require.Positive(t, n)
	}
}

// TestReadBufferShapes checks that reads with arbitrary buffer sizes and offsets
// return the same data as one big read. It doesn't require mounting.
func TestReadBufferShapes(t *testing.T) {
	const isoRoot = "iso_root"

	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })

	rnd := rand.New(rand.NewPCG(5, 6))
	multiSector := make([]byte, 3123)
	for i := range multiSector {
		multiSector[i] = byte(rnd.Uint32())
	}

	require.NoError(t, root.Mkdir(isoRoot, os.ModePerm))
	require.NoError(t, root.WriteFile(filepath.Join(isoRoot, "a.txt"), bytes.Repeat([]byte("a"), 100), os.ModePerm))
	require.NoError(t, root.WriteFile(filepath.Join(isoRoot, "b_empty.txt"), nil, os.ModePerm))
	require.NoError(t, root.WriteFile(filepath.Join(isoRoot, "c.bin"), multiSector, os.ModePerm))
	require.NoError(t, root.WriteFile(filepath.Join(isoRoot, "d.txt"), []byte("d content"), os.ModePerm))

	v, err := viso.NewVirtualISO(t.Context(), pkgfs.NewFS(filesystem.NewStrictSystemRoot(root), nil, nil), isoRoot, false)
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.Close() })

	fi, err := v.Stat()
	require.NoError(t, err)
	require.Zero(t, fi.Size()%0x800)

	full := readAllWithBuf(t, v, int(fi.Size()))
	require.Len(t, full, int(fi.Size()))

	idx := bytes.Index(full, multiSector)
	require.Positive(t, idx, "file content must be present in image")
	require.Zero(t, idx%0x800, "file must be sector-aligned")
	require.Equal(t, make([]byte, 2*0x800-len(multiSector)), full[idx+len(multiSector):idx+2*0x800], "file padding must be zeroed")

	for _, bufSize := range []int{1, 100, 512, 2047, 2048, 2049, 5000} {
		for _, offset := range []int{0, idx - 1, idx, idx + 100, idx + 2047, idx + 3123, idx + 3200, len(full) - 1} {
			t.Run(fmt.Sprintf("buf=%d/off=%d", bufSize, offset), func(t *testing.T) {
				// reuse the same image: volume descriptors contain creation time
				_, err := v.Seek(int64(offset), io.SeekStart)
				require.NoError(t, err)

				require.Equal(t, full[offset:], readAllWithBuf(t, v, bufSize))
			})
		}
	}
}

func TestLongVolumeName(t *testing.T) {
	root, err := os.OpenRoot(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })

	isoRoot := "A Very Long Game Directory Name That Exceeds Volume Identifier Limits"
	require.NoError(t, root.Mkdir(isoRoot, os.ModePerm))
	require.NoError(t, root.WriteFile(filepath.Join(isoRoot, "a.txt"), []byte("a"), os.ModePerm))

	v, err := viso.NewVirtualISO(t.Context(), pkgfs.NewFS(filesystem.NewStrictSystemRoot(root), nil, nil), isoRoot, false)
	require.NoError(t, err)
	require.NoError(t, v.Close())
}
