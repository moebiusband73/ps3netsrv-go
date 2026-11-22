package cso_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"testing"

	"github.com/klauspost/compress/flate"
	"github.com/pierrec/lz4/v4"
	"github.com/stretchr/testify/require"

	"github.com/xakep666/ps3netsrv-go/pkg/fs/cso"
)

type memFile struct {
	*bytes.Reader
}

func (*memFile) Stat() (fs.FileInfo, error)         { return nil, errors.ErrUnsupported }
func (*memFile) Close() error                       { return nil }
func (*memFile) ReadDir(int) ([]fs.DirEntry, error) { return nil, errors.ErrUnsupported }
func (*memFile) Name() string                       { return "test.cso" }

// makeTestData produces data where every odd block is incompressible
// and the last block is partial.
func makeTestData(blockSize, fullBlocks, tail int) []byte {
	rnd := rand.New(rand.NewPCG(1, 2))
	data := make([]byte, blockSize*fullBlocks+tail)
	for i := range data {
		if (i/blockSize)%2 == 1 {
			data[i] = byte(rnd.Uint32())
		} else {
			data[i] = byte(i % 7)
		}
	}
	return data
}

type compressFunc func(block []byte) (compressed []byte, ok bool)

func flateCompress(block []byte) ([]byte, bool) {
	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		panic(err)
	}
	_, _ = w.Write(block)
	_ = w.Close()
	return buf.Bytes(), buf.Len() < len(block)
}

func lz4Compress(block []byte) ([]byte, bool) {
	dst := make([]byte, lz4.CompressBlockBound(len(block)))
	n, err := lz4.CompressBlock(block, dst, nil)
	if err != nil || n == 0 || n >= len(block) {
		return nil, false
	}
	return dst[:n], true
}

// buildImage builds CSOv1 or ZSO image: top bit set means stored block in both variants.
func buildImage(t *testing.T, magic [4]byte, blockSize uint32, data []byte, compress compressFunc) []byte {
	t.Helper()

	hdr := cso.Header{
		Magic:            magic,
		HeaderSize:       cso.AdvisedHeaderSize,
		UncompressedSize: uint64(len(data)),
		BlockSize:        blockSize,
		Version:          1,
	}
	blocks := hdr.BlocksCount()

	var payload bytes.Buffer
	index := make([]uint32, blocks+1)
	dataStart := uint32(binary.Size(hdr) + 4*len(index))
	for i := range blocks {
		block := data[i*int(blockSize) : min((i+1)*int(blockSize), len(data))]
		index[i] = dataStart + uint32(payload.Len())
		if compressed, ok := compress(block); ok {
			payload.Write(compressed)
		} else {
			index[i] |= 1 << 31
			payload.Write(block)
		}
	}
	index[blocks] = dataStart + uint32(payload.Len())

	out, err := binary.Append(nil, binary.LittleEndian, hdr)
	require.NoError(t, err)
	out, err = binary.Append(out, binary.LittleEndian, index)
	require.NoError(t, err)
	return append(out, payload.Bytes()...)
}

func TestFileRead(t *testing.T) {
	const blockSize = 2048
	data := makeTestData(blockSize, 7, 100)

	for _, variant := range []struct {
		name     string
		magic    [4]byte
		compress compressFunc
	}{
		{"CSOv1", cso.CISOMagic, flateCompress},
		{"ZSO", cso.ZISOMagic, lz4Compress},
	} {
		image := buildImage(t, variant.magic, blockSize, data, variant.compress)

		for _, bufSize := range []int{1, 100, 512, 2047, 2048, 2049, 5000, len(data) + 10} {
			for _, offset := range []int{0, 1, 2047, 2048, 3000, len(data) - 50} {
				t.Run(fmt.Sprintf("%s/buf=%d/off=%d", variant.name, bufSize, offset), func(t *testing.T) {
					f, err := cso.NewFile(&memFile{Reader: bytes.NewReader(image)})
					require.NoError(t, err)

					_, err = f.Seek(int64(offset), io.SeekStart)
					require.NoError(t, err)

					var got []byte
					buf := make([]byte, bufSize)
					for {
						n, err := f.Read(buf)
						got = append(got, buf[:n]...)
						if errors.Is(err, io.EOF) {
							break
						}
						require.NoError(t, err)
						require.Positive(t, n)
					}

					require.Equal(t, data[offset:], got)
				})
			}
		}
	}
}

func TestNewFileRejectsBadHeader(t *testing.T) {
	for _, hdr := range []cso.Header{
		{Magic: cso.CISOMagic, Version: 1, HeaderSize: cso.AdvisedHeaderSize, UncompressedSize: 4096, BlockSize: 0},
		{Magic: cso.CISOMagic, Version: 1, HeaderSize: cso.AdvisedHeaderSize, UncompressedSize: 4096, BlockSize: 1 << 31},
		{Magic: cso.CISOMagic, Version: 1, HeaderSize: cso.AdvisedHeaderSize, UncompressedSize: 4096, BlockSize: 2048, IndexShift: 40},
	} {
		raw, err := binary.Append(nil, binary.LittleEndian, hdr)
		require.NoError(t, err)

		_, err = cso.NewFile(&memFile{Reader: bytes.NewReader(raw)})
		require.Error(t, err, "header %+v", hdr)
	}
}
