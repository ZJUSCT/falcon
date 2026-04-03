package shared

import "time"

// Worker -> Master: Registration
type RegisterRequest struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Addr   string            `json:"addr"`
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

// Master -> Worker: Dispatch
type DispatchRequest struct {
	Action DispatchAction `json:"action"`
}

type DispatchAction struct {
	ID               string   `json:"id"`
	JobID            string   `json:"job_id"`
	ContainerImage   string   `json:"container_image"`
	ContainerCommand []string `json:"container_command"`
	ContainerVolumes []Volume `json:"container_volumes"`
	ContainerEnv     []string `json:"container_env"`
	ContainerTimeout string   `json:"container_timeout"`
}

type DispatchResponse struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// WebSocket: Worker -> Master
type WSMessage struct {
	Type            string    `json:"type"`
	ActionID        string    `json:"action_id"`
	Status          string    `json:"status"`
	ContainerStatus string    `json:"container_status"`
	ExitCode        int       `json:"exit_code"`
	ExitReason      string    `json:"exit_reason"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// WebSocket: Master -> Worker
type WSAck struct {
	Type     string `json:"type"`     // "ack"
	ActionID string `json:"action_id"`
}

// Master -> Worker: Action Status Query
type ActionStatusResponse struct {
	Found      bool      `json:"found"`
	ActionID   string    `json:"action_id"`
	Status     string    `json:"status"`
	ExitCode   int       `json:"exit_code"`
	ExitReason string    `json:"exit_reason"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}
