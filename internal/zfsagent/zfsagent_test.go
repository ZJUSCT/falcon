package zfsagent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeRunner is a Runner over canned responses; it records every command so
// tests can assert what the collector executed. respond is keyed by the full
// command line ("zfs get -r ... tank").
type fakeRunner struct {
	commands []string
	respond  func(command string) ([]byte, error)
}

func (f *fakeRunner) Run(_ context.Context, bin string, args ...string) ([]byte, error) {
	command := bin + " " + strings.Join(args, " ")
	f.commands = append(f.commands, command)
	return f.respond(command)
}

// zpoolGetCommand is the exact zpool get invocation Report issues per pool
// (frozen like the zfs get flags below).
const zpoolGetPrefix = "zpool get -Hp -o name,property,value size,allocated,free,capacity,fragmentation,health "

func TestCollectorExplicitPools(t *testing.T) {
	runner := &fakeRunner{respond: func(command string) ([]byte, error) {
		switch {
		case strings.HasPrefix(command, "zfs get "):
			if !strings.HasSuffix(command, " tank") {
				return nil, fmt.Errorf("unexpected pool in %q", command)
			}
			return []byte(cannedZfsGet), nil
		case strings.HasPrefix(command, zpoolGetPrefix):
			return []byte(cannedZpoolGet), nil
		}
		return nil, fmt.Errorf("unexpected command %q", command)
	}}
	c := NewCollector("storage-1", []string{"tank"}, runner)

	report, err := c.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	// Explicit pool list: no zpool enumeration, one zfs get + one zpool get.
	if len(runner.commands) != 2 ||
		!strings.HasPrefix(runner.commands[0], "zfs get ") ||
		runner.commands[1] != zpoolGetPrefix+"tank" {
		t.Errorf("commands = %v, want one zfs get and one zpool get for tank", runner.commands)
	}
	if !strings.Contains(runner.commands[0], "-Hp") ||
		!strings.Contains(runner.commands[0], "-t filesystem,volume,snapshot") ||
		!strings.Contains(runner.commands[0], "-o name,property,value") {
		t.Errorf("zfs get invocation lacks the frozen flags: %q", runner.commands[0])
	}
	if len(report.Pools) != 1 || len(report.Pools[0].Datasets) != 3 {
		t.Errorf("unexpected report: %+v", report)
	}
}

func TestCollectorEnumeratesPools(t *testing.T) {
	runner := &fakeRunner{respond: func(command string) ([]byte, error) {
		switch {
		case command == "zpool list -H -o name":
			return []byte("tank\nzpool2\n"), nil
		case strings.HasPrefix(command, "zfs get ") && strings.HasSuffix(command, " tank"):
			return []byte("tank/a\tused\t1\n"), nil
		case strings.HasPrefix(command, "zfs get ") && strings.HasSuffix(command, " zpool2"):
			return []byte("zpool2/b\tused\t2\n"), nil
		case strings.HasPrefix(command, zpoolGetPrefix):
			return []byte(cannedZpoolGet), nil
		default:
			return nil, fmt.Errorf("unexpected command %q", command)
		}
	}}
	c := NewCollector("storage-1", nil, runner)

	report, err := c.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(runner.commands) != 5 || !strings.HasPrefix(runner.commands[0], "zpool list") {
		t.Errorf("commands = %v, want zpool list then zfs get + zpool get per pool", runner.commands)
	}
	if len(report.Pools) != 2 {
		t.Fatalf("got %d pools, want 2", len(report.Pools))
	}
}

