package snapshot

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"nvidia-license-server-exporter/internal/cls"
)

type fakeFetcher struct {
	mu      sync.Mutex
	calls   int
	results []fetchResult
}

type fetchResult struct {
	snapshot *cls.Snapshot
	err      error
	blockFor time.Duration
}

func (f *fakeFetcher) FetchSnapshot(_ context.Context) (*cls.Snapshot, error) {
	f.mu.Lock()
	f.calls++
	callIdx := f.calls - 1
	var r fetchResult
	if callIdx < len(f.results) {
		r = f.results[callIdx]
	}
	f.mu.Unlock()

	if r.blockFor > 0 {
		time.Sleep(r.blockFor)
	}
	return r.snapshot, r.err
}

func (f *fakeFetcher) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestServiceGetCacheHit(t *testing.T) {
	now := time.Now().UTC()
	fetcher := &fakeFetcher{
		results: []fetchResult{
			{snapshot: &cls.Snapshot{CollectedAt: now}},
		},
	}
	svc := NewService(fetcher, time.Minute)

	snap1, meta1, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("first get error: %v", err)
	}
	if snap1 == nil || !snap1.CollectedAt.Equal(now) {
		t.Fatalf("unexpected first snapshot: %+v", snap1)
	}
	if meta1.Up != 1 {
		t.Fatalf("expected up=1, got %v", meta1.Up)
	}
	if meta1.CacheHit {
		t.Fatalf("expected first get to not be cache hit")
	}

	snap2, meta2, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("second get error: %v", err)
	}
	if snap2 != snap1 {
		t.Fatalf("expected same cached snapshot pointer")
	}
	if !meta2.CacheHit {
		t.Fatalf("expected second get cache hit")
	}
	if meta2.DurationSeconds != 0 {
		t.Fatalf("expected cache hit duration=0, got %f", meta2.DurationSeconds)
	}
	if fetcher.CallCount() != 1 {
		t.Fatalf("expected 1 fetch call, got %d", fetcher.CallCount())
	}
}

func TestServiceGetCacheExpiry(t *testing.T) {
	t0 := time.Now().UTC()
	t1 := t0.Add(2 * time.Second)
	fetcher := &fakeFetcher{
		results: []fetchResult{
			{snapshot: &cls.Snapshot{CollectedAt: t0}},
			{snapshot: &cls.Snapshot{CollectedAt: t1}},
		},
	}
	svc := NewService(fetcher, 20*time.Millisecond)

	first, _, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("first get error: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	second, meta, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("second get error: %v", err)
	}
	if first == second {
		t.Fatalf("expected refreshed snapshot after ttl expiry")
	}
	if meta.CacheHit {
		t.Fatalf("expected non-cache-hit after expiry")
	}
	if fetcher.CallCount() != 2 {
		t.Fatalf("expected 2 fetch calls, got %d", fetcher.CallCount())
	}
}

func TestServiceRefreshStaleFallback(t *testing.T) {
	now := time.Now().UTC()
	fetcher := &fakeFetcher{
		results: []fetchResult{
			{snapshot: &cls.Snapshot{CollectedAt: now}},
			{err: errors.New("boom")},
		},
	}
	svc := NewService(fetcher, time.Minute)

	first, _, err := svc.Get(context.Background())
	if err != nil {
		t.Fatalf("first get error: %v", err)
	}

	second, meta, err := svc.Refresh(context.Background())
	if err != nil {
		t.Fatalf("refresh should return stale snapshot without error, got %v", err)
	}
	if second != first {
		t.Fatalf("expected stale snapshot pointer on refresh failure")
	}
	if meta.Up != 0 {
		t.Fatalf("expected up=0 on stale fallback, got %v", meta.Up)
	}
}

func TestServiceRefreshErrorWithoutCache(t *testing.T) {
	fetcher := &fakeFetcher{
		results: []fetchResult{
			{err: errors.New("boom")},
		},
	}
	svc := NewService(fetcher, time.Minute)

	_, _, err := svc.Get(context.Background())
	if err == nil {
		t.Fatalf("expected error when no cache exists")
	}
}

func TestServiceRefreshSingleflight(t *testing.T) {
	now := time.Now().UTC()
	fetcher := &fakeFetcher{
		results: []fetchResult{
			{snapshot: &cls.Snapshot{CollectedAt: now}, blockFor: 50 * time.Millisecond},
		},
	}
	svc := NewService(fetcher, time.Minute)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := svc.Refresh(context.Background())
			if err != nil {
				t.Errorf("refresh error: %v", err)
			}
		}()
	}
	wg.Wait()

	if fetcher.CallCount() != 1 {
		t.Fatalf("expected single fetch due to singleflight, got %d", fetcher.CallCount())
	}
}
