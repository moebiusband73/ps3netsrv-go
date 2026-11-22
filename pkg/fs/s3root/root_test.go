package s3root

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/require"
)

const testBucket = "games"

// countingTransport counts requests by method.
type countingTransport struct {
	base http.RoundTripper

	mu     sync.Mutex
	counts map[string]int
}

func (t *countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.counts[req.Method]++
	t.mu.Unlock()
	return t.base.RoundTrip(req)
}

func (t *countingTransport) count(method string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counts[method]
}

func (t *countingTransport) reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	clear(t.counts)
}

type testEnv struct {
	backend   *s3mem.Backend
	server    *httptest.Server
	client    *minio.Client
	transport *countingTransport
	endpoint  string
}

// newTestEnv starts in-process S3 server. middleware may intercept requests.
func newTestEnv(t *testing.T, middleware func(http.Handler) http.Handler) *testEnv {
	t.Helper()

	backend := s3mem.New()
	var h http.Handler = gofakes3.New(backend).Server()
	if middleware != nil {
		h = middleware(h)
	}

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	require.NoError(t, err)

	client, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4("key", "secret", ""),
		BucketLookup: minio.BucketLookupPath,
	})
	require.NoError(t, err)
	require.NoError(t, client.MakeBucket(t.Context(), testBucket, minio.MakeBucketOptions{}))

	return &testEnv{
		backend:   backend,
		server:    srv,
		client:    client,
		transport: &countingTransport{base: http.DefaultTransport, counts: map[string]int{}},
		endpoint:  u.Host,
	}
}

func (e *testEnv) put(t *testing.T, key string, data []byte) {
	t.Helper()
	meta := map[string]string{"Last-Modified": time.Now().UTC().Format(http.TimeFormat)}
	_, err := e.backend.PutObject(testBucket, key, meta, bytes.NewReader(data), int64(len(data)), nil)
	require.NoError(t, err)
}

func (e *testEnv) root(t *testing.T, prefix string, tune func(*Config)) *Root {
	t.Helper()

	cfg := Config{
		Endpoint:     e.endpoint,
		BucketLookup: minio.BucketLookupPath,
		Bucket:       testBucket,
		Prefix:       prefix,
		Creds:        credentials.NewStaticV4("key", "secret", ""),
		Transport:    e.transport,
		PageSize:     4 << 10,
		ReadaheadMin: 16 << 10,
		ReadaheadMax: 64 << 10,
		CacheSize:    1 << 20,
	}
	if tune != nil {
		tune(&cfg)
	}

	r, err := New(cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close() })
	require.NoError(t, r.Check(t.Context()))
	e.transport.reset()

	return r
}

func randomBytes(seed uint64, n int) []byte {
	rnd := rand.New(rand.NewPCG(seed, seed))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(rnd.Uint32())
	}
	return b
}

func TestParseURL(t *testing.T) {
	bucket, prefix, err := ParseURL("s3://games/ps3/collection/")
	require.NoError(t, err)
	require.Equal(t, "games", bucket)
	require.Equal(t, "ps3/collection", prefix)

	bucket, prefix, err = ParseURL("s3://games")
	require.NoError(t, err)
	require.Equal(t, "games", bucket)
	require.Empty(t, prefix)

	_, _, err = ParseURL("http://games")
	require.Error(t, err)
	_, _, err = ParseURL("s3:///prefix")
	require.Error(t, err)
}

func TestKeyMapping(t *testing.T) {
	for _, tc := range []struct {
		prefix, name, key string
	}{
		{"", "", ""},
		{"", ".", ""},
		{"", "PS3ISO/game.iso", "PS3ISO/game.iso"},
		{"", "/PS3ISO/game.iso", "PS3ISO/game.iso"},
		{"", "../secret", "secret"},
		{"", "a/../../b", "b"},
		{"games", "", "games"},
		{"/games/", "..", "games"},
		{"games", "PS3ISO", "games/PS3ISO"},
		{"games", "../other/x", "games/other/x"},
	} {
		r := &Root{prefix: strings.Trim(tc.prefix, "/")}
		if r.prefix != "" {
			r.prefix += "/"
		}

		key, err := r.key(filepath.FromSlash(tc.name))
		require.NoError(t, err)
		require.Equal(t, tc.key, key, "prefix %q name %q", tc.prefix, tc.name)
	}
}

