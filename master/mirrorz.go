package master

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/star/mirrorgo/shared"
)

// jsonWriteMu serializes writes to mirrorgo.json and mirrorz.json.
var jsonWriteMu sync.Mutex

// atomicWriteJSON writes JSON to a file atomically (write tmp + rename).
func atomicWriteJSON(path string, v interface{}, indent bool) error {
	jsonWriteMu.Lock()
	defer jsonWriteMu.Unlock()

	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}

	enc := json.NewEncoder(f)
	if indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	f.Close()
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------------------
// MirrorZ types
// ---------------------------------------------------------------------------

// MirrorZSite describes site-level metadata for MirrorZ JSON
type MirrorZSite struct {
	URL          string `json:"url,omitempty"`
	Logo         string `json:"logo,omitempty"`
	LogoDarkmode string `json:"logo_darkmode,omitempty"`
	Abbr         string `json:"abbr,omitempty"`
	Name         string `json:"name,omitempty"`
	Homepage     string `json:"homepage,omitempty"`
	Issue        string `json:"issue,omitempty"`
	Request      string `json:"request,omitempty"`
	Email        string `json:"email,omitempty"`
	Group        string `json:"group,omitempty"`
	Disk         string `json:"disk,omitempty"`
	Note         string `json:"note,omitempty"`
	Big          string `json:"big,omitempty"`
	Disable      bool   `json:"disable,omitempty"`
}

type MirrorZInfoURL struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type MirrorZInfoItem struct {
	Distro   string           `json:"distro"`
	Category string           `json:"category"`
	URLs     []MirrorZInfoURL `json:"urls"`
}

