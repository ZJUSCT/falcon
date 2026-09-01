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

func TestCollectorExplicitPools(t *testing.T) {
	runner := &fakeRunner{respond: func(command string) ([]byte, error) {
		if strings.HasPrefix(command, "zfs get ") {
			if !strings.HasSuffix(command, " tank") {
				return nil, fmt.Errorf("unexpected pool in %q", command)
			}
			return []byte(cannedZfsGet), nil
		}
		return nil, fmt.Errorf("unexpected command %q", command)
	}}
	c := NewCollector("storage-1", []string{"tank"}, runner)

	report, err := c.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	// Explicit pool list: no zpool enumeration.
	if len(runner.commands) != 1 || !strings.HasPrefix(runner.commands[0], "zfs get ") {
		t.Errorf("commands = %v, want exactly one zfs get", runner.commands)
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
		default:
			return nil, fmt.Errorf("unexpected command %q", command)
		}
	}}
	c := NewCollector("storage-1", nil, runner)

	report, err := c.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(runner.commands) != 3 || !strings.HasPrefix(runner.commands[0], "zpool list") {
		t.Errorf("commands = %v, want zpool list then one zfs get per pool", runner.commands)
	}
	if len(report.Pools) != 2 {
		t.Fatalf("got %d pools, want 2", len(report.Pools))
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
			return []byte("healthy/pvc-x\tused\t5\nhealthy/pvc-x\topenebs.io:pvc-name\tm-sync\nhealthy/pvc-x\topenebs.io:pvc-namespace\tmirror\n"), nil
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
		if pool.Name == "broken" && len(pool.Datasets) != 0 {
			t.Errorf("broken pool must have no datasets, got %+v", pool.Datasets)
		}
		if pool.Name == "healthy" && len(pool.Datasets) != 1 {
			t.Errorf("healthy pool datasets = %+v", pool.Datasets)
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
