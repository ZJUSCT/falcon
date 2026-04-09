package shared

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"
)

type I18NString map[string]string

type Info struct {
	Name        I18NString `json:"name"`
	Description I18NString `json:"description"`
	Type        string     `json:"type"` // sync, cached,
	Upstream    string     `json:"upstream"`
	Url         string     `json:"url"`
}

type Volume struct {
	Source      string `json:"src"`
	Destination string `json:"dst"`
}

// Value implements the driver.Valuer interface for Volume, storing as JSON text
func (v Volume) Value() (driver.Value, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan implements the sql.Scanner interface for Volume, reading from JSON text
func (v *Volume) Scan(src interface{}) error {
	switch data := src.(type) {
	case []byte:
		return json.Unmarshal(data, v)
	case string:
		return json.Unmarshal([]byte(data), v)
	default:
		return fmt.Errorf("unsupported Scan type for Volume: %T", src)
	}
}

// VolumeList is a helper type for []Volume to support DB serialization
type VolumeList []Volume

// Value implements driver.Valuer for VolumeList as JSON text
func (vl VolumeList) Value() (driver.Value, error) {
	b, err := json.Marshal(vl)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan implements sql.Scanner for VolumeList from JSON text
func (vl *VolumeList) Scan(src interface{}) error {
	switch data := src.(type) {
	case []byte:
		return json.Unmarshal(data, vl)
	case string:
		return json.Unmarshal([]byte(data), vl)
	default:
		return fmt.Errorf("unsupported Scan type for VolumeList: %T", src)
	}
}

// StringList is a helper type for []string to support DB serialization
type StringList []string

// Value implements driver.Valuer for StringList as JSON text
func (sl StringList) Value() (driver.Value, error) {
	b, err := json.Marshal(sl)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// Scan implements sql.Scanner for StringList from JSON text
func (sl *StringList) Scan(src interface{}) error {
	switch data := src.(type) {
	case []byte:
		return json.Unmarshal(data, sl)
	case string:
		return json.Unmarshal([]byte(data), sl)
	default:
		return fmt.Errorf("unsupported Scan type for StringList: %T", src)
	}
}

// IntervalConfig reflects the JSON structure for sync.interval
type IntervalConfig struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// SyncConfig matches the repo JSON for the sync section
type SyncConfig struct {
	JobName      string            `json:"jobName"`
	Interval     IntervalConfig    `json:"interval"`
	Timeout      string            `json:"timeout"`
	Image        string            `json:"image"`
	Volumes      []Volume          `json:"volumes"`
	Command      []string          `json:"command"`
	Environments []string          `json:"environments"`
	Node         string            `json:"node,omitempty"`
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
}

type Repo struct {
	RepoID     string     `json:"id"`
	Info       Info       `json:"info"`
	SyncParams SyncConfig `json:"sync"`
}

type Job struct {
	RepoID    string    `json:"id"`
	Status    string    `json:"status"` // Waiting, Scheduled, Running, Orphan
	UpdatedAt time.Time `json:"updated_at"`

	LastSuccessAt time.Time `json:"last_success_at"`
	LastFailureAt time.Time `json:"last_failure_at"`
	LastAttemptAt time.Time `json:"last_attempt_at"`

	NextAttemptAt    time.Time `json:"next_attempt_at"`
	LastActionStatus string    `json:"last_action_status"`

	Actions []string `json:"actions"` // last 100 actions
}

type Action struct {
	ID        string    `json:"id"`
	UpdatedAt time.Time `json:"updated_at"`

	JobID   string `json:"job_id"`
	Status  string `json:"status"` // Succeeded, Failed, Running
	Message string `json:"message"`

	ContainerID         string     `json:"container_id"`
	ContainerName       string     `json:"container_name"`
	ContainerImage      string     `json:"container_image"`
	ContainerStatus     string     `json:"container_status"`
	ContainerExitCode   int        `json:"container_exit_code"`
	ContainerExitSignal int        `json:"container_exit_signal"`
	ContainerExitReason string     `json:"container_exit_reason"`
	ContainerVolumes    VolumeList `json:"container_volumes" gorm:"type:TEXT"`
	ContainerEnv        []string   `json:"container_env" gorm:"type:TEXT"`
	ContainerCommand    []string   `json:"container_command" gorm:"type:TEXT"`
	ContainerTimeout    string     `json:"container_timeout"`

	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`

	WorkerName string `json:"worker_name,omitempty"`
}

type Worker struct {
	Name           string            `json:"name"`
	Labels         map[string]string `json:"labels"`
	Vars           map[string]string `json:"vars,omitempty"`
	Status         string            `json:"status"`
	LastHeartbeat  time.Time         `json:"last_heartbeat"`
	RunningActions []string          `json:"running_actions"`
	RegisteredAt   time.Time         `json:"registered_at"`
}

// Job status constants
const (
	JobStatusWaiting   = "Waiting"
	JobStatusScheduled = "Scheduled"
	JobStatusRunning   = "Running"
	JobStatusPaused    = "Paused"
	JobStatusOrphan    = "Orphan"

	ActionStatusRunning     = "Running"
	ActionStatusReconciling = "Reconciling"
	ActionStatusSucceeded   = "Succeeded"
	ActionStatusFailed      = "Failed"

	ContainerStatusRunning = "Running"
	ContainerStatusExited  = "Exited"

	WorkerStatusOnline  = "Online"
	WorkerStatusOffline = "Offline"
)

// ---------------------------------------------------------------------------
// ZFS types
// ---------------------------------------------------------------------------

// ZFSDatasetInfo holds per-dataset ZFS properties.
type ZFSDatasetInfo struct {
	Name              string `json:"name"`
	MountPoint        string `json:"mountpoint"`
	Used              int64  `json:"used"`
	Available         int64  `json:"available"`
	Referenced        int64  `json:"referenced"`
	LogicalUsed       int64  `json:"logicalused"`
	LogicalReferenced int64  `json:"logicalreferenced"`
	CompressRatio     string `json:"compressratio"`
	UsedBySnapshots   int64  `json:"usedbysnapshots"`
	UsedByDataset     int64  `json:"usedbydataset"`
	UsedByChildren    int64  `json:"usedbychildren"`
	Written           int64  `json:"written"`
	Creation          int64  `json:"creation"`
	Quota             int64  `json:"quota"`
	RefQuota          int64  `json:"refquota"`
	Reservation       int64  `json:"reservation"`
	RecordSize        int64  `json:"recordsize"`
	Compression       string `json:"compression"`
	RepoID            string `json:"repo_id,omitempty"`
}

// ZFSSnapshotInfo holds ZFS snapshot metadata.
type ZFSSnapshotInfo struct {
	Name       string `json:"name"`
	Dataset    string `json:"dataset"`
	SnapName   string `json:"snap_name"`
	Used       int64  `json:"used"`
	Referenced int64  `json:"referenced"`
	Creation   int64  `json:"creation"`
}

// ZFSPoolInfo holds zpool-level information.
type ZFSPoolInfo struct {
	Name          string `json:"name"`
	Size          int64  `json:"size"`
	Allocated     int64  `json:"allocated"`
	Free          int64  `json:"free"`
	Fragmentation string `json:"fragmentation"`
	CapacityPct   string `json:"capacity_pct"`
	Dedup         string `json:"dedup"`
	Health        string `json:"health"`
	AltRoot       string `json:"altroot"`
}

// ZFSWorkerReport is the periodic push from worker with all dataset info.
type ZFSWorkerReport struct {
	WorkerName string            `json:"worker_name"`
	Pools      []ZFSPoolInfo     `json:"pools"`
	Datasets   []ZFSDatasetInfo  `json:"datasets"`
	Snapshots  []ZFSSnapshotInfo `json:"snapshots"`
	Timestamp  int64             `json:"timestamp"`
}
