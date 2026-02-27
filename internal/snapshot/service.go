package snapshot

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	"nvidia-license-server-exporter/internal/cls"
)

const defaultCacheTTL = 60 * time.Second

type Fetcher interface {
	FetchSnapshot(ctx context.Context) (*cls.Snapshot, error)
}

type Meta struct {
	Up              float64
	DurationSeconds float64
	Timestamp       time.Time
	CacheHit        bool
}

type Service struct {
	fetcher  Fetcher
	cacheTTL time.Duration

	mu       sync.RWMutex
	snapshot *cls.Snapshot
	meta     Meta
	cachedAt time.Time

	sf singleflight.Group
}

func NewService(fetcher Fetcher, cacheTTL time.Duration) *Service {
	if cacheTTL <= 0 {
		cacheTTL = defaultCacheTTL
	}

	return &Service{
		fetcher:  fetcher,
		cacheTTL: cacheTTL,
	}
}

func (s *Service) Get(ctx context.Context) (*cls.Snapshot, Meta, error) {
	s.mu.RLock()
	snapshot := s.snapshot
	meta := s.meta
	cachedAt := s.cachedAt
	cacheTTL := s.cacheTTL
	s.mu.RUnlock()

	if snapshot != nil && time.Since(cachedAt) < cacheTTL {
		meta.CacheHit = true
		meta.DurationSeconds = 0
		return snapshot, meta, nil
	}

	return s.Refresh(ctx)
}

func (s *Service) Refresh(ctx context.Context) (*cls.Snapshot, Meta, error) {
	type result struct {
		snapshot *cls.Snapshot
		meta     Meta
	}

	v, err, _ := s.sf.Do("refresh", func() (interface{}, error) {
		start := time.Now()
		fetched, fetchErr := s.fetcher.FetchSnapshot(ctx)
		duration := time.Since(start).Seconds()

		now := time.Now()
		if fetchErr == nil {
			meta := Meta{
				Up:              1,
				DurationSeconds: duration,
				Timestamp:       fetched.CollectedAt,
				CacheHit:        false,
			}

			s.mu.Lock()
			s.snapshot = fetched
			s.meta = meta
			s.cachedAt = now
			s.mu.Unlock()

			return result{snapshot: fetched, meta: meta}, nil
		}

		s.mu.Lock()
		defer s.mu.Unlock()

		if s.snapshot != nil {
			staleMeta := Meta{
				Up:              0,
				DurationSeconds: duration,
				Timestamp:       s.snapshot.CollectedAt,
				CacheHit:        false,
			}
			s.meta = staleMeta
			return result{snapshot: s.snapshot, meta: staleMeta}, nil
		}

		s.meta = Meta{
			Up:              0,
			DurationSeconds: duration,
			Timestamp:       now,
			CacheHit:        false,
		}
		return nil, fetchErr
	})
	if err != nil {
		return nil, Meta{}, err
	}

	res := v.(result)
	return res.snapshot, res.meta, nil
}

func (s *Service) Latest() (*cls.Snapshot, Meta, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if s.snapshot == nil {
		return nil, Meta{}, false
	}
	return s.snapshot, s.meta, true
}

func (s *Service) Meta() Meta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.meta
}
