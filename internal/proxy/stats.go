package proxy

import (
	"sync"
	"time"
)

// Stats are the running totals for a proxy session.
type Stats struct {
	mu sync.Mutex
	s  Snapshot
}

// Snapshot is a point-in-time copy of Stats, safe to serialise.
type Snapshot struct {
	StartedAt      time.Time `json:"started_at"`
	Requests       int64     `json:"requests"`
	Replayed       int64     `json:"replayed"`
	Recorded       int64     `json:"recorded"`
	Passthrough    int64     `json:"passthrough"`
	Misses         int64     `json:"misses"`
	UpstreamErrors int64     `json:"upstream_errors"`
	SavedUSD       float64   `json:"saved_usd"`
	SpentUSD       float64   `json:"spent_usd"`
	SavedTokens    int64     `json:"saved_tokens"`
	SavedTimeMs    int64     `json:"saved_time_ms"`
	Unpriced       int64     `json:"unpriced_responses"`
}

func newStats() *Stats { return &Stats{s: Snapshot{StartedAt: time.Now().UTC()}} }

// Snapshot returns a copy of the current totals.
func (st *Stats) Snapshot() Snapshot {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s
}

func (st *Stats) update(f func(s *Snapshot)) {
	st.mu.Lock()
	f(&st.s)
	st.mu.Unlock()
}
