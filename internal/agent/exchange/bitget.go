// Package exchange encapsulates all direct exchange REST API calls.
// The Agent is the only component that ever holds or uses API credentials.
// Docs reference: 系統總體拓撲結構.md §1.2 (LocalAgent 禁區)
package exchange

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"quantsaas/internal/agent/config"
)

// ─── Wire types shared between exchange and ws packages ──────────────────────

// TradeCommand mirrors instance.TradeCommand; defined here so agent/ never
// imports internal/saas/.
type TradeCommand struct {
	ClientOrderID string `json:"client_order_id"`
	Action        string `json:"action"`                // BUY | SELL
	Engine        string `json:"engine"`                // MACRO | MICRO
	Symbol        string `json:"symbol"`
	AmountUSDT    string `json:"amount_usdt,omitempty"` // BUY only
	QtyAsset      string `json:"qty_asset,omitempty"`   // SELL only
	LotType       string `json:"lot_type"`
}

// Execution holds the fill details of a completed order.
type Execution struct {
	ClientOrderID string  `json:"client_order_id"`
	FilledQty     float64 `json:"filled_qty"`
	FilledPrice   float64 `json:"filled_price"`
	Fee           float64 `json:"fee"`
}

// Balance represents the available and frozen amount of one asset.
type Balance struct {
	Asset     string  `json:"asset"`
	Available float64 `json:"available"`
	Frozen    float64 `json:"frozen"`
}

// Client is the minimal exchange interface the Agent requires.
type Client interface {
	PlaceOrder(ctx context.Context, cmd TradeCommand) (Execution, error)
	GetBalances(ctx context.Context) ([]Balance, error)
}

// ─── Bitget implementation ───────────────────────────────────────────────────

const (
	productionAPI = "https://api.bitget.com"
	sandboxAPI    = "https://testnet-api.bitget.com"
)

// BitgetClient implements Client against Bitget Spot V2 REST API.
type BitgetClient struct {
	cfg     *config.AgentConfig
	baseURL string
	http    *http.Client
}

// NewBitgetClient constructs a BitgetClient.
func NewBitgetClient(cfg *config.AgentConfig) *BitgetClient {
	base := productionAPI
	if cfg.Exchange.Sandbox {
		base = sandboxAPI
	}
	return &BitgetClient{
		cfg:     cfg,
		baseURL: base,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

// PlaceOrder places a spot market order and polls for fill details.
// BUY: size = amount_usdt (quote currency).
// SELL: size = qty_asset (base currency).
func (c *BitgetClient) PlaceOrder(ctx context.Context, cmd TradeCommand) (Execution, error) {
	sym := strings.ReplaceAll(cmd.Symbol, "/", "")

	body := map[string]string{
		"symbol":    sym,
		"orderType": "market",
		"force":     "gtc",
		"clientOid": cmd.ClientOrderID,
	}
	switch cmd.Action {
	case "BUY":
		body["side"] = "buy"
		body["size"] = cmd.AmountUSDT // quote currency for market buy
	case "SELL":
		body["side"] = "sell"
		body["size"] = cmd.QtyAsset // base currency for market sell
	default:
		return Execution{}, fmt.Errorf("PlaceOrder: unknown action %q", cmd.Action)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return Execution{}, err
	}

	var placeResp struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			OrderId string `json:"orderId"`
		} `json:"data"`
	}
	if err := c.post(ctx, "/api/v2/spot/trade/place-order", raw, &placeResp); err != nil {
		return Execution{}, fmt.Errorf("PlaceOrder: %w", err)
	}
	if placeResp.Code != "00000" {
		return Execution{}, fmt.Errorf("PlaceOrder: %s %s", placeResp.Code, placeResp.Msg)
	}

	return c.waitForFill(ctx, cmd.ClientOrderID, placeResp.Data.OrderId)
}

// waitForFill polls order status until fully or partially filled (up to ~3 s).
func (c *BitgetClient) waitForFill(ctx context.Context, clientOid, orderID string) (Execution, error) {
	for i := 0; i < 6; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return Execution{ClientOrderID: clientOid}, ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		ex, filled, err := c.fetchOrderInfo(ctx, orderID)
		if err != nil {
			continue // transient; retry
		}
		ex.ClientOrderID = clientOid
		if filled {
			return ex, nil
		}
	}
	// Return zero fill if order didn't confirm within timeout
	return Execution{ClientOrderID: clientOid}, nil
}

