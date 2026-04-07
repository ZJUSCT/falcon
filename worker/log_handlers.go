package worker

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

type logEntry struct {
	Name    string    `json:"name"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

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

// listLogDir returns log directory entries for an action.
func listLogDir(actionID string) ([]logEntry, error) {
	abs, err := resolveActionLogPath(actionID, ".")
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return []logEntry{}, nil
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
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
	return out, nil
}

// readLogFile returns the full content of a log file.
func readLogFile(actionID, file string) (string, error) {
	if strings.TrimSpace(file) == "" {
		return "", errors.New("missing file")
	}
	abs, err := resolveActionLogPath(actionID, file)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", errors.New("path is a directory")
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// streamLogFile streams a log file's new content, similar to tail -f.
func streamLogFile(actionID, file, reqID string, stopCh <-chan struct{}, ws *WSClient) {
	sendError := func(msg string) {
		_ = ws.sendJSON(shared.WSLogStreamData{
			Type:  "log_stream_data",
			ReqID: reqID,
			Data:  "ERROR: " + msg + "\n",
		})
	}
	if strings.TrimSpace(file) == "" {
		sendError("missing file parameter")
		return
	}
	abs, err := resolveActionLogPath(actionID, file)
	if err != nil {
		sendError(err.Error())
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		sendError(err.Error())
		return
	}
	defer f.Close()

	// Start from last 100 lines.
	const lastlines = 100
	offset := int64(0)
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
					_ = ws.sendJSON(shared.WSLogStreamData{
						Type:  "log_stream_data",
						ReqID: reqID,
						Data:  string(buf[startIdx:]),
					})
				}
			}
		}
		offset = size
	}

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	buf := make([]byte, 1<<16)

	for {
		select {
		case <-stopCh:
			log.Debug().Str("req_id", reqID).Msg("log stream stopped by master")
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
				_ = ws.sendJSON(shared.WSLogStreamData{
					Type:  "log_stream_data",
					ReqID: reqID,
					Data:  string(buf[:n]),
				})
				offset += int64(n)
			}
		}
	}
}
