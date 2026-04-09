package worker

import (
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

// WSClient manages the WebSocket connection to the master.
// All master↔worker communication (dispatch, query, ack, action results,
// logs) flows through this single connection.
type WSClient struct {
	mu        sync.Mutex
	conn      *websocket.Conn
	masterURL string
	name      string
	token     string

	writeMu sync.Mutex // protects all writes to conn

	tracker *Tracker
	OnAck   func(actionID string)

	// OnDispatch is called when master sends a dispatch request.
	OnDispatch func(action shared.DispatchAction) (ok bool, message string)
}

func NewWSClient(masterURL, name, token string, tracker *Tracker) *WSClient {
	return &WSClient{
		masterURL: masterURL,
		name:      name,
		token:     token,
		tracker:   tracker,
	}
}

// ConnectLoop runs an infinite reconnect loop.
func (ws *WSClient) ConnectLoop() {
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		err := ws.connect()
		if err != nil {
			log.Error().Err(err).Dur("backoff", backoff).Msg("WebSocket connect failed, retrying")
			time.Sleep(backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		backoff = time.Second
		log.Info().Str("master", ws.masterURL).Msg("WebSocket connected to master")

		// Replay all PendingAck results.
		ws.replayPendingAcks()

		ws.readLoop()

		ws.mu.Lock()
		if ws.conn != nil {
			_ = ws.conn.Close()
			ws.conn = nil
		}
		ws.mu.Unlock()

		// Stop all active log streams — they can't deliver data anymore.
		stopAllStreams()

		log.Warn().Msg("WebSocket disconnected from master, reconnecting")
	}
}

func (ws *WSClient) connect() error {
	u := ws.masterURL
	u = strings.Replace(u, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	u = strings.TrimRight(u, "/")
	u += "/api/internal/ws?name=" + ws.name + "&token=" + ws.token

	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		return err
	}

	ws.mu.Lock()
	ws.conn = conn
	ws.mu.Unlock()
	return nil
}

func (ws *WSClient) readLoop() {
	for {
		ws.mu.Lock()
		conn := ws.conn
		ws.mu.Unlock()
		if conn == nil {
			return
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			log.Warn().Err(err).Msg("WebSocket read error")
			return
		}

		var env shared.WSEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			log.Warn().Err(err).Msg("WebSocket: failed to parse message")
			continue
		}

		switch env.Type {
		case "ack":
			var ack shared.WSAck
			if err := json.Unmarshal(data, &ack); err == nil && ws.OnAck != nil {
				ws.OnAck(ack.ActionID)
			}

		case "dispatch":
			var msg shared.WSDispatch
			if err := json.Unmarshal(data, &msg); err != nil {
				log.Warn().Err(err).Msg("WebSocket: invalid dispatch message")
				continue
			}
			go ws.handleDispatch(msg)

		case "query_action":
			var msg shared.WSQueryAction
			if err := json.Unmarshal(data, &msg); err != nil {
				log.Warn().Err(err).Msg("WebSocket: invalid query_action message")
				continue
			}
			go ws.handleQueryAction(msg)

		case "log_list":
			var msg shared.WSLogList
			if err := json.Unmarshal(data, &msg); err == nil {
				go ws.handleLogList(msg)
			}

		case "log_raw":
			var msg shared.WSLogRaw
			if err := json.Unmarshal(data, &msg); err == nil {
				go ws.handleLogRaw(msg)
			}

		case "log_stream_start":
			var msg shared.WSLogStreamStart
			if err := json.Unmarshal(data, &msg); err == nil {
				go ws.handleLogStream(msg)
			}

		case "log_stream_stop":
			// Handled by cancellation in handleLogStream via streamMu.
			ws.stopStream(data)

		case "zfs_get_report":
			var msg shared.WSZFSGetReport
			if err := json.Unmarshal(data, &msg); err == nil {
				go ws.handleZFSGetReport(msg)
			}
		case "zfs_get_datasets":
			var msg shared.WSZFSGetDatasets
			if err := json.Unmarshal(data, &msg); err == nil {
				go ws.handleZFSGetDatasets(msg)
			}
		case "zfs_get_pools":
			var msg shared.WSZFSGetPools
			if err := json.Unmarshal(data, &msg); err == nil {
				go ws.handleZFSGetPools(msg)
			}
		case "zfs_get_snapshots":
			var msg shared.WSZFSGetSnapshots
			if err := json.Unmarshal(data, &msg); err == nil {
				go ws.handleZFSGetSnapshots(msg)
			}
		case "zfs_create_snapshot":
			var msg shared.WSZFSCreateSnapshot
			if err := json.Unmarshal(data, &msg); err == nil {
				go ws.handleZFSCreateSnapshot(msg)
			}
		case "zfs_destroy_snapshot":
			var msg shared.WSZFSDestroySnapshot
			if err := json.Unmarshal(data, &msg); err == nil {
				go ws.handleZFSDestroySnapshot(msg)
			}
		case "zfs_create_dataset":
			var msg shared.WSZFSCreateDataset
			if err := json.Unmarshal(data, &msg); err == nil {
				go ws.handleZFSCreateDataset(msg)
			}
		case "zfs_set_property":
			var msg shared.WSZFSSetProperty
			if err := json.Unmarshal(data, &msg); err == nil {
				go ws.handleZFSSetProperty(msg)
			}

		default:
			log.Warn().Str("type", env.Type).Msg("WebSocket: unknown message type from master")
		}
	}
}

// writeMessage sends raw data with proper locking.
func (ws *WSClient) writeMessage(conn *websocket.Conn, data []byte) error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	return conn.WriteMessage(websocket.TextMessage, data)
}

