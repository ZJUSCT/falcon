package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

// Package-level state set by worker.go before starting the API.
var (
	OnNewAction func(act *shared.Action) // set by worker.go
	actionCache *ActionCache             // set by worker.go
	authToken   string                   // set by worker.go
)

// SetWorkerAPIState configures package-level state used by the HTTP handlers.
func SetWorkerAPIState(cache *ActionCache, token string, onNew func(act *shared.Action)) {
	actionCache = cache
	authToken = token
	OnNewAction = onNew
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// HandleDispatch handles POST /api/internal/dispatch
func HandleDispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var req shared.DispatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body: " + err.Error()})
		return
	}

	da := req.Action

	// Idempotency: check if the action is already known.
	if actionCache != nil {
		if _, found := actionCache.Get(da.ID); found {
			writeJSON(w, http.StatusOK, shared.DispatchResponse{OK: true, Message: "already known"})
			return
		}
	}

	// Build a shared.Action from the dispatch request.
	act := &shared.Action{
		ID:               da.ID,
		JobID:            da.JobID,
		Status:           shared.ActionStatusRunning,
		ContainerName:    "syncing-" + da.JobID + "-" + da.ID,
		ContainerImage:   da.ContainerImage,
		ContainerCommand: da.ContainerCommand,
		ContainerVolumes: shared.VolumeList(da.ContainerVolumes),
		ContainerEnv:     da.ContainerEnv,
		ContainerTimeout: da.ContainerTimeout,
		ContainerStatus:  shared.ContainerStatusStarting,
		CreatedAt:        time.Now(),
		StartedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}

	// Check for duplicate container from a previous dispatch attempt.
	cExists, cRunning, _ := ContainerExistsByName(act.ContainerName)
	if cExists && cRunning {
		// Already running from a previous dispatch attempt - return OK.
		// The existing monitor will handle it.
		writeJSON(w, http.StatusOK, shared.DispatchResponse{OK: true, Message: "container already running"})
		return
	}
	if cExists && !cRunning {
		// Exited container from previous attempt - remove it before starting new one.
		DockerClient.ContainerRemove(context.Background(), act.ContainerName, container.RemoveOptions{Force: true})
	}

	if err := StartContainer(act); err != nil {
		writeJSON(w, http.StatusInternalServerError, shared.DispatchResponse{
			OK:      false,
			Message: "failed to start container: " + err.Error(),
		})
		return
	}

	act.ContainerStatus = shared.ContainerStatusRunning

	if OnNewAction != nil {
		OnNewAction(act)
	}

	writeJSON(w, http.StatusOK, shared.DispatchResponse{OK: true})
}

// HandleActionStatus handles GET /api/internal/action_status?id=xxx
func HandleActionStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing id"})
		return
	}

	if actionCache == nil {
		writeJSON(w, http.StatusOK, &shared.ActionStatusResponse{Found: false, ActionID: id})
		return
	}

	resp := actionCache.ToStatusResponse(id)
	writeJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// Log handlers (ported from web.go)
// ---------------------------------------------------------------------------

func resolveActionLogPath(actionID string, relPath string) (string, error) {
	actionID = strings.TrimSpace(actionID)
	if actionID == "" {
		return "", errors.New("missing action_id")
	}
	baseDir := filepath.Join(LogDir, actionID)
	baseAbs, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}
	if relPath == "" {
		relPath = "."
	}
	target := filepath.Join(baseAbs, relPath)
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	if targetAbs != baseAbs && !strings.HasPrefix(targetAbs, baseAbs+string(os.PathSeparator)) {
		return "", errors.New("invalid path")
	}
	return targetAbs, nil
}

