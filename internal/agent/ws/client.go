// Package ws implements the LocalAgent WebSocket client.
//
// Lifecycle:
//
//	Run(ctx) → reconnect loop
//	  └─ session(ctx) → login → dial → auth → initial DeltaReport → message loop
//	       ├─ writePump goroutine  (serialises all outbound writes)
//	       ├─ heartbeat goroutine  (sends "heartbeat" every 30 s)
//	       └─ readLoop             (blocks; returns on close or error)
//
// Iron rules enforced here:
//   - Zero strategy code — the agent is a pure execution adapter
//   - API credentials are read once from config at startup and never forwarded
package ws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"quantsaas/internal/agent/config"
	"quantsaas/internal/agent/exchange"
)

// AgentClient manages the SaaS WebSocket connection and order execution.
type AgentClient struct {
	cfg    *config.AgentConfig
	exch   exchange.Client
	logger *zap.Logger
}

// NewAgentClient constructs an AgentClient.
func NewAgentClient(cfg *config.AgentConfig, exch exchange.Client, logger *zap.Logger) *AgentClient {
	return &AgentClient{cfg: cfg, exch: exch, logger: logger}
}

// Run enters the reconnect loop and blocks until ctx is cancelled.
// Backoff: starts at 1 s, doubles each attempt, caps at 5 min.
func (c *AgentClient) Run(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 5 * time.Minute

	for {
		if ctx.Err() != nil {
			return
		}
		if err := c.session(ctx); err != nil && ctx.Err() == nil {
			c.logger.Error("session ended — reconnecting",
				zap.Error(err), zap.Duration("backoff", backoff))
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// session performs one full connection lifecycle.
// It returns only when the connection closes or ctx is cancelled.
func (c *AgentClient) session(ctx context.Context) error {
	// A child context lets us cancel all session goroutines when this session ends.
	sessCtx, cancelSess := context.WithCancel(ctx)
	defer cancelSess()

	// ── Step 1: obtain JWT ────────────────────────────────────────────────────
	token, err := c.login(sessCtx)
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}

	// ── Step 2: dial WebSocket ────────────────────────────────────────────────
	wsURL := toWSURL(c.cfg.SaaSURL) + "/ws/agent"
	conn, _, err := websocket.DefaultDialer.DialContext(sessCtx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("dial %s: %w", wsURL, err)
	}
	defer conn.Close()
	c.logger.Info("agent connected", zap.String("url", wsURL))

	// sendCh is the single write path; all goroutines enqueue here.
	sendCh := make(chan []byte, 64)

	// writePump: the only goroutine that calls conn.WriteMessage.
	go func() {
		for {
			select {
			case data, ok := <-sendCh:
				if !ok {
					return
				}
				conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
					c.logger.Warn("ws write error", zap.Error(err))
					cancelSess()
					return
				}
			case <-sessCtx.Done():
				conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
				_ = conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "shutdown"))
				return
			}
		}
	}()

	// ── Step 3: send auth ─────────────────────────────────────────────────────
	safeSend(sessCtx, sendCh, mustMarshal(authMsg{Type: "auth", Token: token}))

	// ── Step 4: wait for auth_result (15 s deadline) ─────────────────────────
	conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("read auth_result: %w", err)
	}
	var ar authResultMsg
	if err := json.Unmarshal(raw, &ar); err != nil || ar.Type != "auth_result" {
		return fmt.Errorf("unexpected message waiting for auth_result")
	}
	if !ar.Success {
		return fmt.Errorf("SaaS rejected auth")
	}
	conn.SetReadDeadline(time.Time{}) // remove deadline for the message loop
	c.logger.Info("agent authenticated")

	// ── Step 5: send initial DeltaReport (balance snapshot, no execution) ────
	go func() {
		balances, err := c.exch.GetBalances(sessCtx)
		if err != nil {
			c.logger.Warn("initial balance fetch failed", zap.Error(err))
		}
		safeSend(sessCtx, sendCh, mustMarshal(buildDeltaReport(nil, balances)))
	}()

	// ── heartbeat goroutine ───────────────────────────────────────────────────
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				safeSend(sessCtx, sendCh, mustMarshal(basicMsg{Type: "heartbeat"}))
			case <-sessCtx.Done():
				return
			}
		}
	}()

	// ── Step 6: message loop ──────────────────────────────────────────────────
	return c.readLoop(sessCtx, conn, sendCh)
}

