package master

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/star/mirrorgo/shared"
)

func newTestState(t *testing.T) *State {
	t.Helper()

	tempDir := t.TempDir()
	if err := InitDB(filepath.Join(tempDir, "state.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	configDir := filepath.Join(tempDir, "configs")
	if err := os.MkdirAll(configDir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	return &State{
		Repos:         make(map[string]shared.Repo),
		Jobs:          make(map[string]*shared.Job),
		ActiveActions: make(map[string]*shared.Action),
		JobQueue:      NewQueue(),
		ConfigDir:     configDir,
	}
}

func performRepoSave(t *testing.T, s *State, repo shared.Repo) *httptest.ResponseRecorder {
	t.Helper()

	body, err := json.Marshal(repo)
	if err != nil {
		t.Fatalf("marshal repo: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/repos", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleRepoSave(rec, req)
	return rec
}

func TestHandleRepoSaveNonFreeWithoutExistingJob(t *testing.T) {
	s := newTestState(t)

	repo := shared.Repo{
		RepoID: "example",
		SyncParams: shared.SyncConfig{
			Interval: shared.IntervalConfig{
				Type:  "cron",
				Value: "0 * * * *",
			},
		},
	}

	rec := performRepoSave(t, s, repo)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	if _, ok := s.Jobs[repo.RepoID]; ok {
		t.Fatalf("expected no job for non-free repo save, found %+v", s.Jobs[repo.RepoID])
	}

	jobs, err := LoadJobsFromDB()
	if err != nil {
		t.Fatalf("LoadJobsFromDB: %v", err)
	}
	if _, ok := jobs[repo.RepoID]; ok {
		t.Fatalf("expected no persisted job for non-free repo save")
	}
}

func TestHandleRepoSaveFreeCreatesWaitingJob(t *testing.T) {
	s := newTestState(t)

	repo := shared.Repo{
		RepoID: "free-repo",
		SyncParams: shared.SyncConfig{
			Interval: shared.IntervalConfig{
				Type: "free",
			},
		},
	}

	rec := performRepoSave(t, s, repo)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	job, ok := s.Jobs[repo.RepoID]
	if !ok || job == nil {
		t.Fatalf("expected in-memory job to be created")
	}
	if job.Status != shared.JobStatusWaiting {
		t.Fatalf("job status = %q, want %q", job.Status, shared.JobStatusWaiting)
	}

	jobs, err := LoadJobsFromDB()
	if err != nil {
		t.Fatalf("LoadJobsFromDB: %v", err)
	}
	persisted, ok := jobs[repo.RepoID]
	if !ok || persisted == nil {
		t.Fatalf("expected persisted job to be created")
	}
	if persisted.Status != shared.JobStatusWaiting {
		t.Fatalf("persisted job status = %q, want %q", persisted.Status, shared.JobStatusWaiting)
	}
}
