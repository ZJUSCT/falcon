package main

import (
	"embed"
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

	"github.com/rs/zerolog/log"
)

//go:embed ui/dist/*
var uiFS embed.FS

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func handleRepos(w http.ResponseWriter, r *http.Request) {
	reposMu.RLock()
	defer reposMu.RUnlock()

	keys := make([]string, 0, len(Repos))
	for k := range Repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]Repo, 0, len(keys))
	for _, k := range keys {
		out = append(out, Repos[k])
	}

	writeJSON(w, http.StatusOK, out)
}

func handleJobs(w http.ResponseWriter, r *http.Request) {
	jobsMu.RLock()
	defer jobsMu.RUnlock()

	keys := make([]string, 0, len(Jobs))
	for k := range Jobs {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]Job, 0, len(keys))
	for _, k := range keys {
		out = append(out, *Jobs[k])
	}

	writeJSON(w, http.StatusOK, out)
}

func handleActions(w http.ResponseWriter, r *http.Request) {
	actionsMu.RLock()
	defer actionsMu.RUnlock()

	keys := make([]string, 0, len(ActiveActions))
	for k := range ActiveActions {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make([]Action, 0, len(keys))
	for _, k := range keys {
		out = append(out, *ActiveActions[k])
	}

	writeJSON(w, http.StatusOK, out)
}

// GET /api/actions/lookup?ids=id1,id2,id3
func handleActionsLookup(w http.ResponseWriter, r *http.Request) {
	idsParam := r.URL.Query().Get("ids")
	if strings.TrimSpace(idsParam) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing ids query param"})
		return
	}
	rawIDs := strings.Split(idsParam, ",")
	out := make([]Action, 0, len(rawIDs))
	for _, id := range rawIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if a := GetActionByID(id); a != nil {
			out = append(out, *a)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/actions/by_repo?repo_id=<id>&limit=100
func handleActionsByRepo(w http.ResponseWriter, r *http.Request) {
	repoID := r.URL.Query().Get("repo_id")
	if strings.TrimSpace(repoID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing repo_id"})
		return
	}
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 1000 {
				n = 1000
			}
			limit = n
		}
	}
	var rows []ActionModel
	if err := gormDB.Where("job_id = ?", repoID).Order("updated_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]Action, 0, len(rows))
	for _, m := range rows {
		out = append(out, Action{
			ID:                  m.ID,
			UpdatedAt:           m.UpdatedAt,
			JobID:               m.JobID,
			Status:              m.Status,
			Message:             m.Message,
			ContainerID:         m.ContainerID,
			ContainerName:       m.ContainerName,
			ContainerImage:      m.ContainerImage,
			ContainerStatus:     m.ContainerStatus,
			ContainerExitCode:   m.ContainerExitCode,
			ContainerExitSignal: m.ContainerExitSignal,
			ContainerExitReason: m.ContainerExitReason,
			ContainerVolumes:    m.ContainerVolumes,
			ContainerEnv:        m.ContainerEnv,
			ContainerCommand:    m.ContainerCommand,
			ContainerTimeout:    m.ContainerTimeout,
			CreatedAt:           m.CreatedAt,
			StartedAt:           m.StartedAt,
			FinishedAt:          m.FinishedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /api/actions/recent?limit=100
// Returns the most recent actions across all repos, ordered by updated_at DESC
func handleActionsRecent(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 1000 {
				n = 1000
			}
			limit = n
		}
	}
	var rows []ActionModel
	if err := gormDB.Order("updated_at DESC").Limit(limit).Find(&rows).Error; err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]Action, 0, len(rows))
	for _, m := range rows {
		out = append(out, Action{
			ID:                  m.ID,
			UpdatedAt:           m.UpdatedAt,
			JobID:               m.JobID,
			Status:              m.Status,
			Message:             m.Message,
			ContainerID:         m.ContainerID,
			ContainerName:       m.ContainerName,
			ContainerImage:      m.ContainerImage,
			ContainerStatus:     m.ContainerStatus,
			ContainerExitCode:   m.ContainerExitCode,
			ContainerExitSignal: m.ContainerExitSignal,
			ContainerExitReason: m.ContainerExitReason,
			ContainerVolumes:    m.ContainerVolumes,
			ContainerEnv:        m.ContainerEnv,
			ContainerCommand:    m.ContainerCommand,
			ContainerTimeout:    m.ContainerTimeout,
			CreatedAt:           m.CreatedAt,
			StartedAt:           m.StartedAt,
			FinishedAt:          m.FinishedAt,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func handleQueue(w http.ResponseWriter, r *http.Request) {
	if jobQueue == nil {
		writeJSON(w, http.StatusOK, map[string]any{"paused": true, "queue": []string{}})
		return
	}
	out := jobQueue.Snapshot()
	resp := map[string]any{
		"paused": jobQueue.IsPaused(),
		"queue":  out,
	}
	if out == nil {
		resp["queue"] = []string{}
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /api/jobs/next_attempt_now?repo_id=<id>
// If the job is in Waiting status, set NextAttemptAt to now and persist.
func handleJobNextAttemptNow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	repoID := r.URL.Query().Get("repo_id")
	if strings.TrimSpace(repoID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing repo_id"})
		return
	}
	jobsMu.Lock()
	job, ok := Jobs[repoID]
	if !ok {
		jobsMu.Unlock()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "job not found"})
		return
	}
	if job.Status != JobStatusWaiting {
		jobsMu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "job is not in Waiting status"})
		return
	}
	job.NextAttemptAt = time.Now()
	job.UpdatedAt = time.Now()
	jobsMu.Unlock()

	if err := upsertJob(job); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// POST /api/queue/pause  body ignored
// POST /api/queue/continue
func handleQueuePause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	jobQueue.SetPaused(true)
	_ = dbSetQueuePaused(true)
	writeJSON(w, http.StatusOK, map[string]any{"paused": true})
}

func handleQueueContinue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	jobQueue.SetPaused(false)
	_ = dbSetQueuePaused(false)
	writeJSON(w, http.StatusOK, map[string]any{"paused": false})
}

// POST /api/queue/move_to_head?repo_id=<id>
// POST /api/queue/move_to_tail?repo_id=<id>
// POST /api/queue/move_before?target_id=<id>&ref_id=<id>
// POST /api/queue/move_after?target_id=<id>&ref_id=<id>
func handleQueueMoveToHead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("repo_id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing repo_id"})
		return
	}
	ok := jobQueue.MoveToHead(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "queue": jobQueue.Snapshot()})
}