// sendJSON marshals and sends a JSON message.
func (ws *WSClient) sendJSON(v interface{}) error {
	ws.mu.Lock()
	conn := ws.conn
	ws.mu.Unlock()
	if conn == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return ws.writeMessage(conn, data)
}

// SendResult sends an action result message to the master.
func (ws *WSClient) SendResult(act *shared.Action) {
	msg := &shared.WSActionResult{
		Type:            "action_result",
		ActionID:        act.ID,
		Status:          act.Status,
		ContainerStatus: shared.ContainerStatusExited,
		ExitCode:        act.ContainerExitCode,
		ExitReason:      act.ContainerExitReason,
		UpdatedAt:       time.Now(),
	}

	if err := ws.sendJSON(msg); err != nil {
		log.Warn().Err(err).Str("action", act.ID).Msg("WebSocket send failed, result will be replayed on reconnect")
	}
}

func (ws *WSClient) replayPendingAcks() {
	pending := ws.tracker.PendingAckActions()
	if len(pending) == 0 {
		return
	}
	log.Info().Int("count", len(pending)).Msg("Replaying PendingAck results after reconnect")
	for _, act := range pending {
		ws.SendResult(act)
	}
}

// ---------------------------------------------------------------------------
// Request handlers (master → worker)
// ---------------------------------------------------------------------------

func (ws *WSClient) handleDispatch(msg shared.WSDispatch) {
	ok, message := false, "no dispatch handler"
	if ws.OnDispatch != nil {
		ok, message = ws.OnDispatch(msg.Action)
	}
	_ = ws.sendJSON(shared.WSDispatchResult{
		Type:    "dispatch_result",
		ReqID:   msg.ReqID,
		OK:      ok,
		Message: message,
	})
}

func (ws *WSClient) handleQueryAction(msg shared.WSQueryAction) {
	resp := ws.tracker.ToStatusResponse(msg.ActionID)
	// Fallback to action cache if not in tracker.
	if !resp.Found && actionCache != nil {
		resp = actionCache.ToStatusResponse(msg.ActionID)
	}
	_ = ws.sendJSON(shared.WSQueryResult{
		Type:     "query_result",
		ReqID:    msg.ReqID,
		Response: *resp,
	})
}

// ---------------------------------------------------------------------------
// Log request handlers
// ---------------------------------------------------------------------------

func (ws *WSClient) handleLogList(msg shared.WSLogList) {
	entries, err := listLogDir(msg.ActionID)
	if err != nil {
		_ = ws.sendJSON(shared.WSLogListResult{
			Type:  "log_list_result",
			ReqID: msg.ReqID,
			OK:    false,
			Error: err.Error(),
		})
		return
	}
	data, _ := json.Marshal(entries)
	_ = ws.sendJSON(shared.WSLogListResult{
		Type:    "log_list_result",
		ReqID:   msg.ReqID,
		OK:      true,
		Entries: data,
	})
}

func (ws *WSClient) handleLogRaw(msg shared.WSLogRaw) {
	content, err := readLogFile(msg.ActionID, msg.File)
	if err != nil {
		_ = ws.sendJSON(shared.WSLogRawResult{
			Type:  "log_raw_result",
			ReqID: msg.ReqID,
			OK:    false,
			Error: err.Error(),
		})
		return
	}
	_ = ws.sendJSON(shared.WSLogRawResult{
		Type:  "log_raw_result",
		ReqID: msg.ReqID,
		OK:    true,
		Data:  content,
	})
}

// Stream management.
var (
	streamMu     sync.Mutex
	activeStreams = make(map[string]chan struct{}) // reqID -> stop channel
)

func (ws *WSClient) stopStream(data []byte) {
	var msg shared.WSLogStreamStop
	if err := json.Unmarshal(data, &msg); err != nil {
		return
	}
	streamMu.Lock()
	if ch, ok := activeStreams[msg.ReqID]; ok {
		close(ch)
		delete(activeStreams, msg.ReqID)
	}
	streamMu.Unlock()
}

