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