func handleQueueMoveToTail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("repo_id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing repo_id"})
		return
	}
	ok := jobQueue.MoveToTail(id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "queue": jobQueue.Snapshot()})
}

func handleQueueMoveBefore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("target_id"))
	ref := strings.TrimSpace(r.URL.Query().Get("ref_id"))
	if target == "" || ref == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing target_id or ref_id"})
		return
	}
	ok := jobQueue.MoveBefore(target, ref)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "queue": jobQueue.Snapshot()})
}

func handleQueueMoveAfter(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	target := strings.TrimSpace(r.URL.Query().Get("target_id"))
	ref := strings.TrimSpace(r.URL.Query().Get("ref_id"))
	if target == "" || ref == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing target_id or ref_id"})
		return
	}
	ok := jobQueue.MoveAfter(target, ref)
	writeJSON(w, http.StatusOK, map[string]any{"ok": ok, "queue": jobQueue.Snapshot()})
}

// POST /api/queue/delete?repo_id=<id>  removes all occurrences of the repo from the queue
func handleQueueDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("repo_id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing repo_id"})
		return
	}
	removed := 0
	for jobQueue.Remove(id) {
		removed++
	}
	if err := dbDeleteAllQueueByJob(id); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// If job was Scheduled, revert to Waiting
	jobsMu.Lock()
	if job, ok := Jobs[id]; ok {
		if job.Status == JobStatusScheduled {
			job.Status = JobStatusWaiting
			job.UpdatedAt = time.Now()
			job.NextAttemptAt = time.Now().Add(time.Hour * 999999)
		}
	}
	jobsMu.Unlock()
	if job, ok := Jobs[id]; ok {
		_ = upsertJob(job)
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed, "queue": jobQueue.Snapshot(), "paused": jobQueue.IsPaused()})
}

// resolve a path inside an action's log directory safely
// It ensures no path traversal outside the action directory
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
	// Clean joins and normalize
	target := filepath.Join(baseAbs, relPath)
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	// Ensure targetAbs is within baseAbs
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

// GET /api/logs/list?action_id=<id>
// Lists files and directories directly under the action's log directory
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

// GET /api/logs/raw?action_id=<id>&file=<name>
// Returns the full content of the specified log file
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

// GET /api/logs/stream?action_id=<id>&file=<name>&from=start|end
// Streams new data appended to the file similar to `tail -f`.
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
			// Read up to the last 64KB to extract last 5 lines
			const maxScan int64 = 1 << 16
			readStart := size - maxScan
			if readStart < 0 {
				readStart = 0
			}
			toRead := size - readStart
			if toRead > 0 {
				buf := make([]byte, toRead)
				if n, err := f.ReadAt(buf, readStart); err == nil || (err == io.EOF && int64(n) == toRead) {
					// Find start index of the last 5 lines
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
						startIdx = idx + 2 // move to byte after the found '\n'
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

func startWebServer(addr string) {
	http.HandleFunc("/api/repos", handleRepos)
	http.HandleFunc("/api/jobs", handleJobs)
	http.HandleFunc("/api/jobs/next_attempt_now", handleJobNextAttemptNow)
	http.HandleFunc("/api/actions", handleActions)
	http.HandleFunc("/api/actions/lookup", handleActionsLookup)
	http.HandleFunc("/api/actions/by_repo", handleActionsByRepo)
	http.HandleFunc("/api/actions/recent", handleActionsRecent)
	http.HandleFunc("/api/logs/list", handleLogsList)
	http.HandleFunc("/api/logs/raw", handleLogsRaw)
	http.HandleFunc("/api/logs/stream", handleLogsStream)
	http.HandleFunc("/api/queue", handleQueue)
	http.HandleFunc("/api/queue/pause", handleQueuePause)
	http.HandleFunc("/api/queue/continue", handleQueueContinue)
	http.HandleFunc("/api/queue/move_to_head", handleQueueMoveToHead)
	http.HandleFunc("/api/queue/move_to_tail", handleQueueMoveToTail)
	http.HandleFunc("/api/queue/move_before", handleQueueMoveBefore)
	http.HandleFunc("/api/queue/move_after", handleQueueMoveAfter)
	http.HandleFunc("/api/queue/delete", handleQueueDelete)

	// remove prefix /ui/dist/
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.ServeFileFS(w, r, uiFS, "ui/dist/index.html")
			return
		}
		http.ServeFileFS(w, r, uiFS, "ui/dist"+r.URL.Path)
	})

	log.Info().Str("addr", addr).Msg("Starting web server")
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatal().Err(err).Msg("Web server exited")
	}
}
