// Package ws implements the SaaS-side WebSocket Hub that manages LocalAgent connections.
//
// Design invariant: at most one AgentConn per userID is held in the Hub.
// All writes to a connection are serialised through AgentConn.mu so that
// concurrent callers (Cron Tick via SendToAgent + the handler's own writes)
// never race on the underlying websocket.Conn.
//
// Docs reference: 系統總體拓撲結構.md §5
package ws

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
	"gorm.io/gorm"

	"quantsaas/internal/saas/auth"
	"quantsaas/internal/saas/config"
)

var upgrader = websocket.Upgrader{
	// Allow all origins for LocalAgent — the JWT provides the real auth gate.
	CheckOrigin: func(r *http.Request) bool { return true },
}

// AgentConn wraps a single WebSocket connection with a mutex-serialised write path.
type AgentConn struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	userID uint
}

// writeRaw serialises writes so SendToAgent and the read-loop handler never race.
func (ac *AgentConn) writeRaw(data []byte) error {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	_ = ac.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return ac.conn.WriteMessage(websocket.TextMessage, data)
}

// Hub is the connection registry for all authenticated LocalAgents.
// Use NewHub to construct; wire HandleConnection to GET /ws/agent.
type Hub struct {
	conns  sync.Map // uint(userID) → *AgentConn
	cfg    *config.Config
	db     *gorm.DB
	logger *zap.Logger
}

// NewHub creates a Hub ready to accept connections.
func NewHub(cfg *config.Config, db *gorm.DB, logger *zap.Logger) *Hub {
	return &Hub{cfg: cfg, db: db, logger: logger}
}

// SendToAgent delivers payload bytes to the agent authenticated under userID.
// Returns true when the agent is online and the write succeeded; false otherwise.
// Implements instance.Hub.
func (h *Hub) SendToAgent(userID uint, payload []byte) bool {
	v, ok := h.conns.Load(userID)
	if !ok {
		return false
	}
	ac := v.(*AgentConn)
	if err := ac.writeRaw(payload); err != nil {
		h.logger.Warn("hub: write failed — evicting stale connection",
			zap.Uint("user_id", userID), zap.Error(err))
		h.conns.Delete(userID)
		return false
	}
	return true
}

// HandleConnection is the Gin handler for GET /ws/agent.
//
// Flow:
//  1. HTTP → WebSocket upgrade
//  2. 10 s deadline to receive first message (must be type "auth")
//  3. JWT validation; send auth_result
//  4. Register AgentConn (evict previous connection for same userID)
//  5. Message loop: heartbeat → heartbeat_ack; delta_report → processDeltaReport + report_ack
//  6. Deregister on disconnect
func (h *Hub) HandleConnection(c *gin.Context) {
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		h.logger.Warn("ws: upgrade failed", zap.Error(err))
		return
	}
	defer conn.Close()

	// ── Step 1: wait up to 10 s for the auth message ─────────────────────────
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		h.logger.Warn("ws: auth read error or timeout", zap.Error(err))
		return
	}
	_ = conn.SetReadDeadline(time.Time{}) // clear for the message loop

	// ── Step 2: parse and validate auth ──────────────────────────────────────
	var am authMsg
	if err := json.Unmarshal(raw, &am); err != nil || am.Type != "auth" {
		writeDirect(conn, envelope{Type: "auth_result", Payload: authResultPayload{Success: false}})
		return
	}

	claims, err := auth.ParseToken(h.cfg, am.Token)
	if err != nil {
		h.logger.Warn("ws: JWT invalid", zap.Error(err))
		writeDirect(conn, envelope{Type: "auth_result", Payload: authResultPayload{Success: false}})
		return
	}

	userID := claims.UserID
	ac := &AgentConn{conn: conn, userID: userID}

	// ── Step 3: register; evict any previous connection for this user ─────────
	if prev, loaded := h.conns.Swap(userID, ac); loaded {
		old := prev.(*AgentConn)
		old.mu.Lock()
		_ = old.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "replaced by new connection"))
		old.conn.Close()
		old.mu.Unlock()
		h.logger.Info("ws: previous connection replaced", zap.Uint("user_id", userID))
	}
	defer h.conns.Delete(userID)

	h.logger.Info("agent authenticated and registered", zap.Uint("user_id", userID))

	// ── Step 4: send auth_result success ─────────────────────────────────────
	authOK, _ := json.Marshal(envelope{Type: "auth_result", Payload: authResultPayload{Success: true}})
	if err := ac.writeRaw(authOK); err != nil {
		h.logger.Warn("ws: write auth_result failed", zap.Uint("user_id", userID), zap.Error(err))
		return
	}

	// ── Step 5: message loop ──────────────────────────────────────────────────
	ctx := c.Request.Context()
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			h.logger.Info("agent disconnected", zap.Uint("user_id", userID), zap.Error(err))
			return
		}

		var msg inboundMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			h.logger.Warn("ws: unparseable message", zap.Uint("user_id", userID), zap.Error(err))
			continue
		}

		switch msg.Type {
		case "heartbeat":
			data, _ := json.Marshal(envelope{Type: "heartbeat_ack"})
			if err := ac.writeRaw(data); err != nil {
				h.logger.Warn("ws: heartbeat_ack write failed",
					zap.Uint("user_id", userID), zap.Error(err))
				return
			}

		case "delta_report":
			var payload deltaReportPayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				h.logger.Warn("ws: invalid delta_report payload",
					zap.Uint("user_id", userID), zap.Error(err))
				continue
			}
			if err := processDeltaReport(ctx, h.db, h.logger, userID, payload); err != nil {
				h.logger.Error("ws: processDeltaReport failed",
					zap.Uint("user_id", userID), zap.Error(err))
				// still send report_ack to prevent Agent from hanging
			}
			data, _ := json.Marshal(envelope{Type: "report_ack"})
			if err := ac.writeRaw(data); err != nil {
				h.logger.Warn("ws: report_ack write failed",
					zap.Uint("user_id", userID), zap.Error(err))
				return
			}

		default:
			h.logger.Debug("ws: unhandled message type",
				zap.String("type", msg.Type), zap.Uint("user_id", userID))
		}
	}
}

// writeDirect writes to the raw conn before it is wrapped in AgentConn.
// Safe only before the conn is registered in the Hub map.
func writeDirect(conn *websocket.Conn, v interface{}) {
	_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	data, _ := json.Marshal(v)
	_ = conn.WriteMessage(websocket.TextMessage, data)
}

// ─── Message types ────────────────────────────────────────────────────────────

type authMsg struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

type authResultPayload struct {
	Success bool `json:"success"`
}

// envelope is the generic outbound message wrapper.
type envelope struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload,omitempty"`
}

// inboundMsg is the generic inbound message wrapper.
type inboundMsg struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// deltaReportPayload mirrors agent-side ws.deltaReportPayload.
// Defined here to avoid importing internal/agent packages from SaaS.
type deltaReportPayload struct {
	Execution *executionMsg `json:"execution"` // nil for initial balance snapshot
	Balances  []balanceMsg  `json:"balances"`
}

type executionMsg struct {
	ClientOrderID string  `json:"client_order_id"`
	FilledQty     float64 `json:"filled_qty"`
	FilledPrice   float64 `json:"filled_price"`
	Fee           float64 `json:"fee"`
}

type balanceMsg struct {
	Asset     string  `json:"asset"`
	Available float64 `json:"available"`
	Frozen    float64 `json:"frozen"`
}