// TestCollectorPoolCapacity: the zpool get values land on the pool; a pool
// whose zpool get fails keeps its datasets with unknown (zero) capacity.
func TestCollectorPoolCapacity(t *testing.T) {
	runner := &fakeRunner{respond: func(command string) ([]byte, error) {
		switch {
		case strings.HasSuffix(command, " tank"):
			if strings.HasPrefix(command, "zfs get ") {
				return []byte("tank/pvc-x\tused\t5\n"), nil
			}
			return []byte(cannedZpoolGet), nil
		case strings.HasSuffix(command, " degraded"):
			if strings.HasPrefix(command, "zfs get ") {
				return []byte("degraded/pvc-y\tused\t7\n"), nil
			}
			return nil, errors.New("zpool get failed")
		default:
			return nil, fmt.Errorf("unexpected command %q", command)
		}
	}}
	c := NewCollector("storage-1", []string{"tank", "degraded"}, runner)

	report, err := c.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(report.Pools) != 2 {
		t.Fatalf("got %d pools, want 2", len(report.Pools))
	}
	tank := report.Pools[0]
	if tank.SizeBytes != 1099511627776 || tank.AllocatedBytes != 137438953472 ||
		tank.FreeBytes != 962072674304 || tank.CapacityPercent != 12 {
		t.Errorf("tank capacity = %+v", tank)
	}
	// fragmentation "-" counts as unknown; health is passed through.
	if tank.FragmentationPercent != 0 || tank.Health != "ONLINE" {
		t.Errorf("tank fragmentation/health = %d/%q, want 0/ONLINE", tank.FragmentationPercent, tank.Health)
	}
	if len(tank.Datasets) != 1 {
		t.Errorf("tank datasets = %+v", tank.Datasets)
	}
	degraded := report.Pools[1]
	if degraded.SizeBytes != 0 || degraded.Health != "" {
		t.Errorf("failed zpool get must leave capacity unknown, got %+v", degraded)
	}
	if len(degraded.Datasets) != 1 || degraded.Datasets[0].Name != "degraded/pvc-y" {
		t.Errorf("datasets must survive the failed zpool get: %+v", degraded.Datasets)
	}
}

// TestCollectorSkipsBrokenPool: one unavailable pool must not fail the report.
func TestCollectorSkipsBrokenPool(t *testing.T) {
	runner := &fakeRunner{respond: func(command string) ([]byte, error) {
		switch {
		case command == "zpool list -H -o name":
			return []byte("broken\nhealthy\n"), nil
		case strings.HasSuffix(command, " broken"):
			return nil, errors.New("cannot open 'broken': dataset does not exist")
		case strings.HasSuffix(command, " healthy"):
			if strings.HasPrefix(command, "zfs get ") {
				return []byte("healthy/pvc-x\tused\t5\nhealthy/pvc-x\topenebs.io:pvc-name\tm-sync\nhealthy/pvc-x\topenebs.io:pvc-namespace\tmirror\n"), nil
			}
			return []byte("healthy\tsize\t100\nhealthy\thealth\tONLINE\n"), nil
		default:
			return nil, fmt.Errorf("unexpected command %q", command)
		}
	}}
	c := NewCollector("storage-1", nil, runner)

	report, err := c.Report(context.Background())
	if err != nil {
		t.Fatalf("Report must survive a broken pool: %v", err)
	}
	if len(report.Pools) != 2 {
		t.Fatalf("got %d pools, want 2 (broken pool present but empty)", len(report.Pools))
	}
	for _, pool := range report.Pools {
		if pool.Name == "broken" && (len(pool.Datasets) != 0 || pool.SizeBytes != 0) {
			t.Errorf("broken pool must have no data, got %+v", pool)
		}
		if pool.Name == "healthy" && (len(pool.Datasets) != 1 || pool.SizeBytes != 100 || pool.Health != "ONLINE") {
			t.Errorf("healthy pool = %+v", pool)
		}
	}
}

// TestCollectorEnumerationFailureIsFatal: when even zpool list fails there is
// nothing to report — the collector must return an error (HTTP 500).
func TestCollectorEnumerationFailureIsFatal(t *testing.T) {
	runner := &fakeRunner{respond: func(string) ([]byte, error) {
		return nil, errors.New("no working zpool binary")
	}}
	c := NewCollector("storage-1", nil, runner)
	if _, err := c.Report(context.Background()); err == nil {
		t.Fatal("expected an error when pool enumeration fails")
	}
}

func TestCollectorGeneratedAt(t *testing.T) {
	runner := &fakeRunner{respond: func(string) ([]byte, error) { return []byte("tank\tused\t1\n"), nil }}
	c := NewCollector("n", []string{"tank"}, runner)
	c.now = func() time.Time { return time.Unix(1725100000, 0).UTC() }

	report, err := c.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if !report.GeneratedAt.Equal(time.Unix(1725100000, 0).UTC()) {
		t.Errorf("generatedAt = %v", report.GeneratedAt)
	}
}
