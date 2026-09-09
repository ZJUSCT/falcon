package zfsagent

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// Exercise the real streaming runner, timestamp framing, topology changes,
// reuse between collections, disappearance, and source restarts together.
func TestIostatStreamingLifecycle(t *testing.T) {
	var starts atomic.Int32
	lines := make(chan string)
	runner := streamingRunner{stream: func(ctx context.Context, line func(string)) error {
		starts.Add(1)
		for {
			select {
			case value := <-lines:
				if value == "fail" {
					return fmt.Errorf("pool exported")
				}
				line(value)
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}}
	c := NewCollector("storage-1", nil, runner)
	defer c.ClosePerf()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	collect := func() <-chan []VdevIO {
		out := make(chan []VdevIO, 1)
		go func() { out <- c.collectIostat(ctx, []string{"tank"}) }()
		return out
	}
	send := func(value string) {
		t.Helper()
		select {
		case lines <- value:
		case <-ctx.Done():
			t.Fatal("stream did not consume test line")
		}
	}
	first := collect()
	send("100") // Initial timestamp has no interval report after -y.
	send("115")
	send("tank\t1\t2\t3\t4\t5\t6")
	send("disk-a\t0\t0\t3\t4\t5\t6")
	select {
	case <-first:
		t.Fatal("partial report emitted before its closing timestamp")
	default:
	}
	send("130")
	if rows := <-first; len(rows) != 2 || rows[1].Vdev != "disk-a" {
		t.Fatalf("first window: %+v", rows)
	}

	second := collect()
	send("tank\t1\t2\t0\t1\t0\t10")
	send("disk-b\t-\t-\t0\t1\t0\t10")
	send("145")
	if rows := <-second; len(rows) != 2 || rows[1].Vdev != "disk-b" || rows[0].WriteBytes != 10 {
		t.Fatalf("changed topology or decreasing rate lost: %+v", rows)
	}
	if starts.Load() != 1 {
		t.Fatalf("process restarted between windows: %d", starts.Load())
	}

	failed := collect()
	send("fail")
	if rows := <-failed; len(rows) != 0 {
		t.Fatalf("failed stream replayed stale rates: %+v", rows)
	}
	<-c.ioSources["tank"].done
	restarted := collect()
	send("200")
	send("tank\t1\t2\t0\t0\t0\t0")
	send("215")
	if rows := <-restarted; len(rows) != 1 || rows[0].WriteBytes != 0 {
		t.Fatalf("restart did not recover: %+v", rows)
	}
	if starts.Load() != 2 {
		t.Fatalf("failed process not restarted: %d", starts.Load())
	}
	source := c.ioSources["tank"]
	if rows := c.collectIostat(ctx, nil); len(rows) != 0 {
		t.Fatalf("removed pool still emitted: %+v", rows)
	}
	select {
	case <-source.done:
	case <-ctx.Done():
		t.Fatal("removed pool's process was not cancelled")
	}
	if len(c.ioSources) != 0 {
		t.Fatalf("removed pool source retained: %v", c.ioSources)
	}
}

func TestIostatStallCancellation(t *testing.T) {
	stopped := make(chan struct{})
	c := NewCollector("storage-1", nil, streamingRunner{stream: func(ctx context.Context, _ func(string)) error {
		defer close(stopped)
		<-ctx.Done()
		return ctx.Err()
	}})
	defer c.ClosePerf()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if rows := c.collectIostat(ctx, []string{"tank"}); len(rows) != 0 {
		t.Fatalf("stalled source emitted rates: %+v", rows)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stalled source not cancelled")
	}
}

type streamingRunner struct {
	stream func(context.Context, func(string)) error
}

func (r streamingRunner) Run(context.Context, string, ...string) ([]byte, error) {
	return nil, fmt.Errorf("unexpected one-shot command")
}

func (r streamingRunner) Stream(ctx context.Context, _ string, line func(string), _ ...string) error {
	return r.stream(ctx, line)
}
