package zfsagent

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// PushInterval is both the SDK's export interval (PeriodicReader) and the
// agent's perf collection interval, so exactly one collection feeds one
// export.
const PushInterval = 15 * time.Second

// Metric naming: the counters below deliberately do NOT carry a _total
// suffix. Prometheus' OTLP ingest appends _total to monotonic sums on its
// own, so an explicit suffix would surface as zfs_*_total_total.
//
// All instruments are observable (async): the sources are kernel counters
// that are already cumulative, and the collection loop only stores the
// latest reading. A sync Int64Counter would be wrong here — its cumulative
// aggregation sums every Add(), so recording absolute counter snapshots
// each tick would export the *sum of snapshots* (100+150+210...) instead of
// the latest value, inflating every rate() by an order of magnitude. The
// observable precomputed sum reports the last observed value as-is, exactly
// like a Prometheus scrape of the same counter, and counter resets (a
// dataset remount zeroes its objset kstats) flow through to the backend.

// OTLPPusher turns PerfSamples into OTLP metrics. Push only stores the
// sample; the instruments observe it from the SDK reader's callback at each
// export. This file is deliberately thin — all data shaping lives in
// CollectPerf and the parsers.
type OTLPPusher struct {
	node string

	// snapshot is the latest Push result, observed by the reader callback.
	snapshot atomic.Pointer[pushSnapshot]
	// shutdown flushes and stops the provider.
	shutdown func(context.Context) error

	// ARC metrics (attributes: node).
	arcSize     metric.Int64ObservableGauge
	arcHits     metric.Int64ObservableCounter
	arcMisses   metric.Int64ObservableCounter
	arcL2Hits   metric.Int64ObservableCounter
	arcL2Misses metric.Int64ObservableCounter

	// Pool metrics, from each pool's own iostat summary row (node, pool).
	poolReadBytes  metric.Int64ObservableCounter
	poolWriteBytes metric.Int64ObservableCounter
	poolReadOps    metric.Int64ObservableCounter
	poolWriteOps   metric.Int64ObservableCounter

	// Vdev metrics, from the per-vdev iostat rows (node, pool, vdev).
	vdevReadBytes  metric.Int64ObservableCounter
	vdevWriteBytes metric.Int64ObservableCounter
	vdevReadOps    metric.Int64ObservableCounter
	vdevWriteOps   metric.Int64ObservableCounter

	// Dataset metrics, from the objset kstats (node, pool, dataset, pvc).
	dsReads      metric.Int64ObservableCounter
	dsWrites     metric.Int64ObservableCounter
	dsReadBytes  metric.Int64ObservableCounter
	dsWriteBytes metric.Int64ObservableCounter
}

// pushSnapshot is one stored collection result: the sample plus the
// dataset→PVC references resolved at collection time (the callback must not
// call back into live state).
type pushSnapshot struct {
	sample *PerfSample
	// pvc maps a dataset name to "namespace/name" ("" = not zfs-localpv).
	pvc map[string]string
}

// NewOTLPPusher builds the production pusher: an OTLP HTTP exporter reading
// the standard OTEL_EXPORTER_OTLP_* environment variables, a PeriodicReader
// exporting every PushInterval, and a resource identifying the agent as
// service.name=falcon-zfs-agent with service.instance.id=<node>. Export
// failures are routed to log — telemetry must never take down the agent.
func NewOTLPPusher(ctx context.Context, node string, log *slog.Logger) (*OTLPPusher, error) {
	exporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("build OTLP HTTP exporter: %w", err)
	}
	if log != nil {
		otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
			log.Warn("OTLP export failed", "error", err.Error())
		}))
	}
	return newPusher(node, sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(PushInterval)))
}