type logEntry struct {
	Name    string    `json:"name"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

// handleLogsList lists files and directories under the action's log directory.
func handleLogsList(w http.ResponseWriter, r *http.Request) {
	actionID := r.URL.Query().Get("action_id")
	abs, err := resolveActionLogPath(actionID, ".")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !fi.IsDir() {
		writeJSON(w, http.StatusOK, map[string]any{
			"action_id": actionID,
			"entries":   []logEntry{},
		})
		return
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]logEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, logEntry{
			Name:    e.Name(),
			IsDir:   e.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"action_id": actionID,
		"entries":   out,
	})
}

// handleLogsRaw returns the full content of the specified log file.
func handleLogsRaw(w http.ResponseWriter, r *http.Request) {
	actionID := r.URL.Query().Get("action_id")
	file := r.URL.Query().Get("file")
	if strings.TrimSpace(file) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing file"})
		return
	}
	abs, err := resolveActionLogPath(actionID, file)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if fi.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is a directory"})
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
}

// handleLogsStream streams new data appended to the file (SSE, similar to tail -f).
func handleLogsStream(w http.ResponseWriter, r *http.Request) {
	actionID := r.URL.Query().Get("action_id")
	file := r.URL.Query().Get("file")
	if strings.TrimSpace(file) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing file"})
		return
	}
	abs, err := resolveActionLogPath(actionID, file)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	fi, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if fi.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is a directory"})
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer f.Close()

	log.Info().Str("action_id", actionID).Str("file", file).Msg("handleLogsStream")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "streaming not supported"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	var lastlines = 100

	_, _ = w.Write([]byte("data: MIRRORGO LOG STREAM: Connected, last " + strconv.Itoa(lastlines) + " lines\n\n"))
	flusher.Flush()

	start := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("from")))
	offset := int64(0)
	if start == "" || start == "end" {
		if st, err := f.Stat(); err == nil {
			size := st.Size()
			const maxScan int64 = 1 << 16
			readStart := size - maxScan
			if readStart < 0 {
				readStart = 0
			}
			toRead := size - readStart
			if toRead > 0 {
				buf := make([]byte, toRead)
				if n, err := f.ReadAt(buf, readStart); err == nil || (err == io.EOF && int64(n) == toRead) {
					newlinesNeeded := lastlines
					idx := len(buf) - 1
					for idx >= 0 && newlinesNeeded > 0 {
						if buf[idx] == '\n' {
							newlinesNeeded--
						}
						idx--
					}
					startIdx := 0
					if newlinesNeeded == 0 {
						startIdx = idx + 2
					}
					if startIdx < 0 || startIdx > len(buf) {
						startIdx = 0
					}
					if len(buf[startIdx:]) > 0 {
						lines := strings.Split(string(buf[startIdx:]), "\n")
						for _, line := range lines {
							_, _ = w.Write([]byte("data: " + line + "\n\n"))
						}
						flusher.Flush()
					}
				}
			}
			offset = size
		}
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	buf := make([]byte, 1<<16)

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			st, err := f.Stat()
			if err != nil {
				return
			}
			size := st.Size()
			if size < offset {
				offset = size
				continue
			}
			if size == offset {
				continue
			}
			toRead := size - offset
			if int64(len(buf)) < toRead {
				toRead = int64(len(buf))
			}
			n, err := f.ReadAt(buf[:toRead], offset)
			if err != nil && err != io.EOF {
				return
			}
			if n > 0 {
				lines := strings.Split(string(buf[:n]), "\n")
				for _, line := range lines {
					if _, err := w.Write([]byte("data: " + line + "\n\n")); err != nil {
						return
					}
				}
				flusher.Flush()
				offset += int64(n)
			}
		}
	}
}

// StartWorkerAPI creates and starts the worker HTTP server.
func StartWorkerAPI(addr, token string) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/internal/dispatch", HandleDispatch)
	mux.HandleFunc("/api/internal/action_status", HandleActionStatus)
	mux.HandleFunc("/api/logs/list", handleLogsList)
	mux.HandleFunc("/api/logs/raw", handleLogsRaw)
	mux.HandleFunc("/api/logs/stream", handleLogsStream)

	handler := shared.InternalAuthMiddleware(token, mux)

	log.Info().Str("addr", addr).Msg("Starting worker API server")
	return http.ListenAndServe(addr, handler)
}
