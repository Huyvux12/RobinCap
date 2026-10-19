package main

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLRUSingleflightAndEviction(t *testing.T) {
	c := newLRU[int](2, time.Hour)
	var calls atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, _, _ := c.Do("k", func() (int, error) { calls.Add(1); time.Sleep(20 * time.Millisecond); return 7, nil })
			if v != 7 {
				t.Error("bad value")
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("fn ran %d times", calls.Load())
	}
	c.Put("a", 1)
	c.Put("b", 2)
	if _, ok := c.Get("k"); ok {
		t.Fatal("k should be evicted")
	}
	if _, _, err := c.Do("e", func() (int, error) { return 0, errors.New("x") }); err == nil {
		t.Fatal("want error")
	}
	if _, ok := c.Get("e"); ok {
		t.Fatal("errors must not be cached")
	}
}

func TestRateLimiter(t *testing.T) {
	l := newIPLimiter(60, 2)
	a, _ := l.allow("ip", 1)
	b, _ := l.allow("ip", 1)
	c, retry := l.allow("ip", 1)
	if !a || !b || c || retry <= 0 {
		t.Fatal(a, b, c, retry)
	}
	if ok, _ := l.allow("other", 1); !ok {
		t.Fatal("other ip should pass")
	}
}

func TestNormRate(t *testing.T) {
	for in, want := range map[string]string{"": "1.0", "1": "1.0", "1.25": "1.25", " 0.8 ": "0.8"} {
		if got, err := normRate(in); err != nil || got != want {
			t.Fatalf("%q -> %q %v", in, got, err)
		}
	}
	if _, err := normRate("9"); err == nil {
		t.Fatal("want error")
	}
}
