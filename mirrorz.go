package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

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

// GenerateMirrorZ builds the MirrorZ JSON structure from current in-memory state
func GenerateMirrorZ() MirrorZ {
	// Build site from environment variables if provided
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

	reposMu.RLock()
	jobsMu.RLock()
	defer reposMu.RUnlock()
	defer jobsMu.RUnlock()

	// Build mirrors list
	mirrors := make([]MirrorZMirror, 0, len(Repos))
	for repoID, repo := range Repos {
		// Determine detailed MirrorZ status
		status := "U" // default unknown
		if job, ok := Jobs[repoID]; ok {
			// Timestamps
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
			// Inspect last action if any
			var lastAction *Action
			if len(job.Actions) > 0 {
				lastAction = GetActionByID(job.Actions[len(job.Actions)-1])
			}
			// Determine main status
			if lastAction != nil && lastAction.Status == ActionStatusRunning {
				// Y: syncing, use StartedAt when available
				if !lastAction.StartedAt.IsZero() {
					startSync = lastAction.StartedAt.Unix()
				} else if !lastAction.CreatedAt.IsZero() {
					startSync = lastAction.CreatedAt.Unix()
				}
				status = "Y"
				if startSync > 0 {
					status += strconv.FormatInt(startSync, 10)
				}
				// O: old successful timestamp when syncing
				if lastSuccess > 0 {
					status += "O" + strconv.FormatInt(lastSuccess, 10)
				}
			} else if lastFailure > 0 && (lastSuccess == 0 || lastFailure >= lastSuccess) {
				// F: failed (last failed attempt)
				status = "F" + strconv.FormatInt(lastFailure, 10)
				// O: old successful when failed
				if lastSuccess > 0 {
					status += "O" + strconv.FormatInt(lastSuccess, 10)
				}
			} else if lastSuccess > 0 {
				// S: successful (last success)
				status = "S" + strconv.FormatInt(lastSuccess, 10)
			} else if job.Status == JobStatusScheduled || job.Status == JobStatusWaiting {
				// D: pending
				status = "D"
				if nextSched > 0 {
					status += strconv.FormatInt(nextSched, 10)
				}
			} else {
				status = "U" // unknown
			}
			// Auxiliary: X (next scheduled)
			if nextSched > 0 {
				status += "X" + strconv.FormatInt(nextSched, 10)
			}
			// Auxiliary: N (new mirror) if never succeeded/failed and no actions
			if lastSuccess == 0 && lastFailure == 0 && (lastAction == nil) {
				status += "N"
			}
		}

		// Pick a description: prefer zh, then en, then any
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

// WriteMirrorZJSON writes the MirrorZ JSON to BASEDIR/mirrorz.json
func WriteMirrorZJSON(doc MirrorZ) error {
	filePath := filepath.Join(BASEDIR, "mirrorz.json")
	f, err := os.Create(filePath)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// UpdateMirrorZJSON regenerates and writes the MirrorZ JSON document to disk
func UpdateMirrorZJSON() error {
	_ = time.Now() // keep time import for potential future timestamp usage
	doc := GenerateMirrorZ()
	return WriteMirrorZJSON(doc)
}
