package api

import (
	"container/list"
	"sync"
	"time"

	"github.com/snarg/tr-engine/internal/database"
	"golang.org/x/time/rate"
)

// Key cache parameters (§8).
const (
	keyCacheTTL  = 30 * time.Second
	keyCacheSize = 4096 // entries per cache
)

// keyCache holds recent key lookups (§8). Positive and negative results live
// in separate bounded LRUs, so random guesses (which only ever create
// negative entries) cannot evict valid keys. Entries expire after
// keyCacheTTL, and both caches are cleared when the auth generation moves.
//
// The positive cache is indexed by key hash (bearer lookups) and by key ID
// (tickets). It holds records, not decisions: callers check expires_at and
// revoked_at on every hit.
type keyCache struct {
	mu  sync.Mutex
	gen uint64
	now func() time.Time

	pos     *list.List // of *posEntry, most recent first
	byHash  map[string]*list.Element
	byID    map[int]*list.Element
	neg     *list.List // of *negEntry, most recent first
	negHash map[string]*list.Element
	size    int
}

type posEntry struct {
	hash    string // "" when loaded by ID only
	key     *database.APIKey
	expires time.Time
}

type negEntry struct {
	hash    string
	reason  database.KeyStatus // KeyRevoked, KeyExpired or "" (unknown)
	expires time.Time
}

func newKeyCache(size int, now func() time.Time) *keyCache {
	return &keyCache{
		now:     now,
		pos:     list.New(),
		byHash:  make(map[string]*list.Element),
		byID:    make(map[int]*list.Element),
		neg:     list.New(),
		negHash: make(map[string]*list.Element),
		size:    size,
	}
}

// sync clears both caches when gen differs from the generation they were
// filled under. The caller holds mu.
func (c *keyCache) sync(gen uint64) {
	if gen == c.gen {
		return
	}
	c.gen = gen
	c.pos.Init()
	c.neg.Init()
	clear(c.byHash)
	clear(c.byID)
	clear(c.negHash)
}

// getByHash returns the cached record for a key hash.
func (c *keyCache) getByHash(gen uint64, hash string) (*database.APIKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sync(gen)
	return c.hit(c.byHash[hash])
}

// getByID returns the cached record for a key ID.
func (c *keyCache) getByID(gen uint64, id int) (*database.APIKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sync(gen)
	return c.hit(c.byID[id])
}

func (c *keyCache) hit(el *list.Element) (*database.APIKey, bool) {
	if el == nil {
		return nil, false
	}
	e := el.Value.(*posEntry)
	if !c.now().Before(e.expires) {
		c.removePos(el)
		return nil, false
	}
	c.pos.MoveToFront(el)
	return e.key, true
}

// put caches a key record looked up under generation gen, by hash (may be
// "" for a record read by ID) and by ID. A record read before the
// generation moved is dropped: it may predate the change.
func (c *keyCache) put(gen uint64, hash string, k *database.APIKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sync(max(gen, c.gen))
	if gen != c.gen {
		return
	}
	if el, ok := c.byID[k.ID]; ok {
		if old := el.Value.(*posEntry); hash == "" {
			hash = old.hash
		}
		c.removePos(el)
	}
	if el, ok := c.byHash[hash]; ok && hash != "" {
		c.removePos(el)
	}
	el := c.pos.PushFront(&posEntry{hash: hash, key: k, expires: c.now().Add(keyCacheTTL)})
	c.byID[k.ID] = el
	if hash != "" {
		c.byHash[hash] = el
		c.removeNeg(c.negHash[hash])
	}
	for c.pos.Len() > c.size {
		c.removePos(c.pos.Back())
	}
}

func (c *keyCache) removePos(el *list.Element) {
	if el == nil {
		return
	}
	e := el.Value.(*posEntry)
	if c.byID[e.key.ID] == el {
		delete(c.byID, e.key.ID)
	}
	if e.hash != "" && c.byHash[e.hash] == el {
		delete(c.byHash, e.hash)
	}
	c.pos.Remove(el)
}

// invalidate drops every entry for key id (after a PATCH or DELETE).
func (c *keyCache) invalidate(id int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.byID[id]; ok {
		if h := el.Value.(*posEntry).hash; h != "" {
			c.removeNeg(c.negHash[h])
		}
		c.removePos(el)
	}
}

// getNegative returns why a hash was last rejected, if that is cached.
func (c *keyCache) getNegative(gen uint64, hash string) (database.KeyStatus, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sync(gen)
	el := c.negHash[hash]
	if el == nil {
		return "", false
	}
	e := el.Value.(*negEntry)
	if !c.now().Before(e.expires) {
		c.removeNeg(el)
		return "", false
	}
	c.neg.MoveToFront(el)
	return e.reason, true
}

// putNegative caches a rejected hash (unknown, revoked or expired), and
// drops any positive entry for it.
func (c *keyCache) putNegative(gen uint64, hash string, reason database.KeyStatus) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sync(max(gen, c.gen))
	if gen != c.gen {
		return
	}
	c.removePos(c.byHash[hash])
	c.removeNeg(c.negHash[hash])
	c.negHash[hash] = c.neg.PushFront(&negEntry{hash: hash, reason: reason, expires: c.now().Add(keyCacheTTL)})
	for c.neg.Len() > c.size {
		c.removeNeg(c.neg.Back())
	}
}

func (c *keyCache) removeNeg(el *list.Element) {
	if el == nil {
		return
	}
	e := el.Value.(*negEntry)
	if c.negHash[e.hash] == el {
		delete(c.negHash, e.hash)
	}
	c.neg.Remove(el)
}

// limiterSet holds token-bucket rate limiters by key (client IP or key ID).
// Entries idle for limiterIdle are swept lazily; there is no goroutine.
type limiterSet struct {
	mu        sync.Mutex
	now       func() time.Time
	m         map[string]*limiterEntry
	lastSweep time.Time
}

type limiterEntry struct {
	lim      *rate.Limiter
	rps      float64
	burst    int
	lastSeen time.Time
}

const limiterIdle = 10 * time.Minute

func newLimiterSet(now func() time.Time) *limiterSet {
	return &limiterSet{now: now, m: make(map[string]*limiterEntry)}
}

// allow takes one token from the limiter for key, created (or replaced, when
// the limits changed) with rps and burst.
func (s *limiterSet) allow(key string, rps float64, burst int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if now.Sub(s.lastSweep) > limiterIdle {
		for k, e := range s.m {
			if now.Sub(e.lastSeen) > limiterIdle {
				delete(s.m, k)
			}
		}
		s.lastSweep = now
	}
	e, ok := s.m[key]
	if !ok || e.rps != rps || e.burst != burst {
		e = &limiterEntry{lim: rate.NewLimiter(rate.Limit(rps), burst), rps: rps, burst: burst}
		s.m[key] = e
	}
	e.lastSeen = now
	return e.lim.AllowN(now, 1)
}
