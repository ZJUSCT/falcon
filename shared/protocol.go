package shared

import (
	"encoding/json"
	"time"
)

// Worker -> Master: Registration
type RegisterRequest struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Vars   map[string]string `json:"vars,omitempty"`
}

// Deprecated HTTP dispatch types (kept for backward compat during migration).
type DispatchRequest struct {
	Action DispatchAction `json:"action"`
}

type DispatchResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

type RegisterResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// Worker -> Master: Heartbeat
type HeartbeatRequest struct {
	Name           string   `json:"name"`
	RunningActions []string `json:"running_actions"`
}

type HeartbeatResponse struct {
	OK bool `json:"ok"`
}

// Dispatch action payload (used in both HTTP and WS dispatch)
type DispatchAction struct {
	ID               string   `json:"id"`
	JobID            string   `json:"job_id"`
	ContainerImage   string   `json:"container_image"`
	ContainerCommand []string `json:"container_command"`
	ContainerVolumes []Volume `json:"container_volumes"`
	ContainerEnv     []string `json:"container_env"`
	ContainerTimeout string   `json:"container_timeout"`
}

// ---------------------------------------------------------------------------
// WebSocket message types (all communication over worker-initiated WS)
// ---------------------------------------------------------------------------

// WSEnvelope is used to peek at the "type" field before full unmarshal.
type WSEnvelope struct {
	Type string `json:"type"`
}

// Worker -> Master: action result
type WSActionResult struct {
	Type            string    `json:"type"` // "action_result"
	ActionID        string    `json:"action_id"`
	Status          string    `json:"status"`
	ContainerStatus string    `json:"container_status"`
	ExitCode        int       `json:"exit_code"`
	ExitReason      string    `json:"exit_reason"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// Master -> Worker: ack
type WSAck struct {
	Type     string `json:"type"` // "ack"
	ActionID string `json:"action_id"`
}

// Master -> Worker: dispatch request
type WSDispatch struct {
	Type   string         `json:"type"` // "dispatch"
	ReqID  string         `json:"req_id"`
	Action DispatchAction `json:"action"`
}

// Worker -> Master: dispatch response
type WSDispatchResult struct {
	Type    string `json:"type"` // "dispatch_result"
	ReqID   string `json:"req_id"`
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// Master -> Worker: query action status
type WSQueryAction struct {
	Type     string `json:"type"` // "query_action"
	ReqID    string `json:"req_id"`
	ActionID string `json:"action_id"`
}

// Worker -> Master: query action response
type WSQueryResult struct {
	Type     string               `json:"type"` // "query_result"
	ReqID    string               `json:"req_id"`
	Response ActionStatusResponse `json:"response"`
}

// Master -> Worker: log list request
type WSLogList struct {
	Type     string `json:"type"` // "log_list"
	ReqID    string `json:"req_id"`
	ActionID string `json:"action_id"`
}

// Worker -> Master: log list response
type WSLogListResult struct {
	Type    string          `json:"type"` // "log_list_result"
	ReqID   string          `json:"req_id"`
	OK      bool            `json:"ok"`
	Error   string          `json:"error,omitempty"`
	Entries json.RawMessage `json:"entries,omitempty"` // []logEntry as JSON
}

// Master -> Worker: log raw request
type WSLogRaw struct {
	Type     string `json:"type"` // "log_raw"
	ReqID    string `json:"req_id"`
	ActionID string `json:"action_id"`
	File     string `json:"file"`
}

// Worker -> Master: log raw response
type WSLogRawResult struct {
	Type  string `json:"type"` // "log_raw_result"
	ReqID string `json:"req_id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Data  string `json:"data,omitempty"` // file content
}

// Master -> Worker: start log stream
type WSLogStreamStart struct {
	Type     string `json:"type"` // "log_stream_start"
	ReqID    string `json:"req_id"`
	ActionID string `json:"action_id"`
	File     string `json:"file"`
}

// Worker -> Master: log stream data chunk
type WSLogStreamData struct {
	Type  string `json:"type"` // "log_stream_data"
	ReqID string `json:"req_id"`
	Data  string `json:"data"`
}

// Master -> Worker: stop log stream
type WSLogStreamStop struct {
	Type  string `json:"type"` // "log_stream_stop"
	ReqID string `json:"req_id"`
}