func TestStatAndReadDir(t *testing.T) {
	env := newTestEnv(t, nil)
	env.put(t, "lib/PS3ISO/a.iso", []byte("aaaa"))
	env.put(t, "lib/PS3ISO/b.iso", []byte("bb"))
	env.put(t, "lib/PS3ISO/sub/c.iso", []byte("c"))
	env.put(t, "lib/EMPTY/", nil) // directory marker
	env.put(t, "lib/top.txt", []byte("top"))
	env.put(t, "outside.txt", []byte("x"))

	r := env.root(t, "lib", nil)

	t.Run("stat", func(t *testing.T) {
		for name, isDir := range map[string]bool{
			"":                 false,
			"PS3ISO":           true,
			"PS3ISO/sub":       true,
			"EMPTY":            true,
			"top.txt":          false,
			"PS3ISO/a.iso":     false,
			"PS3ISO/sub/c.iso": false,
		} {
			fi, err := r.Stat(filepath.FromSlash(name))
			require.NoError(t, err, name)
			require.Equal(t, isDir || name == "", fi.IsDir(), name)
		}

		fi, err := r.Stat(filepath.FromSlash("PS3ISO/a.iso"))
		require.NoError(t, err)
		require.EqualValues(t, 4, fi.Size())
		require.Equal(t, "a.iso", fi.Name())

		for _, name := range []string{"missing", "PS3ISO/missing.iso", "../outside.txt", "PS3IS"} {
			_, err = r.Stat(filepath.FromSlash(name))
			require.ErrorIs(t, err, fs.ErrNotExist, name)
		}
	})

	t.Run("readdir all", func(t *testing.T) {
		d, err := r.Open("")
		require.NoError(t, err)
		defer d.Close()

		entries, err := d.ReadDir(-1)
		require.NoError(t, err)
		require.Equal(t, []string{"EMPTY/", "PS3ISO/", "top.txt"}, entryNames(entries))

		entries, err = d.ReadDir(-1)
		require.NoError(t, err)
		require.Empty(t, entries)
	})

	t.Run("readdir incremental and cached stat", func(t *testing.T) {
		d, err := r.Open("PS3ISO")
		require.NoError(t, err)
		defer d.Close()

		var all []fs.DirEntry
		for {
			entries, err := d.ReadDir(1)
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
			require.Len(t, entries, 1)
			all = append(all, entries...)
		}
		require.Equal(t, []string{"a.iso", "b.iso", "sub/"}, entryNames(all))

		info, err := all[1].Info()
		require.NoError(t, err)
		require.EqualValues(t, 2, info.Size())

		// stats of listed entries and missing siblings are served from cache
		env.transport.reset()
		for _, name := range []string{"PS3ISO/a.iso", "PS3ISO/b.iso", "PS3ISO/sub"} {
			_, err := r.Stat(filepath.FromSlash(name))
			require.NoError(t, err)
		}
		_, err = r.Stat(filepath.FromSlash("PS3ISO/a.dkey"))
		require.ErrorIs(t, err, fs.ErrNotExist)
		require.Zero(t, env.transport.count(http.MethodHead)+env.transport.count(http.MethodGet))
	})
}

