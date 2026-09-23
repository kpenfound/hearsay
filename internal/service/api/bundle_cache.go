package api

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

	"github.com/kpenfound/hearsay/internal/bundle"
	"github.com/kpenfound/hearsay/internal/l1"
)

// bundleCacheLimit is fixed per API replica. Each entry holds one encoded
// bundle and its audit report; the least recently used entry leaves first.
const bundleCacheLimit = 128

// bundleKey is what a cached bundle was assembled for. The reach is in it as
// well as the caller: it is read from the graph on every call, and a bundle
// assembled within a reach that has since changed is not one to serve.
type bundleKey struct {
	scope, principal, agent, directive, reach string
	revision                                  int64
}

// reachKey is a reader's reach as a key: its digest, since a reach can be many
// entity ids.
func reachKey(reader l1.Reader) string {
	scopes := reader.Effective.Grant.Scopes
	if scopes.All {
		return "*"
	}
	sum := sha256.Sum256([]byte(strings.Join(scopes.IDs, "\n")))
	return hex.EncodeToString(sum[:])
}

type bundleValue struct {
	key    bundleKey
	body   []byte
	report bundle.Report
}

type bundleCache struct {
	mu    sync.Mutex
	order *list.List
	items map[bundleKey]*list.Element
}

func newBundleCache() *bundleCache {
	return &bundleCache{order: list.New(), items: make(map[bundleKey]*list.Element)}
}

func (c *bundleCache) get(key bundleKey) (bundleValue, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[key]
	if !ok {
		return bundleValue{}, false
	}
	c.order.MoveToFront(e)
	return e.Value.(bundleValue), true
}

func (c *bundleCache) put(value bundleValue) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[value.key]; ok {
		e.Value = value
		c.order.MoveToFront(e)
		return
	}
	e := c.order.PushFront(value)
	c.items[value.key] = e
	if c.order.Len() > bundleCacheLimit {
		oldest := c.order.Back()
		delete(c.items, oldest.Value.(bundleValue).key)
		c.order.Remove(oldest)
	}
}
