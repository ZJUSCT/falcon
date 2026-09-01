package zfsagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// Runner executes ZFS administration binaries ("zfs", "zpool") and returns
// their stdout. The collector depends only on this narrow interface so tests
// can feed canned output instead of requiring a real ZFS toolchain.
type Runner interface {
	Run(ctx context.Context, bin string, args ...string) ([]byte, error)
}

// DefaultZfsBinCandidates are the well-known absolute zfs paths probed in
// order when no explicit binary is configured. The paths must be absolute:
// inside the agent container the exec is chrooted into the host root, so a
// bare name would be looked up in the (nearly empty) agent image instead of
// on the host.
var DefaultZfsBinCandidates = []string{"/sbin/zfs", "/usr/sbin/zfs"}

// HostRunner is the production Runner: it execs the host's binaries. Root, when
// non-empty, chroot()s each child into that directory (the DaemonSet mounts
// the host root at /host): relative binary paths then resolve on the host and
// dynamic linking resolves against the host's libraries. Root empty execs
// directly (local development against a host zfs).
//
// For each binary the candidate paths are tried in order until one exec
// *starts*; the first working candidate is memoized. A candidate that cannot
// even start (binary absent — e.g. /sbin/zfs missing on the host) advances to
// the next one, while a candidate that starts but exits non-zero is a real
// zfs/zpool error and does not advance.
type HostRunner struct {
	root          string
	zfsCandidates []string

	mu     sync.Mutex
	chosen map[string]string
}

// NewHostRunner returns a runner execing zfsCandidates (DefaultZfsBinCandidates
// when empty) chrooted into root ("" disables chrooting).
func NewHostRunner(root string, zfsCandidates []string) *HostRunner {
	if len(zfsCandidates) == 0 {
		zfsCandidates = DefaultZfsBinCandidates
	}
	return &HostRunner{root: root, zfsCandidates: zfsCandidates, chosen: map[string]string{}}
}

// candidatesFor derives the candidate paths of zfs/zpool. zpool sits next to
// zfs (a custom --zfs-bin implies a custom zpool in the same directory), so
// its candidates are the zfs candidates with the basename swapped.
func (r *HostRunner) candidatesFor(bin string) []string {
	out := make([]string, 0, len(r.zfsCandidates))
	for _, zfsPath := range r.zfsCandidates {
		out = append(out, filepath.Join(filepath.Dir(zfsPath), bin))
	}
	return out
}

// Run implements Runner.
func (r *HostRunner) Run(ctx context.Context, bin string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if path := r.chosen[bin]; path != "" {
		return r.exec(ctx, path, args)
	}
	var attempts []string
	for _, path := range r.candidatesFor(bin) {
		out, err := r.exec(ctx, path, args)
		if err == nil {
			r.chosen[bin] = path
			return out, nil
		}
		var start *startError
		if errors.As(err, &start) {
			attempts = append(attempts, fmt.Sprintf("%s: %v", path, start.err))
			continue
		}
		return out, err
	}
	return nil, fmt.Errorf("no working %s binary (tried %s)", bin, strings.Join(attempts, "; "))
}

// exec runs one candidate and captures stdout/stderr.
func (r *HostRunner) exec(ctx context.Context, path string, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	if r.root != "" {
		cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: r.root}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, &startError{path: path, err: err}
	}
	if err := cmd.Wait(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return stdout.Bytes(), fmt.Errorf("%s: %w: %s", path, err, msg)
		}
		return stdout.Bytes(), fmt.Errorf("%s: %w", path, err)
	}
	return stdout.Bytes(), nil
}

// startError marks "the process never ran" (binary not found, wrong exec
// format, permission denied, chroot failure): HostRunner treats it as
// "candidate unusable" and tries the next candidate.
type startError struct {
	path string
	err  error
}

func (e *startError) Error() string { return fmt.Sprintf("exec %s: %v", e.path, e.err) }

func (e *startError) Unwrap() error { return e.err }
