package s3root

import (
	"sync"
	"time"
)

// metaCache caches object metadata to avoid HEAD request per directory entry stat.
// Directory listings mark directory as "complete": missing direct child of such directory is known to not exist.
type metaCache struct {
	ttl, negativeTTL time.Duration
	now              func() time.Time

	mu       sync.Mutex
	entries  map[string]metaEntry // by key
	complete map[string]time.Time // listing prefix -> expiration
	inserts  int
}

type metaEntry struct {
	info    objInfo
	exists  bool
	expires time.Time
}

const metaPruneInterval = 4096 // inserts between expired entries cleanup

func newMetaCache(ttl, negativeTTL time.Duration) *metaCache {
	return &metaCache{
		ttl:         ttl,
		negativeTTL: negativeTTL,
		now:         time.Now,
		entries:     make(map[string]metaEntry),
		complete:    make(map[string]time.Time),
	}
}

// get returns cached info. found reports whether answer is known, exists reports whether object exists.
func (c *metaCache) get(key, parentPrefix string) (info objInfo, found, exists bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()

	if e, ok := c.entries[key]; ok && now.Before(e.expires) {
		return e.info, true, e.exists
	}

	// Children of complete listing expire later than listing itself (see beginListing),
	// so absence of the entry means absence of the object.
	if exp, ok := c.complete[parentPrefix]; ok && now.Before(exp) {
		return objInfo{}, true, false
	}

	return objInfo{}, false, false
}

func (c *metaCache) put(key string, info objInfo) {
	c.set(key, metaEntry{info: info, exists: true, expires: c.now().Add(c.ttl)})
}

func (c *metaCache) putMissing(key string) {
	c.set(key, metaEntry{expires: c.now().Add(c.negativeTTL)})
}

// invalidate removes entry, i.e. when object was changed.
func (c *metaCache) invalidate(key, parentPrefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.entries, key)
	delete(c.complete, parentPrefix)
}

// beginListing returns expiration time for directory listing which must be passed to completeListing.
// It's taken before any child is inserted so children always outlive the listing.
func (c *metaCache) beginListing() time.Time {
	return c.now().Add(c.ttl)
}

func (c *metaCache) completeListing(prefix string, expires time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.complete[prefix] = expires
}

func (c *metaCache) set(key string, e metaEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[key] = e

	c.inserts++
	if c.inserts%metaPruneInterval == 0 {
		c.pruneLocked()
	}
}

func (c *metaCache) pruneLocked() {
	now := c.now()
	for k, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, k)
		}
	}
	for k, exp := range c.complete {
		if !now.Before(exp) {
			delete(c.complete, k)
		}
	}
}
