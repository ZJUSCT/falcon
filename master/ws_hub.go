package master

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

// workerConn tracks a single WebSocket connection and its lifecycle.
type workerConn struct {
	conn *websocket.Conn
	wmu  sync.Mutex     // serializes writes to conn
	done chan struct{}   // closed when this connection dies
}

// WSHub accepts WebSocket connections from workers and routes messages.
type WSHub struct {
	mu    sync.RWMutex
	conns map[string]*workerConn // worker name -> active connection
	token string

	OnActionStatus  func(workerName string, msg *shared.WSActionResult)
	OnWorkerWSReady func(workerName string) // called when WS connection is established
	OnWorkerWSLost  func(workerName string) // called when WS connection is lost

	// Pending request-response correlation (global, keyed by reqID).
	pendingMu sync.Mutex
	pending   map[string]chan json.RawMessage
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func NewWSHub(token string) *WSHub {
	return &WSHub{
		conns:   make(map[string]*workerConn),
		token:   token,
		pending: make(map[string]chan json.RawMessage),
	}
}

// HandleWS handles GET /api/internal/ws?name=xxx — upgrades to WebSocket.
func (h *WSHub) HandleWS(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		http.Error(w, "missing name parameter", http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Error().Err(err).Str("worker", name).Msg("websocket upgrade failed")
		return
	}

	log.Info().Str("worker", name).Msg("websocket connected")

	wc := &workerConn{
		conn: conn,
		done: make(chan struct{}),
	}

	h.mu.Lock()
	if old, ok := h.conns[name]; ok {
		// Close old connection — its read loop will exit and run cleanup.
		old.conn.Close()
		log.Warn().Str("worker", name).Msg("closing stale websocket connection")
	}
	h.conns[name] = wc
	h.mu.Unlock()

	// Mark worker as truly online now that WS is ready.
	if h.OnWorkerWSReady != nil {
		h.OnWorkerWSReady(name)
	}

	defer func() {
		h.mu.Lock()
		isCurrentConn := h.conns[name] == wc
		if isCurrentConn {
			delete(h.conns, name)
		}
		h.mu.Unlock()

		close(wc.done)
		conn.Close()

		// Only trigger offline if this was the current connection (not a
		// stale one being replaced by a new WS for the same worker).
		if isCurrentConn && h.OnWorkerWSLost != nil {
			h.OnWorkerWSLost(name)
		}

		log.Info().Str("worker", name).Msg("websocket disconnected")
	}()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Error().Err(err).Str("worker", name).Msg("websocket read error")
			}
			return
		}

		var env shared.WSEnvelope
		if err := json.Unmarshal(data, &env); err != nil {
			log.Warn().Err(err).Str("worker", name).Msg("invalid websocket message")
			continue
		}

		switch env.Type {
		case "action_result":
			var msg shared.WSActionResult
			if err := json.Unmarshal(data, &msg); err != nil {
				log.Warn().Err(err).Str("worker", name).Msg("invalid action_result message")
				continue
			}
			if h.OnActionStatus != nil {
				h.OnActionStatus(name, &msg)
			}

		case "dispatch_result", "query_result", "log_list_result", "log_raw_result":
			h.routeResponse(data, true)

		case "log_stream_data":
			h.routeResponse(data, false)

		case "zfs_get_report_result",
			"zfs_get_datasets_result", "zfs_get_pools_result", "zfs_get_snapshots_result",
			"zfs_create_snapshot_result", "zfs_destroy_snapshot_result",
			"zfs_create_dataset_result",
			"zfs_set_property_result":
			h.routeResponse(data, true)

		default:
			log.Warn().Str("type", env.Type).Str("worker", name).Msg("unknown websocket message type")
		}
	}
}