// newPusher wires the instruments and their callback over one SDK reader
// (a PeriodicReader in production, a ManualReader in tests).
func newPusher(node string, reader sdkmetric.Reader) (*OTLPPusher, error) {
	res, err := resource.New(context.Background(), resource.WithAttributes(
		semconv.ServiceName("falcon-zfs-agent"),
		semconv.ServiceInstanceID(node),
	))
	if err != nil {
		return nil, fmt.Errorf("build OTLP resource: %w", err)
	}
	provider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(reader),
		sdkmetric.WithResource(res),
	)

	p := &OTLPPusher{
		node:     node,
		shutdown: provider.Shutdown,
	}
	meter := provider.Meter("github.com/ZJUSCT/falcon/internal/zfsagent")

	// instruments collects every observable for the single callback
	// registration below.
	var instruments []metric.Observable
	newCounter := func(name, description string, target *metric.Int64ObservableCounter) error {
		inst, err := meter.Int64ObservableCounter(name, metric.WithDescription(description))
		if err != nil {
			return err
		}
		*target = inst
		instruments = append(instruments, inst)
		return nil
	}

	arcSize, err := meter.Int64ObservableGauge("zfs_arc_size_bytes",
		metric.WithDescription("ZFS ARC size in bytes."))
	if err != nil {
		return nil, err
	}
	p.arcSize = arcSize
	instruments = append(instruments, p.arcSize)
	if err := newCounter("zfs_arc_hits", "ZFS ARC hits.", &p.arcHits); err != nil {
		return nil, err
	}
	if err := newCounter("zfs_arc_misses", "ZFS ARC misses.", &p.arcMisses); err != nil {
		return nil, err
	}
	if err := newCounter("zfs_arc_l2_hits", "ZFS ARC L2 (cache device) hits.", &p.arcL2Hits); err != nil {
		return nil, err
	}
	if err := newCounter("zfs_arc_l2_misses", "ZFS ARC L2 (cache device) misses.", &p.arcL2Misses); err != nil {
		return nil, err
	}
	if err := newCounter("zfs_pool_read_bytes", "ZFS pool bytes read since pool import.", &p.poolReadBytes); err != nil {
		return nil, err
	}
	if err := newCounter("zfs_pool_write_bytes", "ZFS pool bytes written since pool import.", &p.poolWriteBytes); err != nil {
		return nil, err
	}
	if err := newCounter("zfs_pool_read_ops", "ZFS pool read operations since pool import.", &p.poolReadOps); err != nil {
		return nil, err
	}
	if err := newCounter("zfs_pool_write_ops", "ZFS pool write operations since pool import.", &p.poolWriteOps); err != nil {
		return nil, err
	}
	if err := newCounter("zfs_vdev_read_bytes", "ZFS vdev bytes read since pool import.", &p.vdevReadBytes); err != nil {
		return nil, err
	}
	if err := newCounter("zfs_vdev_write_bytes", "ZFS vdev bytes written since pool import.", &p.vdevWriteBytes); err != nil {
		return nil, err
	}
	if err := newCounter("zfs_vdev_read_ops", "ZFS vdev read operations since pool import.", &p.vdevReadOps); err != nil {
		return nil, err
	}
	if err := newCounter("zfs_vdev_write_ops", "ZFS vdev write operations since pool import.", &p.vdevWriteOps); err != nil {
		return nil, err
	}
	if err := newCounter("zfs_dataset_reads", "ZFS dataset read operations since mount.", &p.dsReads); err != nil {
		return nil, err
	}
	if err := newCounter("zfs_dataset_writes", "ZFS dataset write operations since mount.", &p.dsWrites); err != nil {
		return nil, err
	}
	if err := newCounter("zfs_dataset_read_bytes", "ZFS dataset bytes read since mount.", &p.dsReadBytes); err != nil {
		return nil, err
	}
	if err := newCounter("zfs_dataset_write_bytes", "ZFS dataset bytes written since mount.", &p.dsWriteBytes); err != nil {
		return nil, err
	}

	// The one callback behind every instrument: observe the latest stored
	// snapshot, or nothing at all (series are simply absent until the first
	// collection). The node attribute is attached to every series explicitly
	// — the resource already carries service.instance.id, but an explicit
	// attribute keeps the series directly queryable by node in Prometheus.
	if _, err := meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		snap := p.snapshot.Load()
		if snap == nil {
			return nil
		}
		node := attribute.String("node", p.node)
		if v, ok := snap.sample.Arc["size"]; ok {
			o.ObserveInt64(p.arcSize, v, metric.WithAttributes(node))
		}
		for key, instrument := range map[string]metric.Int64ObservableCounter{
			"hits":      p.arcHits,
			"misses":    p.arcMisses,
			"l2_hits":   p.arcL2Hits,
			"l2_misses": p.arcL2Misses,
		} {
			if v, ok := snap.sample.Arc[key]; ok {
				o.ObserveInt64(instrument, v, metric.WithAttributes(node))
			}
		}
		for _, v := range snap.sample.Vdevs {
			pool := attribute.String("pool", v.Pool)
			if v.Vdev == v.Pool {
				// The pool's own summary row (its name column equals the pool).
				attrs := metric.WithAttributes(node, pool)
				o.ObserveInt64(p.poolReadBytes, v.ReadBytes, attrs)
				o.ObserveInt64(p.poolWriteBytes, v.WriteBytes, attrs)
				o.ObserveInt64(p.poolReadOps, v.ReadOps, attrs)
				o.ObserveInt64(p.poolWriteOps, v.WriteOps, attrs)
				continue
			}
			attrs := metric.WithAttributes(node, pool, attribute.String("vdev", v.Vdev))
			o.ObserveInt64(p.vdevReadBytes, v.ReadBytes, attrs)
			o.ObserveInt64(p.vdevWriteBytes, v.WriteBytes, attrs)
			o.ObserveInt64(p.vdevReadOps, v.ReadOps, attrs)
			o.ObserveInt64(p.vdevWriteOps, v.WriteOps, attrs)
		}
		for _, d := range snap.sample.Datasets {
			attrs := metric.WithAttributes(
				node,
				attribute.String("pool", d.Pool),
				attribute.String("dataset", d.Dataset),
				attribute.String("pvc", snap.pvc[d.Dataset]),
			)
			o.ObserveInt64(p.dsReads, d.Reads, attrs)
			o.ObserveInt64(p.dsWrites, d.Writes, attrs)
			o.ObserveInt64(p.dsReadBytes, d.NreadBytes, attrs)
			o.ObserveInt64(p.dsWriteBytes, d.NwrittenBytes, attrs)
		}
		return nil
	}, instruments...); err != nil {
		return nil, fmt.Errorf("register OTLP callback: %w", err)
	}
	return p, nil
}

// Push stores one sample for the next export, resolving every dataset's PVC
// reference up front (pvcOf maps a dataset to "namespace/name"; nil or ""
// for datasets not managed by zfs-localpv). Storing atomically replaces the
// previous snapshot — the exporter always reads the latest complete one.
func (p *OTLPPusher) Push(sample *PerfSample, pvcOf func(pool, dataset string) string) {
	if sample == nil {
		return
	}
	snap := &pushSnapshot{sample: sample, pvc: make(map[string]string, len(sample.Datasets))}
	for _, d := range sample.Datasets {
		if pvcOf != nil {
			snap.pvc[d.Dataset] = pvcOf(d.Pool, d.Dataset)
		}
	}
	p.snapshot.Store(snap)
}

// Shutdown flushes and stops the push pipeline (best-effort; the caller
// decides how fatal a failure is).
func (p *OTLPPusher) Shutdown(ctx context.Context) error {
	return p.shutdown(ctx)
}
