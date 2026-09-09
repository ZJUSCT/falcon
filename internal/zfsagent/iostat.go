package zfsagent

import (
	"context"
	"slices"
	"strconv"
	"strings"
)

// iostatSource owns one persistent CLI process and at most one completed
// window. Keeping the process alive avoids gaps between execs: a TXG can
// flush substantial I/O even during a short process-startup gap.
type iostatSource struct {
	cancel  context.CancelFunc
	done    chan struct{}
	windows chan []VdevIO
}

// collectIostat is called only by the serial CollectPerf loop. Pool workers
// run concurrently, but never mutate the collector or its snapshots.
func (c *Collector) collectIostat(ctx context.Context, pools []string) []VdevIO {
	if c.ioSources == nil {
		c.ioSources = make(map[string]*iostatSource)
	}
	for pool, source := range c.ioSources {
		if !slices.Contains(pools, pool) {
			source.cancel()
			delete(c.ioSources, pool)
		}
	}
	for _, pool := range pools {
		source := c.ioSources[pool]
		if source != nil {
			select {
			case <-source.done:
				source = nil
			default:
			}
		}
		if source == nil {
			sourceCtx, cancel := context.WithCancel(ctx)
			source = &iostatSource{cancel: cancel, done: make(chan struct{}), windows: make(chan []VdevIO, 1)}
			c.ioSources[pool] = source
			go c.streamIostat(sourceCtx, pool, source)
		}
	}

	// The first complete report needs two timestamps (30s). Later windows
	// arrive every 15s. One shared deadline bounds the entire sweep even if
	// several pools stall. A stalled source is killed and retried only after
	// its process exits, so an uninterruptible host ioctl cannot accumulate
	// another stuck child on every collection tick.
	ctx, cancel := context.WithTimeout(ctx, 3*PushInterval)
	defer cancel()
	var rows []VdevIO
	for _, pool := range pools {
		source := c.ioSources[pool]
		select {
		case window := <-source.windows:
			rows = append(rows, window...)
		case <-source.done:
			// Failed commands never replay an earlier window as fresh data.
		case <-ctx.Done():
			c.log().Warn("pool I/O stream stalled", "pool", pool, "error", ctx.Err())
			source.cancel()
		}
	}
	return rows
}

func (c *Collector) streamIostat(ctx context.Context, pool string, source *iostatSource) {
	defer close(source.done)
	publish := func(rows []VdevIO) {
		// Drop an unconsumed older window, never block the CLI pipe reader.
		select {
		case <-source.windows:
		default:
		}
		source.windows <- rows
	}
	var report strings.Builder
	line := func(line string) {
		if _, err := strconv.ParseInt(line, 10, 64); err == nil {
			// -T u marks the START of each report; the next timestamp
			// terminates it without assuming a stable vdev topology. This
			// adds one interval of framing delay, but loses no I/O windows.
			if report.Len() != 0 {
				publish(parseZpoolIostat(pool, []byte(report.String())))
				report.Reset()
			}
			return
		}
		report.WriteString(line)
		report.WriteByte('\n')
	}
	// -y omits the first report's lifetime-average rates. -p controls
	// formatting only; it never makes I/O values cumulative counters.
	err := c.Runner.Stream(ctx, "zpool", line, "iostat", "-Hp", "-v", "-y", "-T", "u", pool,
		strconv.Itoa(int(PushInterval.Seconds())))
	publish(nil)
	if ctx.Err() == nil {
		c.log().Warn("pool I/O stream exited", "pool", pool, "error", err)
	}
}

// ClosePerf stops performance subprocesses. Call after the CollectPerf loop
// has stopped; it must not race a collection. Usage reporting is unaffected.
func (c *Collector) ClosePerf() {
	for pool, source := range c.ioSources {
		source.cancel()
		delete(c.ioSources, pool)
	}
}