type MirrorZMirror struct {
	CName    string `json:"cname"`
	Desc     string `json:"desc,omitempty"`
	URL      string `json:"url"`
	Status   string `json:"status"`
	Help     string `json:"help,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Size     string `json:"size,omitempty"`
	Disable  bool   `json:"disable,omitempty"`
}

type MirrorZ struct {
	Version float64           `json:"version"`
	Site    MirrorZSite       `json:"site"`
	Info    []MirrorZInfoItem `json:"info"`
	Mirrors []MirrorZMirror   `json:"mirrors"`
}

// ---------------------------------------------------------------------------
// MirrorStatus (for mirrorgo.json)
// ---------------------------------------------------------------------------

type MirrorStatus struct {
	ID            string            `json:"id"`
	URL           string            `json:"url"`
	Name          map[string]string `json:"name"`
	Desc          map[string]string `json:"desc"`
	Upstream      string            `json:"upstream"`
	Size          int64             `json:"size"`
	Status        string            `json:"status"`
	LastAttempt   int64             `json:"lastAttempt"`
	NextScheduled int64             `json:"nextScheduled"`
	LastSuccess   int64             `json:"lastSuccess"`
	LastFailure   int64             `json:"lastFailure"`
}

// ---------------------------------------------------------------------------
// MirrorZ generation
// ---------------------------------------------------------------------------

// GenerateMirrorZ builds the MirrorZ JSON structure from current in-memory state.
func (s *State) GenerateMirrorZ() MirrorZ {
	site := MirrorZSite{
		URL:          os.Getenv("MIRRORZ_SITE_URL"),
		Logo:         os.Getenv("MIRRORZ_SITE_LOGO"),
		LogoDarkmode: os.Getenv("MIRRORZ_SITE_LOGO_DARK"),
		Abbr:         os.Getenv("MIRRORZ_SITE_ABBR"),
		Name:         os.Getenv("MIRRORZ_SITE_NAME"),
		Homepage:     os.Getenv("MIRRORZ_SITE_HOMEPAGE"),
		Issue:        os.Getenv("MIRRORZ_SITE_ISSUE"),
		Request:      os.Getenv("MIRRORZ_SITE_REQUEST"),
		Email:        os.Getenv("MIRRORZ_SITE_EMAIL"),
		Group:        os.Getenv("MIRRORZ_SITE_GROUP"),
		Disk:         os.Getenv("MIRRORZ_SITE_DISK"),
		Note:         os.Getenv("MIRRORZ_SITE_NOTE"),
		Big:          os.Getenv("MIRRORZ_SITE_BIG"),
		Disable:      strings.ToLower(os.Getenv("MIRRORZ_SITE_DISABLE")) == "true",
	}

	s.ReposMu.RLock()
	s.JobsMu.RLock()
	defer s.ReposMu.RUnlock()
	defer s.JobsMu.RUnlock()

	mirrors := make([]MirrorZMirror, 0, len(s.Repos))
	for repoID, repo := range s.Repos {
		status := "U" // default unknown
		if job, ok := s.Jobs[repoID]; ok {
			var (
				lastSuccess int64
				lastFailure int64
				nextSched   int64
				startSync   int64
			)
			if !job.LastSuccessAt.IsZero() {
				lastSuccess = job.LastSuccessAt.Unix()
			}
			if !job.LastFailureAt.IsZero() {
				lastFailure = job.LastFailureAt.Unix()
			}
			if !job.NextAttemptAt.IsZero() {
				nextSched = job.NextAttemptAt.Unix()
			}

			var lastAction *shared.Action
			if len(job.Actions) > 0 {
				lastAction = s.GetActionByIDFromActiveOrDB(job.Actions[len(job.Actions)-1])
			}

			if lastAction != nil && lastAction.Status == shared.ActionStatusRunning {
				if !lastAction.StartedAt.IsZero() {
					startSync = lastAction.StartedAt.Unix()
				} else if !lastAction.CreatedAt.IsZero() {
					startSync = lastAction.CreatedAt.Unix()
				}
				status = "Y"
				if startSync > 0 {
					status += strconv.FormatInt(startSync, 10)
				}
				if lastSuccess > 0 {
					status += "O" + strconv.FormatInt(lastSuccess, 10)
				}
			} else if lastFailure > 0 && (lastSuccess == 0 || lastFailure >= lastSuccess) {
				status = "F" + strconv.FormatInt(lastFailure, 10)
				if lastSuccess > 0 {
					status += "O" + strconv.FormatInt(lastSuccess, 10)
				}
			} else if lastSuccess > 0 {
				status = "S" + strconv.FormatInt(lastSuccess, 10)
			} else if job.Status == shared.JobStatusScheduled || job.Status == shared.JobStatusWaiting {
				status = "D"
				if nextSched > 0 {
					status += strconv.FormatInt(nextSched, 10)
				}
			} else {
				status = "U"
			}

			if nextSched > 0 {
				status += "X" + strconv.FormatInt(nextSched, 10)
			}
			if lastSuccess == 0 && lastFailure == 0 && lastAction == nil {
				status += "N"
			}
		}

		desc := ""
		if v, ok := repo.Info.Description["zh"]; ok && v != "" {
			desc = v
		} else if v, ok := repo.Info.Description["en"]; ok && v != "" {
			desc = v
		} else {
			for _, v := range repo.Info.Description {
				if v != "" {
					desc = v
					break
				}
			}
		}

		mirrors = append(mirrors, MirrorZMirror{
			CName:    repoID,
			Desc:     desc,
			URL:      repo.Info.Url,
			Status:   status,
			Help:     "/docs/" + repoID + "/",
			Upstream: repo.Info.Upstream,
			Size:     "",
			Disable:  false,
		})
	}

	sort.Slice(mirrors, func(i, j int) bool { return mirrors[i].CName < mirrors[j].CName })

	return MirrorZ{
		Version: 1.7,
		Site:    site,
		Info:    []MirrorZInfoItem{},
		Mirrors: mirrors,
	}
}

// WriteMirrorZJSON writes the MirrorZ JSON to BaseDir/mirrorz.json atomically.
func (s *State) WriteMirrorZJSON(doc MirrorZ) error {
	filePath := filepath.Join(s.BaseDir, "mirrorz.json")
	return atomicWriteJSON(filePath, doc, true)
}

// UpdateMirrorZJSON regenerates and writes the MirrorZ JSON document to disk.
func (s *State) UpdateMirrorZJSON() error {
	_ = time.Now()
	doc := s.GenerateMirrorZ()
	return s.WriteMirrorZJSON(doc)
}

// ---------------------------------------------------------------------------
// mirrorgo.json generation
// ---------------------------------------------------------------------------

// getMirrorStatus returns the current mirror status without writing to file.
func (s *State) getMirrorStatus() ([]MirrorStatus, error) {
	s.ReposMu.RLock()
	s.JobsMu.RLock()
	defer s.ReposMu.RUnlock()
	defer s.JobsMu.RUnlock()

	var mirrors []MirrorStatus

	for repoID, repo := range s.Repos {
		job, exists := s.Jobs[repoID]

		mirror := MirrorStatus{
			ID:       repoID,
			URL:      repo.Info.Url,
			Name:     repo.Info.Name,
			Desc:     repo.Info.Description,
			Upstream: repo.Info.Upstream,
			Size:     0,
		}

		if !exists {
			mirror.Status = "cached"
			mirror.LastAttempt = 0
			mirror.NextScheduled = 0
			mirror.LastSuccess = 0
			mirror.LastFailure = 0
		} else {
			if len(job.Actions) > 0 {
				lastAction := s.GetActionByIDFromActiveOrDB(job.Actions[len(job.Actions)-1])
				if lastAction != nil {
					switch lastAction.Status {
					case shared.ActionStatusRunning:
						mirror.Status = "syncing"
					case shared.ActionStatusSucceeded:
						mirror.Status = "succeeded"
					case shared.ActionStatusFailed:
						mirror.Status = "failed"
					}
				} else {
					mirror.Status = "failed"
				}

				mirror.LastAttempt = job.LastAttemptAt.Unix()
				mirror.NextScheduled = job.NextAttemptAt.Unix()
				mirror.LastSuccess = job.LastSuccessAt.Unix()
				mirror.LastFailure = job.LastFailureAt.Unix()
			} else {
				mirror.Status = "cached"
				mirror.LastAttempt = 0
				mirror.NextScheduled = 0
				mirror.LastSuccess = 0
				mirror.LastFailure = 0
			}
		}

		mirrors = append(mirrors, mirror)
	}

	sort.Slice(mirrors, func(i, j int) bool {
		return mirrors[i].ID < mirrors[j].ID
	})

	return mirrors, nil
}

// writeMirrorgoJSON writes the mirror status list to BaseDir/mirrorgo.json atomically.
func (s *State) writeMirrorgoJSON(mirrors []MirrorStatus) error {
	filePath := filepath.Join(s.BaseDir, "mirrorgo.json")
	return atomicWriteJSON(filePath, mirrors, false)
}

// UpdateMirrorgoJSON updates the mirrorgo.json file with current status.
func (s *State) UpdateMirrorgoJSON() error {
	mirrors, err := s.getMirrorStatus()
	if err != nil {
		return err
	}
	return s.writeMirrorgoJSON(mirrors)
}
