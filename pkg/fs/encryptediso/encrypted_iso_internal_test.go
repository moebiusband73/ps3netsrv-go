package encryptediso

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xakep666/ps3netsrv-go/internal/iso9660"
)

type memFile struct {
	*bytes.Reader
}

func (*memFile) Stat() (fs.FileInfo, error)         { return nil, errors.ErrUnsupported }
func (*memFile) Close() error                       { return nil }
func (*memFile) ReadDir(int) ([]fs.DirEntry, error) { return nil, errors.ErrUnsupported }
func (*memFile) Name() string                       { return "test.iso" }

// makeEncryptedImage returns plain and encrypted images with layout:
// sectors [0;1) unencrypted (contains region map), [1;3) encrypted, [3;5) unencrypted.
func makeEncryptedImage(t *testing.T, data1 []byte) (plain, encrypted []byte) {
	t.Helper()

	const (
		sectors    = 5
		sectorSize = int(iso9660.SectorSize)
	)
	rnd := rand.New(rand.NewPCG(3, 4))
	plain = make([]byte, sectors*sectorSize)
	for i := range plain {
		plain[i] = byte(rnd.Uint32())
	}

	hdr, err := binary.Append(nil, binary.BigEndian, unencryptedRegionsHeader{Count: 2})
	require.NoError(t, err)
	hdr, err = binary.Append(hdr, binary.BigEndian, []unencryptedRegion{{Start: 0, End: 1}, {Start: 3, End: sectors}})
	require.NoError(t, err)
	copy(plain, hdr)

	var key [encryptionKeySize]byte
	require.NoError(t, deriveISOKey(key[:], data1))
	cip, err := aes.NewCipher(key[:])
	require.NoError(t, err)

	encrypted = bytes.Clone(plain)
	for sector := 1; sector < 3; sector++ {
		var iv [encryptionKeySize]byte
		binary.BigEndian.PutUint32(iv[len(iv)-4:], uint32(sector))
		span := encrypted[sector*sectorSize : (sector+1)*sectorSize]
		cipher.NewCBCEncrypter(cip, iv[:]).CryptBlocks(span, span)
	}

	return plain, encrypted
}

func TestEncryptedISORead(t *testing.T) {
	data1 := bytes.Repeat([]byte{0x5a}, encryptionKeySize)
	plain, encrypted := makeEncryptedImage(t, data1)
	require.NotEqual(t, plain, encrypted)

	for _, bufSize := range []int{1, 512, 2047, 2048, 2049, 4096, 6000, len(plain) + 10} {
		for _, offset := range []int{0, 100, 2048, 2048 + 512, 4095, 4096, 6143, 6144, len(plain) - 1} {
			t.Run(fmt.Sprintf("buf=%d/off=%d", bufSize, offset), func(t *testing.T) {
				iso, err := NewEncryptedISO(&memFile{Reader: bytes.NewReader(encrypted)}, data1, false)
				require.NoError(t, err)

				_, err = iso.Seek(int64(offset), io.SeekStart)
				require.NoError(t, err)

				var got []byte
				buf := make([]byte, bufSize)
				for {
					n, err := iso.Read(buf)
					got = append(got, buf[:n]...)
					if errors.Is(err, io.EOF) {
						break
					}
					require.NoError(t, err)
					require.Positive(t, n)
				}

				require.Equal(t, plain[offset:], got)
			})
		}
	}
}

func TestNewEncryptedISORejectsHugeRegionCount(t *testing.T) {
	hdr, err := binary.Append(nil, binary.BigEndian, unencryptedRegionsHeader{Count: 1 << 30})
	require.NoError(t, err)

	_, err = NewEncryptedISO(&memFile{Reader: bytes.NewReader(hdr)}, make([]byte, encryptionKeySize), false)
	require.Error(t, err)
}
