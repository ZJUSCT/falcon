// Command zfs-agent is a read-only ZFS usage reporter. It runs as one
// DaemonSet pod per storage node, executes the host's zfs/zpool binaries
// (chrooted into the /host hostPath mount), and answers:
//
//   - GET /v1/zfs   the node's ZFS dataset/snapshot usage report
//   - GET /healthz  liveness/readiness probe
//
// It never talks to the Kubernetes API; the controller's webapi discovers the
// agents through the headless Service and aggregates their reports (see
// internal/webapi/usage.go). The listen port (9474) is part of that contract
// and therefore not configurable.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ZJUSCT/falcon/internal/zfsagent"
)

const defaultBind = ":9474"

// hostRoot is where the DaemonSet mounts the host's root filesystem. When it
// exists the agent chroots every zfs/zpool exec into it; otherwise (local
// development) the local binaries run directly.
const hostRoot = "/host"

func main() {
	var bind, zfsBin, pools, nodeName string
	flag.StringVar(&bind, "bind", defaultBind, "Listen address for the HTTP endpoints.")
	flag.StringVar(&zfsBin, "zfs-bin", "", fmt.Sprintf("Path to the zfs binary (%s). Its directory also provides zpool.", strings.Join(zfsagent.DefaultZfsBinCandidates, ", ")))
	flag.StringVar(&pools, "pools", "", "Comma-separated ZFS pools to report (default: all pools, via zpool list).")
	flag.StringVar(&nodeName, "node-name", "", "Node name reported in /v1/zfs (default: $HOSTNAME).")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	if nodeName == "" {
		nodeName = os.Getenv("HOSTNAME")
		if nodeName == "" {
			name, err := os.Hostname()
			if err != nil {
				logger.Error("cannot determine node name", "error", err.Error())
				os.Exit(1)
			}
			nodeName = name
		}
	}

	var poolList []string
	for _, pool := range strings.Split(pools, ",") {
		if pool = strings.TrimSpace(pool); pool != "" {
			poolList = append(poolList, pool)
		}
	}

	var candidates []string
	if zfsBin != "" {
		candidates = []string{zfsBin}
	}

	root := ""
	if info, err := os.Stat(hostRoot); err == nil && info.IsDir() {
		// Chroot into the host root: the zfs exec then sees the host's
		// /dev, /sys and module state and links against the host's
		// libraries. Requires a privileged (root) container.
		root = hostRoot
	}

	collector := zfsagent.NewCollector(nodeName, poolList, zfsagent.NewHostRunner(root, candidates))
	collector.Log = logger

	server := &http.Server{
		Addr:              bind,
		Handler:           (&zfsagent.Server{Collector: collector}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	logger.Info("starting zfs-agent",
		"bind", bind,
		"node", nodeName,
		"hostRoot", root,
		"pools", poolsOrAll(pools),
	)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("zfs-agent exited", "error", err.Error())
		os.Exit(1)
	}
}

func poolsOrAll(pools string) string {
	if pools == "" {
		return "<all>"
	}
	return pools
}