// routeResponse delivers a response to the waiting caller by reqID.
// Safe to call even if the channel has been closed (uses recover).
func (h *WSHub) routeResponse(data []byte, remove bool) {
	var peek struct {
		ReqID string `json:"req_id"`
	}
	if err := json.Unmarshal(data, &peek); err != nil || peek.ReqID == "" {
		return
	}

	h.pendingMu.Lock()
	ch, ok := h.pending[peek.ReqID]
	if ok && remove {
		delete(h.pending, peek.ReqID)
	}
	h.pendingMu.Unlock()

	if !ok {
		return
	}

	// Recover from send-on-closed-channel if the connection was closed
	// between our lock release and this send.
	defer func() { recover() }()
	select {
	case ch <- json.RawMessage(data):
	default:
	}
}

// getWorkerConn returns the active connection for a worker.
func (h *WSHub) getWorkerConn(workerName string) (*workerConn, error) {
	h.mu.RLock()
	wc, ok := h.conns[workerName]
	h.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("worker %s not connected", workerName)
	}
	return wc, nil
}

// writeJSON sends a JSON message to the named worker with proper locking.
func (h *WSHub) writeJSON(workerName string, v interface{}) error {
	wc, err := h.getWorkerConn(workerName)
	if err != nil {
		return err
	}
	wc.wmu.Lock()
	err = wc.conn.WriteJSON(v)
	wc.wmu.Unlock()
	return err
}

// SendAck sends an ack for a given action (fire-and-forget).
func (h *WSHub) SendAck(workerName, actionID string) {
	ack := shared.WSAck{Type: "ack", ActionID: actionID}
	if err := h.writeJSON(workerName, ack); err != nil {
		log.Error().Err(err).Str("worker", workerName).Str("action", actionID).Msg("failed to send ack")
	}
}

// request sends a message and waits for a correlated response.
// Returns error if the worker disconnects or the timeout expires.
func (h *WSHub) request(workerName string, msg interface{}, reqID string, timeout time.Duration) (json.RawMessage, error) {
	wc, err := h.getWorkerConn(workerName)
	if err != nil {
		return nil, err
	}

	ch := make(chan json.RawMessage, 1)

	h.pendingMu.Lock()
	h.pending[reqID] = ch
	h.pendingMu.Unlock()

	cleanup := func() {
		h.pendingMu.Lock()
		delete(h.pending, reqID)
		h.pendingMu.Unlock()
	}

	// Write on the captured connection (not by worker name) to ensure the
	// request and the done channel belong to the same connection.
	wc.wmu.Lock()
	writeErr := wc.conn.WriteJSON(msg)
	wc.wmu.Unlock()
	if writeErr != nil {
		cleanup()
		return nil, fmt.Errorf("send to %s: %w", workerName, writeErr)
	}

	select {
	case resp := <-ch:
		return resp, nil
	case <-wc.done:
		cleanup()
		return nil, fmt.Errorf("worker %s disconnected", workerName)
	case <-time.After(timeout):
		cleanup()
		return nil, fmt.Errorf("request to %s timed out after %s", workerName, timeout)
	}
}

// DispatchRejectedError indicates the worker explicitly rejected the dispatch
// (e.g. container start failed). This is distinct from communication errors
// where the container may have started.
type DispatchRejectedError struct {
	Worker string
	Reason string
}

func (e *DispatchRejectedError) Error() string {
	return fmt.Sprintf("worker %s rejected dispatch: %s", e.Worker, e.Reason)
}

func genReqID() string {
	return fmt.Sprintf("%d-%04x", time.Now().UnixNano(), rand.Intn(0xFFFF))
}

// Dispatch sends a dispatch request to a worker and waits for the response.
func (h *WSHub) Dispatch(workerName string, action shared.DispatchAction) error {
	reqID := genReqID()
	msg := shared.WSDispatch{
		Type:   "dispatch",
		ReqID:  reqID,
		Action: action,
	}

	data, err := h.request(workerName, msg, reqID, 30*time.Second)
	if err != nil {
		return err
	}

	var result shared.WSDispatchResult
	if err := json.Unmarshal(data, &result); err != nil {
		return fmt.Errorf("unmarshal dispatch result from %s: %w", workerName, err)
	}
	if !result.OK {
		return &DispatchRejectedError{Worker: workerName, Reason: result.Message}
	}
	return nil
}

