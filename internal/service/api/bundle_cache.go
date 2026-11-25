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

const bundleCacheLimit = 128

type bundleKey struct {
	scope, principal, agent, directive, reach string
	revision                                  int64
}

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
