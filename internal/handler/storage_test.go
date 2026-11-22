package handler_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"

	pkgfs "github.com/xakep666/ps3netsrv-go/pkg/fs"
	"github.com/xakep666/ps3netsrv-go/pkg/fs/cso"
	"github.com/xakep666/ps3netsrv-go/pkg/fs/encryptediso"
	"github.com/xakep666/ps3netsrv-go/pkg/fs/iso3k3y"
	"github.com/xakep666/ps3netsrv-go/pkg/fs/s3root"
	"github.com/xakep666/ps3netsrv-go/pkg/fs/seekablezstd"
	"github.com/xakep666/ps3netsrv-go/pkg/fs/viso"
)

const cdSectorSize = 2352

// storageFixture contains files served by every storage backend.
type storageFixture struct {
	files map[string][]byte // slash-separated path -> content
}

func newStorageFixture(t *testing.T) storageFixture {
	t.Helper()

	random := func(seed uint64, n int) []byte {
		rnd := rand.New(rand.NewPCG(seed, seed))
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(rnd.Uint32())
		}
		return b
	}

	files := map[string][]byte{
		"PS3ISO/game.iso":           random(1, 1<<20+77),
		"PSXISO/game.bin":           random(2, cdSectorSize*600),
		"GAMES/MYGAME/a.txt":        []byte("hello"),
		"GAMES/MYGAME/sub/data.bin": random(3, 5000),
		"outside_of_prefix.txt":     []byte("secret"), // must not be reachable from prefixed S3 root
	}

	// header-only CSO images: enough for directory listing
	for i := range 5 {
		hdr, err := binary.Append(nil, binary.LittleEndian, cso.Header{
			Magic:            cso.CISOMagic,
			HeaderSize:       cso.AdvisedHeaderSize,
			UncompressedSize: uint64(i+1) << 20,
			BlockSize:        2048,
			Version:          1,
		})
		require.NoError(t, err)
		files[fmt.Sprintf("CSO/game%d.cso", i)] = hdr
	}

	return storageFixture{files: files}
}

func newTestFS(root pkgfs.SystemRoot) *pkgfs.FS {
	return pkgfs.NewFS(root,
		[]pkgfs.FileOpener{viso.Opener{}, cso.Opener{}, seekablezstd.Opener{}},
		[]pkgfs.FileWrapper{iso3k3y.KeyExtractionFileWrapper{}, encryptediso.FileWrapper{}, iso3k3y.FileWrapper{}},
	)
}

func (f storageFixture) localRoot(t *testing.T) pkgfs.SystemRoot {
	t.Helper()

	dir := t.TempDir()
	served := filepath.Join(dir, "lib")
	for name, data := range f.files {
		target := filepath.Join(served, filepath.FromSlash(name))
		if name == "outside_of_prefix.txt" {
			target = filepath.Join(dir, name)
		}
		require.NoError(t, os.MkdirAll(filepath.Dir(target), 0o755))
		require.NoError(t, os.WriteFile(target, data, 0o644))
	}

	return pkgfs.NewRelaxedSystemRoot(served)
}

type countingTransport struct {
	mu     sync.Mutex
	counts map[string]int
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.counts[req.Method]++
	t.mu.Unlock()
	return http.DefaultTransport.RoundTrip(req)
}

func (t *countingTransport) take() map[string]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	ret := t.counts
	t.counts = map[string]int{}
	return ret
}

func (f storageFixture) s3Root(t *testing.T) (*s3root.Root, *countingTransport) {
	t.Helper()

	backend := s3mem.New()
	srv := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(srv.Close)

	const bucket = "games"
	require.NoError(t, backend.CreateBucket(bucket))
	for name, data := range f.files {
		key := "lib/" + name
		if name == "outside_of_prefix.txt" {
			key = name
		}
		meta := map[string]string{"Last-Modified": time.Now().UTC().Format(http.TimeFormat)}
		_, err := backend.PutObject(bucket, key, meta, bytes.NewReader(data), int64(len(data)), nil)
		require.NoError(t, err)
	}

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	transport := &countingTransport{counts: map[string]int{}}
	root, err := s3root.New(s3root.Config{
		Endpoint:     u.Host,
		BucketLookup: minio.BucketLookupPath,
		Bucket:       bucket,
		Prefix:       "lib",
		Creds:        credentials.NewStaticV4("key", "secret", ""),
		Transport:    transport,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = root.Close() })
	require.NoError(t, root.Check(t.Context()))

	return root, transport
}

