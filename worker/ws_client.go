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

type WSClient struct {
	mu        sync.Mutex
	conn      *websocket.Conn
	masterURL string // e.g. "http://master:8080"
	name      string
	token     string

	bufMu  sync.Mutex
	buffer []*shared.WSMessage

	OnAck func(actionID string) // callback when Master acks an action
}

func NewWSClient(masterURL, name, token string) *WSClient {
	return &WSClient{
		masterURL: masterURL,
		name:      name,
		token:     token,
	}
}

// ConnectLoop runs an infinite reconnect loop: connect, flush buffer, read
// (blocks), then retry with exponential backoff on disconnect.
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

		// Connected — reset backoff and flush any buffered messages.
		backoff = time.Second
		log.Info().Str("master", ws.masterURL).Msg("WebSocket connected to master")
		ws.flushBuffer()
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
	// Build ws[s]:// URL from http[s]://
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

// Send tries to write msg to the connection. If the connection is nil or the
// write fails, the message is buffered for later delivery.
func (ws *WSClient) Send(msg *shared.WSMessage) {
	ws.mu.Lock()
	conn := ws.conn
	ws.mu.Unlock()

	if conn != nil {
		data, err := json.Marshal(msg)
		if err != nil {
			log.Error().Err(err).Msg("WebSocket: failed to marshal message")
			return
		}
		if err := conn.WriteMessage(websocket.TextMessage, data); err == nil {
			return
		}
		log.Warn().Err(err).Str("action", msg.ActionID).Msg("WebSocket write failed, buffering")
	}

	ws.bufMu.Lock()
	ws.buffer = append(ws.buffer, msg)
	ws.bufMu.Unlock()
}

func (ws *WSClient) flushBuffer() {
	ws.bufMu.Lock()
	pending := ws.buffer
	ws.buffer = nil
	ws.bufMu.Unlock()

	for _, msg := range pending {
		ws.mu.Lock()
		conn := ws.conn
		ws.mu.Unlock()
		if conn == nil {
			// Re-buffer remaining messages.
			ws.bufMu.Lock()
			ws.buffer = append(pending, ws.buffer...)
			ws.bufMu.Unlock()
			return
		}

		data, err := json.Marshal(msg)
		if err != nil {
			log.Error().Err(err).Msg("WebSocket: failed to marshal buffered message")
			continue
		}
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			log.Warn().Err(err).Msg("WebSocket: flush write failed, re-buffering remaining")
			ws.bufMu.Lock()
			ws.buffer = append(pending, ws.buffer...)
			ws.bufMu.Unlock()
			return
		}
		// Advance past the successfully sent message.
		pending = pending[1:]
	}
}
