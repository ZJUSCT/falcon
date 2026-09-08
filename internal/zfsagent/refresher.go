package zfsagent

import (
	"context"
	"errors"
	"sync"
	"time"
)

// errNoReport is returned by Snapshot before the first successful collection.
var errNoReport = errors.New("no ZFS report collected yet")

// refreshInterval is how often the background loop rebuilds the report. 60s
// because the zfs get behind Report is the most expensive thing the agent
// runs: one ioctl per dataset and per snapshot (linear in the pool's tree),
// and the controller already caches aggregations for 30s — anything much
// faster would mostly re-answer identical questions.
const refreshInterval = 60 * time.Second

// Refresher decouples serving from collecting: a background loop keeps the
// latest successful Report of a Collector cached, so HTTP requests are served
// from memory instead of each triggering a full collection sweep.
type Refresher struct {
	collector *Collector
	interval  time.Duration

	mu     sync.Mutex
	report *Report // latest successful report; nil until the first success
}

// NewRefresher returns a Refresher refreshing collector's report every
// refreshInterval.
func NewRefresher(collector *Collector) *Refresher {
	return &Refresher{collector: collector, interval: refreshInterval}
}

// Run refreshes the report once immediately, then every interval, until ctx
// is done. A failed refresh keeps the previous report (its GeneratedAt tells
// consumers how stale it is); the loop simply tries again next interval.
func (r *Refresher) Run(ctx context.Context) {
	r.refresh(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refresh(ctx)
		}
	}
}

// refresh collects once and stores the report on success. Unexported so tests
// can drive single refreshes deterministically.
func (r *Refresher) refresh(ctx context.Context) {
	report, err := r.collector.Report(ctx)
	if err != nil {
		r.collector.log().WarnContext(ctx, "report refresh failed; keeping the previous report", "error", err.Error())
		return
	}
	r.mu.Lock()
	r.report = report
	r.mu.Unlock()
}

// Snapshot returns the latest successful report. Callers must treat it as
// read-only (the same pointer is served to every request). An error is
// returned only when collection has never succeeded yet.
func (r *Refresher) Snapshot() (*Report, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.report == nil {
		return nil, errNoReport
	}
	return r.report, nil
}