func TestStorageBackends(t *testing.T) {
	fixture := newStorageFixture(t)
	s3, transport := fixture.s3Root(t)

	for name, root := range map[string]pkgfs.SystemRoot{
		"local": fixture.localRoot(t),
		"s3":    s3,
	} {
		t.Run(name, func(t *testing.T) {
			c := startServerWithFS(t, newTestFS(root), false)

			t.Run("read file", func(t *testing.T) {
				data := fixture.files["PS3ISO/game.iso"]
				res, err := c.OpenFile(timeoutCtx(t), "/PS3ISO/game.iso")
				require.NoError(t, err)
				require.EqualValues(t, len(data), res.FileSize)

				var buf bytes.Buffer
				for off := 0; off < len(data); off += 64 << 10 {
					require.NoError(t, c.ReadFileCritical(timeoutCtx(t), uint32(min(64<<10, len(data)-off)), uint64(off), &buf))
				}
				require.True(t, bytes.Equal(data, buf.Bytes()))

				buf.Reset()
				require.NoError(t, c.ReadFile(timeoutCtx(t), 3000, 12345, &buf))
				require.Equal(t, data[12345:12345+3000], buf.Bytes())
			})

			t.Run("read cd2048", func(t *testing.T) {
				data := fixture.files["PSXISO/game.bin"]
				_, err := c.OpenFile(timeoutCtx(t), "/PSXISO/game.bin")
				require.NoError(t, err)

				var buf bytes.Buffer
				require.NoError(t, c.ReadCD2048Critical(timeoutCtx(t), 50, 16, &buf))
				for i := range 50 {
					sector := 16 + i
					expected := data[24+sector*cdSectorSize : 24+sector*cdSectorSize+2048]
					require.Equal(t, expected, buf.Bytes()[i*2048:(i+1)*2048], "sector %d", sector)
				}
			})

			t.Run("list compressed images", func(t *testing.T) {
				if name == "s3" {
					transport.take()
				}

				require.NoError(t, c.OpenDir(timeoutCtx(t), "/CSO"))
				var names []string
				for {
					entry, err := c.ReadDirEntry(timeoutCtx(t))
					if errors.Is(err, io.EOF) {
						break
					}
					require.NoError(t, err)
					names = append(names, fmt.Sprintf("%s:%d", entry.Name, entry.FileSize))
				}
				require.ElementsMatch(t, []string{
					"game0.cso.iso:1048576", "game1.cso.iso:2097152", "game2.cso.iso:3145728",
					"game3.cso.iso:4194304", "game4.cso.iso:5242880",
				}, names)

				if name == "s3" {
					// Directory itself is resolved with HEAD (404) + LIST(max-keys=1) because its parent wasn't listed.
					// Then one listing and one header page per image, no per-entry HEAD requests.
					counts := transport.take()
					require.LessOrEqual(t, counts[http.MethodHead], 1, "%v", counts)
					require.LessOrEqual(t, counts[http.MethodGet], 2+5, "%v", counts)
				}
			})

			t.Run("virtual iso", func(t *testing.T) {
				res, err := c.OpenFile(timeoutCtx(t), "/***DVD***/GAMES/MYGAME")
				require.NoError(t, err)
				require.Zero(t, res.FileSize%2048)

				var buf bytes.Buffer
				require.NoError(t, c.ReadFile(timeoutCtx(t), uint32(res.FileSize), 0, &buf))

				content := fixture.files["GAMES/MYGAME/sub/data.bin"]
				idx := bytes.Index(buf.Bytes(), content)
				require.Positive(t, idx)
				require.Zero(t, idx%2048)
				require.Contains(t, buf.String(), "hello")
			})

			t.Run("root escape", func(t *testing.T) {
				_, err := c.StatFile(timeoutCtx(t), "/../outside_of_prefix.txt")
				require.Error(t, err)
			})

			t.Run("write forbidden keeps connection", func(t *testing.T) {
				require.Error(t, c.CreateFile(timeoutCtx(t), "/new.bin"))
				_, err := c.StatFile(timeoutCtx(t), "/PS3ISO/game.iso")
				require.NoError(t, err)
			})
		})
	}
}

func TestStorageS3ReadOnlyEvenIfWriteAllowed(t *testing.T) {
	fixture := newStorageFixture(t)
	s3, _ := fixture.s3Root(t)

	c := startServerWithFS(t, newTestFS(s3), true)
	require.Error(t, c.CreateFile(timeoutCtx(t), "/new.bin"))
	require.Error(t, c.MkDir(timeoutCtx(t), "/newdir"))
	require.Error(t, c.DeleteFile(timeoutCtx(t), "/PS3ISO/game.iso"))

	st, err := c.StatFile(timeoutCtx(t), "/PS3ISO/game.iso")
	require.NoError(t, err)
	require.False(t, strings.HasPrefix(fmt.Sprint(st.FileSize), "-"))
}