func entryNames(entries []fs.DirEntry) []string {
	var names []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestReadDirPagination(t *testing.T) {
	env := newTestEnv(t, nil)
	const count = 1100
	for i := range count {
		env.put(t, fmt.Sprintf("many/f%04d.iso", i), []byte{byte(i)})
	}

	r := env.root(t, "", nil)
	d, err := r.Open("many")
	require.NoError(t, err)
	defer d.Close()

	entries, err := d.ReadDir(-1)
	require.NoError(t, err)
	require.Len(t, entries, count)
}

func TestReadOnly(t *testing.T) {
	env := newTestEnv(t, nil)
	r := env.root(t, "", nil)

	_, err := r.Create("x")
	require.ErrorIs(t, err, fs.ErrPermission)
	require.ErrorIs(t, r.Remove("x"), fs.ErrPermission)
	require.ErrorIs(t, r.Mkdir("x", 0o755), fs.ErrPermission)
}

func TestFileRead(t *testing.T) {
	env := newTestEnv(t, nil)
	data := randomBytes(1, 300<<10+123) // not aligned to page size
	env.put(t, "img/game.iso", data)

	r := env.root(t, "", nil)

	t.Run("sequential with various buffers", func(t *testing.T) {
		for _, bufSize := range []int{1, 100, 2048, 4096, 5000, 64 << 10, len(data) + 1} {
			f, err := r.Open(filepath.FromSlash("img/game.iso"))
			require.NoError(t, err)

			got, err := io.ReadAll(io.LimitReader(readerFunc(func(b []byte) (int, error) {
				return f.Read(b[:min(len(b), bufSize)])
			}), int64(len(data)+10)))
			require.NoError(t, err)
			require.True(t, bytes.Equal(data, got), "buf %d", bufSize)
			require.NoError(t, f.Close())
		}
	})

	t.Run("seek and read at", func(t *testing.T) {
		f, err := r.Open(filepath.FromSlash("img/game.iso"))
		require.NoError(t, err)
		defer f.Close()

		ra := f.(io.ReaderAt)
		for _, off := range []int{0, 1, 4095, 4096, 100_000, len(data) - 10} {
			buf := make([]byte, 5000)
			n, err := ra.ReadAt(buf, int64(off))
			expected := data[off:min(off+len(buf), len(data))]
			require.Equal(t, len(expected), n)
			require.Equal(t, expected, buf[:n])
			if n < len(buf) {
				require.ErrorIs(t, err, io.EOF)
			} else {
				require.NoError(t, err)
			}

			pos, err := f.Seek(int64(off), io.SeekStart)
			require.NoError(t, err)
			require.EqualValues(t, off, pos)
			n, err = f.Read(buf[:10])
			require.NoError(t, err)
			require.Equal(t, data[off:off+n], buf[:n])
		}

		end, err := f.Seek(0, io.SeekEnd)
		require.NoError(t, err)
		require.EqualValues(t, len(data), end)
		_, err = f.Read(make([]byte, 1))
		require.ErrorIs(t, err, io.EOF)
	})

	t.Run("sequential read uses readahead", func(t *testing.T) {
		r := env.root(t, "", nil) // fresh cache
		f, err := r.Open(filepath.FromSlash("img/game.iso"))
		require.NoError(t, err)
		defer f.Close()

		env.transport.reset()
		buf := make([]byte, 2048)
		for {
			_, err := f.Read(buf)
			if errors.Is(err, io.EOF) {
				break
			}
			require.NoError(t, err)
		}

		// 151 reads of 2048 bytes, page size 4k, max window 64k: about size/window requests
		gets := env.transport.count(http.MethodGet)
		require.LessOrEqual(t, gets, len(data)/(64<<10)+8, "too many requests: %d", gets)
	})

	t.Run("cd sectors strided read is sequential", func(t *testing.T) {
		r := env.root(t, "", nil) // fresh cache
		f, err := r.Open(filepath.FromSlash("img/game.iso"))
		require.NoError(t, err)
		defer f.Close()

		env.transport.reset()
		buf := make([]byte, 2048)
		for off := int64(24); off+2048 <= int64(len(data)); off += 2352 {
			_, err := f.Seek(off, io.SeekStart)
			require.NoError(t, err)
			_, err = io.ReadFull(f, buf)
			require.NoError(t, err)
			require.Equal(t, data[off:off+2048], buf)
		}

		gets := env.transport.count(http.MethodGet)
		require.LessOrEqual(t, gets, len(data)/(64<<10)+8, "too many requests: %d", gets)
	})
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(b []byte) (int, error) { return f(b) }

func TestObjectChanged(t *testing.T) {
	env := newTestEnv(t, nil)
	env.put(t, "game.iso", randomBytes(2, 64<<10))

	r := env.root(t, "", nil)
	f, err := r.Open("game.iso")
	require.NoError(t, err)
	defer f.Close()

	buf := make([]byte, 100)
	_, err = f.Read(buf)
	require.NoError(t, err)

	// replace object, pages of new version must not be mixed with old ones
	env.put(t, "game.iso", randomBytes(3, 64<<10))

	_, err = f.Seek(40<<10, io.SeekStart)
	require.NoError(t, err)
	_, err = f.Read(buf)
	require.ErrorIs(t, err, ErrObjectChanged)

	// metadata invalidated: new open sees new version
	f2, err := r.Open("game.iso")
	require.NoError(t, err)
	defer f2.Close()
	_, err = f2.Read(buf)
	require.NoError(t, err)
}

// blockingBodies makes GET object responses send headers and then hang until unblocked or client gives up.
func blockingBodies(unblock <-chan struct{}) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Method != http.MethodGet || req.Header.Get("Range") == "" {
				next.ServeHTTP(w, req)
				return
			}

			// send real headers but no body
			rec := httptest.NewRecorder()
			next.ServeHTTP(rec, req)
			for k, v := range rec.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(rec.Code)
			w.(http.Flusher).Flush()

			select {
			case <-unblock:
			case <-req.Context().Done():
			}
		})
	}
}