// fetchOrderInfo queries /api/v2/spot/trade/orderInfo for fill details.
func (c *BitgetClient) fetchOrderInfo(ctx context.Context, orderID string) (Execution, bool, error) {
	var resp struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Status     string `json:"status"` // full_fill | partially_fill | live | cancelled
			BaseVolume string `json:"baseVolume"`
			FillPrice  string `json:"fillPrice"`
			FeeDetail  struct {
				TotalDeductedFee string `json:"totalDeductedFee"`
			} `json:"feeDetail"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/api/v2/spot/trade/orderInfo", "orderId="+orderID, &resp); err != nil {
		return Execution{}, false, err
	}
	if resp.Code != "00000" {
		return Execution{}, false, fmt.Errorf("orderInfo: %s", resp.Msg)
	}

	filledQty, _ := strconv.ParseFloat(resp.Data.BaseVolume, 64)
	fillPrice, _ := strconv.ParseFloat(resp.Data.FillPrice, 64)
	fee, _ := strconv.ParseFloat(resp.Data.FeeDetail.TotalDeductedFee, 64)

	filled := resp.Data.Status == "full_fill" || resp.Data.Status == "partially_fill"
	return Execution{
		FilledQty:   filledQty,
		FilledPrice: fillPrice,
		Fee:         fee,
	}, filled, nil
}

// GetBalances returns all non-zero spot account balances.
func (c *BitgetClient) GetBalances(ctx context.Context) ([]Balance, error) {
	var resp struct {
		Code string `json:"code"`
		Msg  string `json:"msg"`
		Data []struct {
			CoinName  string `json:"coinName"`
			Available string `json:"available"`
			Frozen    string `json:"frozen"`
		} `json:"data"`
	}
	if err := c.get(ctx, "/api/v2/spot/account/assets", "", &resp); err != nil {
		return nil, fmt.Errorf("GetBalances: %w", err)
	}
	if resp.Code != "00000" {
		return nil, fmt.Errorf("GetBalances: %s %s", resp.Code, resp.Msg)
	}

	out := make([]Balance, 0, len(resp.Data))
	for _, d := range resp.Data {
		avail, _ := strconv.ParseFloat(d.Available, 64)
		frozen, _ := strconv.ParseFloat(d.Frozen, 64)
		if avail > 0 || frozen > 0 {
			out = append(out, Balance{
				Asset:     d.CoinName,
				Available: avail,
				Frozen:    frozen,
			})
		}
	}
	return out, nil
}

// ─── HTTP helpers ─────────────────────────────────────────────────────────────

func (c *BitgetClient) post(ctx context.Context, path string, body []byte, dst interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	c.addAuth(req, path, "", string(body))
	return c.do(req, dst)
}

func (c *BitgetClient) get(ctx context.Context, path, query string, dst interface{}) error {
	u := c.baseURL + path
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	c.addAuth(req, path, query, "")
	return c.do(req, dst)
}

func (c *BitgetClient) do(req *http.Request, dst interface{}) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

// addAuth injects Bitget v2 HMAC-SHA256 authentication headers.
// prehash = timestamp + METHOD + requestPath(+query) + body
func (c *BitgetClient) addAuth(req *http.Request, path, query, body string) {
	ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
	requestPath := path
	if query != "" {
		requestPath = path + "?" + query
	}
	prehash := ts + strings.ToUpper(req.Method) + requestPath + body

	mac := hmac.New(sha256.New, []byte(c.cfg.Exchange.SecretKey))
	mac.Write([]byte(prehash))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("ACCESS-KEY", c.cfg.Exchange.APIKey)
	req.Header.Set("ACCESS-SIGN", sig)
	req.Header.Set("ACCESS-TIMESTAMP", ts)
	req.Header.Set("ACCESS-PASSPHRASE", c.cfg.Exchange.Passphrase)
	req.Header.Set("locale", "zh-CN")
}
