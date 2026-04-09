package master

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/star/mirrorgo/shared"
)

func TestAtomicWriteJSONFallsBackForBusyMountTarget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirrorz.json")
	if err := os.WriteFile(path, []byte(`{"old":true}`), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	prevRename := renameFile
	renameFile = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EBUSY}
	}
	t.Cleanup(func() {
		renameFile = prevRename
	})

	if err := atomicWriteJSON(path, map[string]string{"status": "ok"}, true); err != nil {
		t.Fatalf("atomicWriteJSON: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got map[string]string
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got["status"] != "ok" {
		t.Fatalf("status = %q, want %q", got["status"], "ok")
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("expected JSON output to end with newline, got %q", string(data))
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("expected tmp file to be removed, stat err = %v", err)
	}
}

func TestGetMirrorStatusTreatsReconcilingAsSyncing(t *testing.T) {
	s := newTestState(t)
	s.Repos["repo"] = shared.Repo{
		RepoID: "repo",
		Info: shared.Info{
			Url:         "https://example.invalid/repo",
			Name:        shared.I18NString{"en": "Repo"},
			Description: shared.I18NString{"en": "Desc"},
			Upstream:    "upstream",
		},
	}
	s.Jobs["repo"] = &shared.Job{
		RepoID:  "repo",
		Actions: []string{"action-1"},
	}
	s.ActiveActions["action-1"] = &shared.Action{
		ID:        "action-1",
		JobID:     "repo",
		Status:    shared.ActionStatusReconciling,
		StartedAt: time.Unix(123, 0),
	}

	mirrors, err := s.getMirrorStatus()
	if err != nil {
		t.Fatalf("getMirrorStatus: %v", err)
	}
	if len(mirrors) != 1 {
		t.Fatalf("len(mirrors) = %d, want 1", len(mirrors))
	}
	if mirrors[0].Status != "syncing" {
		t.Fatalf("status = %q, want %q", mirrors[0].Status, "syncing")
	}
}

func TestGenerateMirrorZTreatsReconcilingAsRunning(t *testing.T) {
	s := newTestState(t)
	s.Repos["repo"] = shared.Repo{
		RepoID: "repo",
		Info: shared.Info{
			Url:         "https://example.invalid/repo",
			Description: shared.I18NString{"en": "Desc"},
			Upstream:    "upstream",
		},
	}
	s.Jobs["repo"] = &shared.Job{
		RepoID:    "repo",
		Actions:   []string{"action-1"},
		UpdatedAt: time.Now(),
	}
	s.ActiveActions["action-1"] = &shared.Action{
		ID:        "action-1",
		JobID:     "repo",
		Status:    shared.ActionStatusReconciling,
		StartedAt: time.Unix(123, 0),
	}

	doc := s.GenerateMirrorZ()
	if len(doc.Mirrors) != 1 {
		t.Fatalf("len(doc.Mirrors) = %d, want 1", len(doc.Mirrors))
	}
	if !strings.HasPrefix(doc.Mirrors[0].Status, "Y123") {
		t.Fatalf("status = %q, want prefix %q", doc.Mirrors[0].Status, "Y123")
	}
}