// QueryActionStatus queries a worker for the status of a specific action.
func (h *WSHub) QueryActionStatus(workerName, actionID string) (*shared.ActionStatusResponse, error) {
	reqID := genReqID()
	msg := shared.WSQueryAction{
		Type:     "query_action",
		ReqID:    reqID,
		ActionID: actionID,
	}

	data, err := h.request(workerName, msg, reqID, 10*time.Second)
	if err != nil {
		return nil, err
	}

	var result shared.WSQueryResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("unmarshal query result from %s: %w", workerName, err)
	}
	return &result.Response, nil
}

// LogList requests a log file listing from a worker.
func (h *WSHub) LogList(workerName, actionID string) (json.RawMessage, error) {
	reqID := genReqID()
	msg := shared.WSLogList{
		Type:     "log_list",
		ReqID:    reqID,
		ActionID: actionID,
	}

	data, err := h.request(workerName, msg, reqID, 10*time.Second)
	if err != nil {
		return nil, err
	}

	var result shared.WSLogListResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("%s", result.Error)
	}
	return result.Entries, nil
}

// LogRaw requests the content of a log file from a worker.
func (h *WSHub) LogRaw(workerName, actionID, file string) (string, error) {
	reqID := genReqID()
	msg := shared.WSLogRaw{
		Type:     "log_raw",
		ReqID:    reqID,
		ActionID: actionID,
		File:     file,
	}

	data, err := h.request(workerName, msg, reqID, 30*time.Second)
	if err != nil {
		return "", err
	}

	var result shared.WSLogRawResult
	if err := json.Unmarshal(data, &result); err != nil {
		return "", err
	}
	if !result.OK {
		return "", fmt.Errorf("%s", result.Error)
	}
	return result.Data, nil
}

// LogStream starts a log stream from a worker. Returns a data channel
// and a stop function. The data channel is closed when stop is called
// or when the worker disconnects.
func (h *WSHub) LogStream(workerName, actionID, file string) (string, <-chan string, func(), error) {
	wc, err := h.getWorkerConn(workerName)
	if err != nil {
		return "", nil, nil, err
	}

	reqID := genReqID()
	msg := shared.WSLogStreamStart{
		Type:     "log_stream_start",
		ReqID:    reqID,
		ActionID: actionID,
		File:     file,
	}

	ch := make(chan json.RawMessage, 256)
	dataCh := make(chan string, 256)
	doneCh := make(chan struct{})

	h.pendingMu.Lock()
	h.pending[reqID] = ch
	h.pendingMu.Unlock()

	if err := h.writeJSON(workerName, msg); err != nil {
		h.pendingMu.Lock()
		delete(h.pending, reqID)
		h.pendingMu.Unlock()
		return "", nil, nil, err
	}

	// Forwarder: reads raw chunks from ch, parses, and sends to dataCh.
	// Exits on doneCh (stop called) or wc.done (worker disconnected).
	go func() {
		defer close(dataCh)
		for {
			select {
			case <-doneCh:
				return
			case <-wc.done:
				return
			case raw, ok := <-ch:
				if !ok {
					return
				}
				var chunk shared.WSLogStreamData
				if err := json.Unmarshal(raw, &chunk); err != nil {
					continue
				}
				select {
				case dataCh <- chunk.Data:
				case <-doneCh:
					return
				case <-wc.done:
					return
				}
			}
		}
	}()

	stop := func() {
		close(doneCh)
		stopMsg := shared.WSLogStreamStop{Type: "log_stream_stop", ReqID: reqID}
		_ = h.writeJSON(workerName, stopMsg)
		h.pendingMu.Lock()
		delete(h.pending, reqID)
		h.pendingMu.Unlock()
	}

	return reqID, dataCh, stop, nil
}

