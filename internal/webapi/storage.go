package webapi

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/ZJUSCT/falcon/internal/zfsagent"
)

// StorageNode is one node's ZFS report, verbatim from its zfs-agent: every
// pool with its datasets, capacity and health fields.
type StorageNode struct {
	Node  string          `json:"node"`
	Pools []zfsagent.Pool `json:"pools"`
}

// StorageResponse is the wire shape of GET /api/storage.
type StorageResponse struct {
	// GeneratedAt is when the aggregation was computed (the served data is
	// at most one cache TTL older).
	GeneratedAt time.Time `json:"generatedAt"`
	// Complete is false when any agent failed or no agent is ready; Errors
	// carries the per-agent reasons.
	Complete bool     `json:"complete"`
	Errors   []string `json:"errors"`
	// Nodes are the reporting nodes, sorted by node name.
	Nodes []StorageNode `json:"nodes"`
}

// Storage returns the /api/storage payload: the (cached) per-node ZFS reports
// of all zfs-agents, sharing /api/usage's aggregation and cache. An error is
// returned only when the aggregation cannot be computed at all; per-agent
// failures degrade the result instead.
func (a *UsageAggregator) Storage(ctx context.Context) (*StorageResponse, error) {
	agg, err := a.aggregate(ctx)
	if err != nil {
		return nil, err
	}
	// The aggregate is cache-shared; serve a copy so sorting cannot touch it.
	nodes := make([]StorageNode, len(agg.nodes))
	copy(nodes, agg.nodes)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Node < nodes[j].Node })
	return &StorageResponse{
		GeneratedAt: agg.generatedAt,
		Complete:    agg.complete,
		Errors:      agg.errors,
		Nodes:       nodes,
	}, nil
}

// handleStorage serves GET /api/storage. Like /api/usage, the endpoint is
// gated on wiring: with ZFS_AGENT_SERVICE unset no aggregator exists and the
// endpoint answers 404.
func (s *Server) handleStorage(w http.ResponseWriter, r *http.Request) {
	if s.Usage == nil {
		writeJSONError(w, http.StatusNotFound, "storage aggregation is disabled")
		return
	}
	storage, err := s.Usage.Storage(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, storage)
}
