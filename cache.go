package main

import (
	"container/list"
	"sync"
	"time"
)

// lruCache is a TTL'd LRU with in-flight de-duplication (singleflight), so
// concurrent identical TTS requests hit CapCut once.
type lruCache[V any] struct {
	mu       sync.Mutex
	max      int
	ttl      time.Duration
	ll       *list.List
	items    map[string]*list.Element
	inflight map[string]*call[V]
	hits     int64
	misses   int64
}

type entry[V any] struct {
	key string
	val V
	exp time.Time
}

type call[V any] struct {
	wg  sync.WaitGroup
	val V
	err error
}

func newLRU[V any](max int, ttl time.Duration) *lruCache[V] {
	return &lruCache[V]{max: max, ttl: ttl, ll: list.New(), items: map[string]*list.Element{}, inflight: map[string]*call[V]{}}
}

func (c *lruCache[V]) Get(k string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(k)
}

func (c *lruCache[V]) getLocked(k string) (V, bool) {
	var zero V
	if c.max <= 0 {
		return zero, false
	}
	el, ok := c.items[k]
	if !ok {
		return zero, false
	}
	e := el.Value.(*entry[V])
	if time.Now().After(e.exp) {
		c.ll.Remove(el)
		delete(c.items, k)
		return zero, false
	}
	c.ll.MoveToFront(el)
	return e.val, true
}

func (c *lruCache[V]) Put(k string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.max <= 0 {
		return
	}
	if el, ok := c.items[k]; ok {
		el.Value = &entry[V]{k, v, time.Now().Add(c.ttl)}
		c.ll.MoveToFront(el)
		return
	}
	c.items[k] = c.ll.PushFront(&entry[V]{k, v, time.Now().Add(c.ttl)})
	for c.ll.Len() > c.max {
		old := c.ll.Back()
		c.ll.Remove(old)
		delete(c.items, old.Value.(*entry[V]).key)
	}
}

// Do returns the cached value or runs fn once per key across concurrent callers.
func (c *lruCache[V]) Do(k string, fn func() (V, error)) (v V, cached bool, err error) {
	c.mu.Lock()
	if v, ok := c.getLocked(k); ok {
		c.hits++
		c.mu.Unlock()
		return v, true, nil
	}
	if cl, ok := c.inflight[k]; ok {
		c.hits++
		c.mu.Unlock()
		cl.wg.Wait()
		return cl.val, true, cl.err
	}
	c.misses++
	cl := &call[V]{}
	cl.wg.Add(1)
	c.inflight[k] = cl
	c.mu.Unlock()

	cl.val, cl.err = fn()
	if cl.err == nil {
		c.Put(k, cl.val)
	}
	c.mu.Lock()
	delete(c.inflight, k)
	c.mu.Unlock()
	cl.wg.Done()
	return cl.val, false, cl.err
}

func (c *lruCache[V]) Stats() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return map[string]any{"size": c.ll.Len(), "max": c.max, "ttl_sec": int(c.ttl.Seconds()), "hits": c.hits, "misses": c.misses}
}