// IsConnected reports whether the named worker has an active WebSocket connection.
func (h *WSHub) IsConnected(workerName string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.conns[workerName]
	return ok
}

// ---------------------------------------------------------------------------
// ZFS proxy methods
// ---------------------------------------------------------------------------

func (h *WSHub) ZFSGetReport(workerName string) (*shared.ZFSWorkerReport, error) {
	reqID := genReqID()
	data, err := h.request(workerName, shared.WSZFSGetReport{Type: "zfs_get_report", ReqID: reqID}, reqID, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var result shared.WSZFSGetReportResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("%s", result.Error)
	}
	return &result.Report, nil
}

func (h *WSHub) ZFSGetDatasets(workerName string) ([]shared.ZFSDatasetInfo, error) {
	reqID := genReqID()
	data, err := h.request(workerName, shared.WSZFSGetDatasets{Type: "zfs_get_datasets", ReqID: reqID}, reqID, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var result shared.WSZFSGetDatasetsResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("%s", result.Error)
	}
	return result.Datasets, nil
}

func (h *WSHub) ZFSGetPools(workerName string) ([]shared.ZFSPoolInfo, error) {
	reqID := genReqID()
	data, err := h.request(workerName, shared.WSZFSGetPools{Type: "zfs_get_pools", ReqID: reqID}, reqID, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var result shared.WSZFSGetPoolsResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("%s", result.Error)
	}
	return result.Pools, nil
}

func (h *WSHub) ZFSGetSnapshots(workerName, dataset string) ([]shared.ZFSSnapshotInfo, error) {
	reqID := genReqID()
	data, err := h.request(workerName, shared.WSZFSGetSnapshots{Type: "zfs_get_snapshots", ReqID: reqID, Dataset: dataset}, reqID, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var result shared.WSZFSGetSnapshotsResult
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if !result.OK {
		return nil, fmt.Errorf("%s", result.Error)
	}
	return result.Snapshots, nil
}

func (h *WSHub) ZFSCreateSnapshot(workerName, dataset, snapName string, recursive bool) error {
	reqID := genReqID()
	data, err := h.request(workerName, shared.WSZFSCreateSnapshot{
		Type: "zfs_create_snapshot", ReqID: reqID,
		Dataset: dataset, SnapName: snapName, Recursive: recursive,
	}, reqID, 30*time.Second)
	if err != nil {
		return err
	}
	var result shared.WSZFSCreateSnapshotResult
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("%s", result.Error)
	}
	return nil
}

func (h *WSHub) ZFSDestroySnapshot(workerName, snapshot string) error {
	reqID := genReqID()
	data, err := h.request(workerName, shared.WSZFSDestroySnapshot{
		Type: "zfs_destroy_snapshot", ReqID: reqID, Snapshot: snapshot,
	}, reqID, 30*time.Second)
	if err != nil {
		return err
	}
	var result shared.WSZFSDestroySnapshotResult
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("%s", result.Error)
	}
	return nil
}

func (h *WSHub) ZFSCreateDataset(workerName, name string, properties map[string]string) error {
	reqID := genReqID()
	data, err := h.request(workerName, shared.WSZFSCreateDataset{
		Type: "zfs_create_dataset", ReqID: reqID, Name: name, Properties: properties,
	}, reqID, 30*time.Second)
	if err != nil {
		return err
	}
	var result shared.WSZFSCreateDatasetResult
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("%s", result.Error)
	}
	return nil
}

func (h *WSHub) ZFSSetProperty(workerName, dataset, property, value string) error {
	reqID := genReqID()
	data, err := h.request(workerName, shared.WSZFSSetProperty{
		Type: "zfs_set_property", ReqID: reqID,
		Dataset: dataset, Property: property, Value: value,
	}, reqID, 30*time.Second)
	if err != nil {
		return err
	}
	var result shared.WSZFSSetPropertyResult
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("%s", result.Error)
	}
	return nil
}
