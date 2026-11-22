package s3root

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
)

var errStalled = errors.New("s3 response stalled")

// pageKey identifies cached page. ETag prevents mixing data of different object versions.
type pageKey struct {
	key  string
	etag string
	idx  int64
}

// pageCall represents in-flight page download. data and err are valid after done is closed.
type pageCall struct {
	done chan struct{}
	data []byte
	err  error
}

// pageCache is an LRU cache of object pages shared between all open files.
// It also deduplicates concurrent downloads of the same page.
type pageCache struct {
	capacity int64

	mu       sync.Mutex
	used     int64
	lru      *list.List // of *cachedPage, front is the most recently used
	items    map[pageKey]*list.Element
	inflight map[pageKey]*pageCall
}

type cachedPage struct {
	key  pageKey
	data []byte
}

func newPageCache(capacity int64) *pageCache {
	return &pageCache{
		capacity: capacity,
		lru:      list.New(),
		items:    make(map[pageKey]*list.Element),
		inflight: make(map[pageKey]*pageCall),
	}
}

// lookup returns cached data or in-flight call for the page.
// If neither exists a new call is registered and owner is true: caller must download the page and call complete.
func (c *pageCache) lookup(k pageKey) (data []byte, call *pageCall, owner bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[k]; ok {
		c.lru.MoveToFront(el)
		return el.Value.(*cachedPage).data, nil, false
	}

	if call, ok := c.inflight[k]; ok {
		return nil, call, false
	}

	call = &pageCall{done: make(chan struct{})}
	c.inflight[k] = call
	return nil, call, true
}

// complete finishes the call, successfully downloaded data is put to cache.
func (c *pageCache) complete(k pageKey, call *pageCall, data []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	call.data, call.err = data, err
	close(call.done)
	delete(c.inflight, k)

	if err != nil {
		return
	}

	if el, ok := c.items[k]; ok { // shouldn't happen, but keep accounting consistent
		c.used -= int64(len(el.Value.(*cachedPage).data))
		c.lru.Remove(el)
	}

	c.items[k] = c.lru.PushFront(&cachedPage{key: k, data: data})
	c.used += int64(len(data))

	for c.used > c.capacity && c.lru.Len() > 1 {
		el := c.lru.Back()
		page := el.Value.(*cachedPage)
		c.lru.Remove(el)
		delete(c.items, page.key)
		c.used -= int64(len(page.data))
	}
}

func (c *pageCache) stats() (pages int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.lru.Len(), c.used
}

func (r *Root) pageKey(obj objInfo, idx int64) pageKey {
	return pageKey{key: obj.key, etag: obj.etag, idx: idx}
}

func (r *Root) pagesCount(obj objInfo) int64 {
	return (obj.size + r.cfg.PageSize - 1) / r.cfg.PageSize
}

// fetch starts background downloads of pages [from, to] which are neither cached nor in flight.
// Contiguous missing pages are downloaded with a single ranged request.
func (r *Root) fetch(ctx context.Context, obj objInfo, from, to int64) {
	to = min(to, r.pagesCount(obj)-1)

	var (
		runStart int64
		run      []*pageCall
	)
	flush := func() {
		if len(run) > 0 {
			go r.download(ctx, obj, runStart, run)
			run = nil
		}
	}

	for idx := from; idx <= to; idx++ {
		_, call, owner := r.pages.lookup(r.pageKey(obj, idx))
		if !owner {
			flush()
			continue
		}

		if len(run) == 0 {
			runStart = idx
		}
		run = append(run, call)
	}

	flush()
}

// download fetches pages starting from first and completes calls one by one as data arrives.
func (r *Root) download(ctx context.Context, obj objInfo, first int64, calls []*pageCall) {
	completeFrom := func(i int, err error) {
		for j := i; j < len(calls); j++ {
			r.pages.complete(r.pageKey(obj, first+int64(j)), calls[j], nil, err)
		}
	}

	if err := r.acquire(ctx); err != nil {
		completeFrom(0, err)
		return
	}
	defer r.release()

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// cancel request if server stops sending data
	stall := time.AfterFunc(r.cfg.RequestTimeout, cancel)
	defer stall.Stop()

	pageSize := r.cfg.PageSize
	start := first * pageSize
	end := min((first+int64(len(calls)))*pageSize, obj.size)

	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(start, end-1); err != nil {
		completeFrom(0, err)
		return
	}
	if obj.etag != "" {
		_ = opts.SetMatchETag(obj.etag)
	}

	body, info, _, err := r.core.GetObject(reqCtx, r.bucket, obj.key, opts)
	if err == nil && obj.etag != "" && strings.Trim(info.ETag, `"`) != obj.etag {
		// some servers ignore If-Match for GET requests
		_ = body.Close()
		err = ErrObjectChanged
	}
	if err != nil {
		err = r.downloadError(ctx, obj, err)
		completeFrom(0, err)
		return
	}
	defer body.Close()

	for i, call := range calls {
		idx := first + int64(i)
		buf := make([]byte, min(pageSize, obj.size-idx*pageSize))

		stall.Reset(r.cfg.RequestTimeout)
		if _, err := io.ReadFull(body, buf); err != nil {
			completeFrom(i, r.downloadError(ctx, obj, err))
			return
		}

		r.pages.complete(r.pageKey(obj, idx), call, buf, nil)
	}
}

func (r *Root) downloadError(ctx context.Context, obj objInfo, err error) error {
	if ctx.Err() == nil && errors.Is(err, context.Canceled) {
		// request context was cancelled by stall timer
		err = errStalled
	}

	err = mapError(err)
	if errors.Is(err, ErrObjectChanged) {
		r.meta.invalidate(obj.key, r.parentPrefix(obj.key))
	}

	return fmt.Errorf("get %s: %w", obj.key, err)
}