func TestStalledResponse(t *testing.T) {
	unblock := make(chan struct{})
	defer close(unblock)

	env := newTestEnv(t, blockingBodies(unblock))
	env.put(t, "game.iso", randomBytes(4, 64<<10))

	r := env.root(t, "", func(c *Config) { c.RequestTimeout = 300 * time.Millisecond })
	f, err := r.Open("game.iso")
	require.NoError(t, err)
	defer f.Close()

	start := time.Now()
	_, err = f.Read(make([]byte, 100))
	require.ErrorIs(t, err, errStalled)
	require.Less(t, time.Since(start), 5*time.Second)
}

func TestCloseCancelsReads(t *testing.T) {
	unblock := make(chan struct{})
	defer close(unblock)

	env := newTestEnv(t, blockingBodies(unblock))
	env.put(t, "game.iso", randomBytes(5, 64<<10))

	r := env.root(t, "", func(c *Config) { c.RequestTimeout = time.Minute })
	f, err := r.Open("game.iso")
	require.NoError(t, err)

	errc := make(chan error, 1)
	go func() {
		_, err := f.Read(make([]byte, 100))
		errc <- err
	}()

	time.Sleep(100 * time.Millisecond)
	require.NoError(t, r.Close())

	select {
	case err := <-errc:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("read was not cancelled by root close")
	}
}

func TestConcurrentReaders(t *testing.T) {
	env := newTestEnv(t, nil)
	data := randomBytes(6, 200<<10)
	env.put(t, "game.iso", data)

	r := env.root(t, "", nil)

	var (
		wg     sync.WaitGroup
		failed atomic.Bool
	)
	for i := range 8 {
		wg.Go(func() {
			f, err := r.Open("game.iso")
			if err != nil {
				failed.Store(true)
				return
			}
			defer f.Close()

			// some readers close early to cancel shared downloads
			limit := int64(len(data))
			if i%2 == 1 {
				limit = int64(len(data) / 3)
			}

			got, err := io.ReadAll(io.LimitReader(f, limit))
			if err != nil || !bytes.Equal(data[:limit], got) {
				failed.Store(true)
			}
		})
	}
	wg.Wait()
	require.False(t, failed.Load())

	pages, used := r.pages.stats()
	require.LessOrEqual(t, used, r.cfg.CacheSize)
	require.Positive(t, pages)
}