// stopAllStreams stops all active log streams (called on WS disconnect).
func stopAllStreams() {
	streamMu.Lock()
	for reqID, ch := range activeStreams {
		close(ch)
		delete(activeStreams, reqID)
	}
	streamMu.Unlock()
}

// ---------------------------------------------------------------------------
// ZFS request handlers
// ---------------------------------------------------------------------------

func (ws *WSClient) handleZFSGetReport(msg shared.WSZFSGetReport) {
	report := BuildZFSReport(ws.name)
	_ = ws.sendJSON(shared.WSZFSGetReportResult{
		Type: "zfs_get_report_result", ReqID: msg.ReqID, OK: true, Report: report,
	})
}

func (ws *WSClient) handleZFSGetDatasets(msg shared.WSZFSGetDatasets) {
	datasets, err := ListDatasets("")
	if err != nil {
		_ = ws.sendJSON(shared.WSZFSGetDatasetsResult{
			Type: "zfs_get_datasets_result", ReqID: msg.ReqID, OK: false, Error: err.Error(),
		})
		return
	}
	_ = ws.sendJSON(shared.WSZFSGetDatasetsResult{
		Type: "zfs_get_datasets_result", ReqID: msg.ReqID, OK: true, Datasets: datasets,
	})
}

func (ws *WSClient) handleZFSGetPools(msg shared.WSZFSGetPools) {
	pools, err := ListPools()
	if err != nil {
		_ = ws.sendJSON(shared.WSZFSGetPoolsResult{
			Type: "zfs_get_pools_result", ReqID: msg.ReqID, OK: false, Error: err.Error(),
		})
		return
	}
	_ = ws.sendJSON(shared.WSZFSGetPoolsResult{
		Type: "zfs_get_pools_result", ReqID: msg.ReqID, OK: true, Pools: pools,
	})
}

func (ws *WSClient) handleZFSGetSnapshots(msg shared.WSZFSGetSnapshots) {
	snaps, err := ListSnapshots(msg.Dataset)
	if err != nil {
		_ = ws.sendJSON(shared.WSZFSGetSnapshotsResult{
			Type: "zfs_get_snapshots_result", ReqID: msg.ReqID, OK: false, Error: err.Error(),
		})
		return
	}
	_ = ws.sendJSON(shared.WSZFSGetSnapshotsResult{
		Type: "zfs_get_snapshots_result", ReqID: msg.ReqID, OK: true, Snapshots: snaps,
	})
}

func (ws *WSClient) handleZFSCreateSnapshot(msg shared.WSZFSCreateSnapshot) {
	err := CreateSnapshot(msg.Dataset, msg.SnapName, msg.Recursive)
	resp := shared.WSZFSCreateSnapshotResult{
		Type: "zfs_create_snapshot_result", ReqID: msg.ReqID, OK: err == nil,
	}
	if err != nil {
		resp.Error = err.Error()
	}
	_ = ws.sendJSON(resp)
}

func (ws *WSClient) handleZFSDestroySnapshot(msg shared.WSZFSDestroySnapshot) {
	err := DestroySnapshot(msg.Snapshot)
	resp := shared.WSZFSDestroySnapshotResult{
		Type: "zfs_destroy_snapshot_result", ReqID: msg.ReqID, OK: err == nil,
	}
	if err != nil {
		resp.Error = err.Error()
	}
	_ = ws.sendJSON(resp)
}

func (ws *WSClient) handleZFSCreateDataset(msg shared.WSZFSCreateDataset) {
	err := CreateDataset(msg.Name, msg.Properties)
	resp := shared.WSZFSCreateDatasetResult{
		Type: "zfs_create_dataset_result", ReqID: msg.ReqID, OK: err == nil,
	}
	if err != nil {
		resp.Error = err.Error()
	}
	_ = ws.sendJSON(resp)
}

func (ws *WSClient) handleZFSSetProperty(msg shared.WSZFSSetProperty) {
	err := SetProperty(msg.Dataset, msg.Property, msg.Value)
	resp := shared.WSZFSSetPropertyResult{
		Type: "zfs_set_property_result", ReqID: msg.ReqID, OK: err == nil,
	}
	if err != nil {
		resp.Error = err.Error()
	}
	_ = ws.sendJSON(resp)
}

func (ws *WSClient) handleLogStream(msg shared.WSLogStreamStart) {
	stopCh := make(chan struct{})
	streamMu.Lock()
	activeStreams[msg.ReqID] = stopCh
	streamMu.Unlock()

	defer func() {
		streamMu.Lock()
		delete(activeStreams, msg.ReqID)
		streamMu.Unlock()
	}()

	streamLogFile(msg.ActionID, msg.File, msg.ReqID, stopCh, ws)
}
