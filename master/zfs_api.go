package master

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/star/mirrorgo/shared"
)

// handleZFSRefresh — POST /api/zfs/refresh — trigger immediate pull from all workers
func (s *State) handleZFSRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	s.RefreshZFSReports()
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// handleZFSReport — GET /api/zfs/report[?worker=<name>]
func (s *State) handleZFSReport(w http.ResponseWriter, r *http.Request) {
	workerName := strings.TrimSpace(r.URL.Query().Get("worker"))

	s.ZFSReportsMu.RLock()
	defer s.ZFSReportsMu.RUnlock()

	if workerName != "" {
		report, ok := s.ZFSReports[workerName]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "no report for worker"})
			return
		}
		writeJSON(w, http.StatusOK, report)
		return
	}

	reports := make([]*shared.ZFSWorkerReport, 0, len(s.ZFSReports))
	for _, r := range s.ZFSReports {
		reports = append(reports, r)
	}
	writeJSON(w, http.StatusOK, reports)
}

// handleZFSPools — GET /api/zfs/pools?worker=<name>
func (s *State) handleZFSPools(w http.ResponseWriter, r *http.Request) {
	workerName := strings.TrimSpace(r.URL.Query().Get("worker"))
	if workerName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing worker parameter"})
		return
	}
	pools, err := s.WSHub.ZFSGetPools(workerName)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, pools)
}

// handleZFSDatasets — GET /api/zfs/datasets?worker=<name>
func (s *State) handleZFSDatasets(w http.ResponseWriter, r *http.Request) {
	workerName := strings.TrimSpace(r.URL.Query().Get("worker"))
	if workerName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing worker parameter"})
		return
	}
	datasets, err := s.WSHub.ZFSGetDatasets(workerName)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, datasets)
}

// handleZFSSnapshots — GET /api/zfs/snapshots?worker=<name>[&dataset=<ds>]
func (s *State) handleZFSSnapshots(w http.ResponseWriter, r *http.Request) {
	workerName := strings.TrimSpace(r.URL.Query().Get("worker"))
	dataset := strings.TrimSpace(r.URL.Query().Get("dataset"))
	if workerName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing worker parameter"})
		return
	}
	snaps, err := s.WSHub.ZFSGetSnapshots(workerName, dataset)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, snaps)
}

// handleZFSCreateSnapshot — POST /api/zfs/snapshots/create
func (s *State) handleZFSCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req struct {
		Worker    string `json:"worker"`
		Dataset   string `json:"dataset"`
		SnapName  string `json:"snap_name"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Worker == "" || req.Dataset == "" || req.SnapName == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker, dataset, and snap_name are required"})
		return
	}
	if err := s.WSHub.ZFSCreateSnapshot(req.Worker, req.Dataset, req.SnapName, req.Recursive); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// handleZFSDestroySnapshot — POST /api/zfs/snapshots/destroy
func (s *State) handleZFSDestroySnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req struct {
		Worker   string `json:"worker"`
		Snapshot string `json:"snapshot"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Worker == "" || req.Snapshot == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker and snapshot are required"})
		return
	}
	if err := s.WSHub.ZFSDestroySnapshot(req.Worker, req.Snapshot); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// handleZFSCreateDataset — POST /api/zfs/datasets/create
func (s *State) handleZFSCreateDataset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req struct {
		Worker     string            `json:"worker"`
		Name       string            `json:"name"`
		Properties map[string]string `json:"properties"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Worker == "" || req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker and name are required"})
		return
	}
	if err := s.WSHub.ZFSCreateDataset(req.Worker, req.Name, req.Properties); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

// handleZFSSetProperty — POST /api/zfs/datasets/set_property
func (s *State) handleZFSSetProperty(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req struct {
		Worker   string `json:"worker"`
		Dataset  string `json:"dataset"`
		Property string `json:"property"`
		Value    string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
		return
	}
	if req.Worker == "" || req.Dataset == "" || req.Property == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "worker, dataset, and property are required"})
		return
	}
	if err := s.WSHub.ZFSSetProperty(req.Worker, req.Dataset, req.Property, req.Value); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}