// Action status query response (shared by WS and direct lookup)
type ActionStatusResponse struct {
	Found      bool      `json:"found"`
	ActionID   string    `json:"action_id"`
	Status     string    `json:"status"`
	ExitCode   int       `json:"exit_code"`
	ExitReason string    `json:"exit_reason"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// ---------------------------------------------------------------------------
// ZFS WebSocket message types
// ---------------------------------------------------------------------------

// Master -> Worker: request full ZFS report
type WSZFSGetReport struct {
	Type  string `json:"type"` // "zfs_get_report"
	ReqID string `json:"req_id"`
}

type WSZFSGetReportResult struct {
	Type   string          `json:"type"` // "zfs_get_report_result"
	ReqID  string          `json:"req_id"`
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Report ZFSWorkerReport `json:"report,omitempty"`
}

// Master -> Worker: request ZFS datasets
type WSZFSGetDatasets struct {
	Type  string `json:"type"` // "zfs_get_datasets"
	ReqID string `json:"req_id"`
}

type WSZFSGetDatasetsResult struct {
	Type     string           `json:"type"` // "zfs_get_datasets_result"
	ReqID    string           `json:"req_id"`
	OK       bool             `json:"ok"`
	Error    string           `json:"error,omitempty"`
	Datasets []ZFSDatasetInfo `json:"datasets,omitempty"`
}

// Master -> Worker: request ZFS pools
type WSZFSGetPools struct {
	Type  string `json:"type"` // "zfs_get_pools"
	ReqID string `json:"req_id"`
}

type WSZFSGetPoolsResult struct {
	Type  string        `json:"type"` // "zfs_get_pools_result"
	ReqID string        `json:"req_id"`
	OK    bool          `json:"ok"`
	Error string        `json:"error,omitempty"`
	Pools []ZFSPoolInfo `json:"pools,omitempty"`
}

// Master -> Worker: request ZFS snapshots
type WSZFSGetSnapshots struct {
	Type    string `json:"type"` // "zfs_get_snapshots"
	ReqID   string `json:"req_id"`
	Dataset string `json:"dataset"`
}

type WSZFSGetSnapshotsResult struct {
	Type      string            `json:"type"` // "zfs_get_snapshots_result"
	ReqID     string            `json:"req_id"`
	OK        bool              `json:"ok"`
	Error     string            `json:"error,omitempty"`
	Snapshots []ZFSSnapshotInfo `json:"snapshots,omitempty"`
}

// Master -> Worker: create ZFS snapshot
type WSZFSCreateSnapshot struct {
	Type      string `json:"type"` // "zfs_create_snapshot"
	ReqID     string `json:"req_id"`
	Dataset   string `json:"dataset"`
	SnapName  string `json:"snap_name"`
	Recursive bool   `json:"recursive"`
}

type WSZFSCreateSnapshotResult struct {
	Type  string `json:"type"` // "zfs_create_snapshot_result"
	ReqID string `json:"req_id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Master -> Worker: destroy ZFS snapshot
type WSZFSDestroySnapshot struct {
	Type     string `json:"type"` // "zfs_destroy_snapshot"
	ReqID    string `json:"req_id"`
	Snapshot string `json:"snapshot"`
}

type WSZFSDestroySnapshotResult struct {
	Type  string `json:"type"` // "zfs_destroy_snapshot_result"
	ReqID string `json:"req_id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Master -> Worker: create ZFS dataset
type WSZFSCreateDataset struct {
	Type       string            `json:"type"` // "zfs_create_dataset"
	ReqID      string            `json:"req_id"`
	Name       string            `json:"name"`
	Properties map[string]string `json:"properties,omitempty"`
}

type WSZFSCreateDatasetResult struct {
	Type  string `json:"type"` // "zfs_create_dataset_result"
	ReqID string `json:"req_id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Master -> Worker: set ZFS dataset property
type WSZFSSetProperty struct {
	Type     string `json:"type"` // "zfs_set_property"
	ReqID    string `json:"req_id"`
	Dataset  string `json:"dataset"`
	Property string `json:"property"`
	Value    string `json:"value"`
}

type WSZFSSetPropertyResult struct {
	Type  string `json:"type"` // "zfs_set_property_result"
	ReqID string `json:"req_id"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}
