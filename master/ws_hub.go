package master

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
	"github.com/star/mirrorgo/shared"
)

// WSHub accepts WebSocket connections from workers and routes action status
// messages back to the master via the OnActionStatus callback.
type WSHub struct {
	mu       sync.RWMutex
	conns    map[string]*websocket.Conn // worker name -> conn
	writeMus map[string]*sync.Mutex     // worker name -> write mutex
	token    string

	// OnActionStatus is called for every incoming WSMessage from a worker.
	OnActionStatus func(workerName string, msg *shared.WSMessage)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// NewWSHub creates a new WSHub with the given authentication token.
func NewWSHub(token string) *WSHub {
	return &WSHub{
		conns:    make(map[string]*websocket.Conn),
		writeMus: make(map[string]*sync.Mutex),
		token:    token,
	}
}

// HandleWS handles GET /api/internal/ws?name=xxx&token=xxx — upgrades the
// connection to WebSocket and starts the read loop. Token validation is
// expected to be handled by auth middleware wrapping the HTTP mux.
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

	// If there is an existing connection for the same worker, close it.
	h.mu.Lock()
	if old, ok := h.conns[name]; ok {
		old.Close()
		log.Warn().Str("worker", name).Msg("closing stale websocket connection")
	}
	h.conns[name] = conn
	if _, ok := h.writeMus[name]; !ok {
		h.writeMus[name] = &sync.Mutex{}
	}
	h.mu.Unlock()

	// Read loop — runs until the connection is closed or an error occurs.
	defer func() {
		h.mu.Lock()
		// Only delete if the map still points to this conn (could have been
		// replaced by a new connection for the same worker name).
		if h.conns[name] == conn {
			delete(h.conns, name)
		}
		h.mu.Unlock()
		conn.Close()
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

		var msg shared.WSMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Warn().Err(err).Str("worker", name).Msg("invalid websocket message")
			continue
		}

		if h.OnActionStatus != nil {
			h.OnActionStatus(name, &msg)
		}
	}
}

// SendAck sends an acknowledgement for a given action to the named worker.
func (h *WSHub) SendAck(workerName, actionID string) {
	h.mu.RLock()
	conn, ok := h.conns[workerName]
	wmu := h.writeMus[workerName]
	h.mu.RUnlock()
	if !ok || wmu == nil {
		log.Warn().Str("worker", workerName).Str("action", actionID).Msg("cannot send ack: worker not connected")
		return
	}

	ack := shared.WSAck{
		Type:     "ack",
		ActionID: actionID,
	}
	wmu.Lock()
	err := conn.WriteJSON(ack)
	wmu.Unlock()
	if err != nil {
		log.Error().Err(err).Str("worker", workerName).Str("action", actionID).Msg("failed to send ack")
	}
}

// IsConnected reports whether the named worker has an active WebSocket connection.
func (h *WSHub) IsConnected(workerName string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.conns[workerName]
	return ok
}