// readLoop processes inbound WebSocket messages until the connection closes.
func (c *AgentClient) readLoop(ctx context.Context, conn *websocket.Conn, sendCh chan<- []byte) error {
	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return nil // clean shutdown via context
			}
			return fmt.Errorf("read: %w", err)
		}

		var msg inboundMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			c.logger.Warn("ws: unparseable message", zap.Error(err))
			continue
		}

		switch msg.Type {
		case "command":
			var cmd exchange.TradeCommand
			if err := json.Unmarshal(msg.Payload, &cmd); err != nil {
				c.logger.Warn("ws: invalid command payload", zap.Error(err))
				continue
			}
			// Immediately ACK — do NOT wait for order execution to complete.
			safeSend(ctx, sendCh, mustMarshal(commandAckMsg{
				Type:          "command_ack",
				ClientOrderID: cmd.ClientOrderID,
			}))
			// Execute order asynchronously; report when done.
			go c.executeAndReport(ctx, cmd, sendCh)

		case "heartbeat_ack":
			// nothing required

		case "report_ack":
			// nothing required

		default:
			c.logger.Debug("ws: unhandled message", zap.String("type", msg.Type))
		}
	}
}

// executeAndReport places the order, then sends a DeltaReport with fill details
// and current balances. Runs in a goroutine; uses ctx so it stops on session end.
func (c *AgentClient) executeAndReport(ctx context.Context, cmd exchange.TradeCommand, sendCh chan<- []byte) {
	exec, err := c.exch.PlaceOrder(ctx, cmd)
	var execPtr *exchange.Execution
	if err != nil {
		c.logger.Error("place order failed",
			zap.String("client_order_id", cmd.ClientOrderID),
			zap.Error(err))
		// execPtr stays nil; DeltaReport still carries current balances
	} else {
		execPtr = &exec
	}

	balances, err := c.exch.GetBalances(ctx)
	if err != nil {
		c.logger.Warn("get balances after order failed", zap.Error(err))
	}

	safeSend(ctx, sendCh, mustMarshal(buildDeltaReport(execPtr, balances)))
}

// ─── HTTP helpers ─────────────────────────────────────────────────────────────

// login POSTs credentials to the SaaS REST endpoint and returns the JWT.
func (c *AgentClient) login(ctx context.Context) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"email":    c.cfg.Email,
		"password": c.cfg.Password,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.SaaSURL+"/api/v1/auth/login",
		bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("login %d: %s", resp.StatusCode, string(b))
	}
	var result struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	if result.Token == "" {
		return "", fmt.Errorf("empty token")
	}
	return result.Token, nil
}

// ─── Message types ────────────────────────────────────────────────────────────

type authMsg struct {
	Type  string `json:"type"`
	Token string `json:"token"`
}

type authResultMsg struct {
	Type    string `json:"type"`
	Success bool   `json:"success"`
}

type basicMsg struct {
	Type string `json:"type"`
}

type commandAckMsg struct {
	Type          string `json:"type"`
	ClientOrderID string `json:"client_order_id"`
}

type inboundMsg struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type deltaReportMsg struct {
	Type    string             `json:"type"`
	Payload deltaReportPayload `json:"payload"`
}

type deltaReportPayload struct {
	Execution *exchange.Execution `json:"execution"` // nil for initial snapshot
	Balances  []exchange.Balance  `json:"balances"`
}

func buildDeltaReport(exec *exchange.Execution, balances []exchange.Balance) deltaReportMsg {
	return deltaReportMsg{
		Type: "delta_report",
		Payload: deltaReportPayload{
			Execution: exec,
			Balances:  balances,
		},
	}
}

// ─── Utilities ────────────────────────────────────────────────────────────────

// safeSend enqueues data on sendCh without blocking.
// If ctx is cancelled (session ended) the send is dropped — no panic.
func safeSend(ctx context.Context, sendCh chan<- []byte, data []byte) {
	select {
	case sendCh <- data:
	case <-ctx.Done():
	}
}

func mustMarshal(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

// toWSURL converts an HTTP(S) URL to its WebSocket equivalent.
func toWSURL(u string) string {
	u = strings.TrimRight(u, "/")
	u = strings.Replace(u, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	return u
}
