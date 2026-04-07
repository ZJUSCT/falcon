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
// It does NOT buffer messages internally — on reconnect it replays all
// PendingAck actions from the Tracker, which is the single source of truth.
type WSClient struct {
	mu        sync.Mutex
	conn      *websocket.Conn
	masterURL string
	name      string
	token     string

	writeMu sync.Mutex // protects all writes to conn

	tracker *Tracker              // used to replay PendingAck on reconnect
	OnAck   func(actionID string) // callback when Master acks an action
}

func NewWSClient(masterURL, name, token string, tracker *Tracker) *WSClient {
	return &WSClient{
		masterURL: masterURL,
		name:      name,
		token:     token,
		tracker:   tracker,
	}
}

// ConnectLoop runs an infinite reconnect loop: connect, replay pending acks,
// read (blocks), then retry with exponential backoff on disconnect.
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

		// Replay all PendingAck results — the tracker is the reliable source.
		ws.replayPendingAcks()

		ws.readLoop()

		// readLoop returned — connection lost.
		ws.mu.Lock()
		if ws.conn != nil {
			_ = ws.conn.Close()
			ws.conn = nil
		}
		ws.mu.Unlock()

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

		var ack shared.WSAck
		if err := json.Unmarshal(data, &ack); err != nil {
			log.Warn().Err(err).Msg("WebSocket: failed to parse message from master")
			continue
		}

		if ack.Type == "ack" && ws.OnAck != nil {
			ws.OnAck(ack.ActionID)
		}
	}
}

// writeMessage sends raw data while holding the write mutex.
func (ws *WSClient) writeMessage(conn *websocket.Conn, data []byte) error {
	ws.writeMu.Lock()
	defer ws.writeMu.Unlock()
	return conn.WriteMessage(websocket.TextMessage, data)
}

// SendResult sends an action result message to the master.
// If the connection is down, the message is NOT buffered — the tracker
// retains the PendingAck state and replayPendingAcks will resend on reconnect.
func (ws *WSClient) SendResult(act *shared.Action) {
	msg := &shared.WSMessage{
		Type:            "action_result",
		ActionID:        act.ID,
		Status:          act.Status,
		ContainerStatus: shared.ContainerStatusExited,
		ExitCode:        act.ContainerExitCode,
		ExitReason:      act.ContainerExitReason,
		UpdatedAt:       time.Now(),
	}

	ws.mu.Lock()
	conn := ws.conn
	ws.mu.Unlock()

	if conn == nil {
		log.Debug().Str("action", act.ID).Msg("WebSocket not connected, result will be replayed on reconnect")
		return
	}

	data, err := json.Marshal(msg)
	if err != nil {
		log.Error().Err(err).Msg("WebSocket: failed to marshal message")
		return
	}
	if err := ws.writeMessage(conn, data); err != nil {
		log.Warn().Err(err).Str("action", act.ID).Msg("WebSocket write failed, result will be replayed on reconnect")
	}
}

// replayPendingAcks resends results for all PendingAck actions after reconnect.
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
