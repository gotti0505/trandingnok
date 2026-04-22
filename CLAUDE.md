# QuantSaaS — AI 工作約束憲法

> 每次對話開始前自動載入。違反任何一條鐵律必須立即停下來。

---

## 唯一功能真源

**所有功能實現的唯一依據是 `docs/` 下的三份文檔：**

- `docs/系統總體拓撲結構.md` — 物理端、邏輯模組、狀態流轉、生命週期
- `docs/策略數學引擎.md` — Step() 契約、Sigmoid 公式、宏觀/微觀引擎規格
- `docs/進化計算引擎.md` — GA 遺傳演算法、染色體定義、回測適應度

**三份文檔沒有明確定義的功能，不進入任何實現。** 如有疑問先澄清文檔，再動代碼。

---

## 工作順序

1. **策略或回測相關** → 先讀 `docs/策略數學引擎.md` 和 `docs/進化計算引擎.md`，確認公式和參數邊界後再動代碼。
2. **Go 後端相關** → 遵守 GORM Code-First；資料庫 schema 真源是 Go struct；只用 `AutoMigrate`，永不手寫 SQL migration 文件。
3. **價格計算相關** → 優先無量綱表達（對數收益率 / 比率）；禁止跨標的比較絕對價格。
4. **架構調整相關** → 保持 SaaS-Strategy-Agent 三端分工；不做預防性解耦；不引入三份文檔未定義的中間層。

---

## 核心約束（五條鐵律）

| # | 鐵律 | 後果 |
|---|------|------|
| 1 | 策略必須滿足複利前置條件（DeadBTC 只進不出，ReserveFloor 永不動用）| 違反 → 停止，重讀宏觀引擎文檔 |
| 2 | 回測與實盤**必須**調用同一個 `Step()` 實現，內部**禁止** `if isBacktest` 分支 | 違反 → 立即回滾 |
| 3 | `Step()` **只在 SaaS 側執行**，Agent 端無策略代碼 | 違反 → 架構性錯誤，停止 |
| 4 | 策略包（`internal/strategies/`）**內部禁止**網路請求、資料庫讀寫、任何文件 I/O | 違反 → 立即移除 |
| 5 | 交易所 API Key / Secret **只能**存在於 `config.agent.yaml`，永不進入 SaaS 代碼或 DB | 違反 → 安全事故 |

---

## 代碼目錄職責

```
cmd/saas/               主入口：SaaS 雲端服務，啟動 HTTP server + Cron + WS Hub
cmd/agent/              主入口：LocalAgent 極簡進程，只做 WS 通信 + 交易所下單

internal/saas/          SaaS 業務邏輯：實例管理、Cron Tick、WS Hub、用戶/訂閱
internal/agent/         Agent 業務邏輯：WS 客戶端、交易所 REST 調用、DeltaReport 上報

internal/strategy/      策略接口定義：Step() 簽名、StrategyInput/Output 結構體、純函數契約
internal/strategies/    具體策略包，每個子目錄一個策略（e.g. sigmoid_dca/）
  └─ [策略名]/          只含 Step() 實現與輔助純函數，無任何 I/O

internal/quant/         量化數學庫：無量綱指標、Sigmoid、EMA、ATR 等純函數
internal/adapters/
  └─ backtest/          回測適配器：模擬 PortfolioState 驅動 Step()，不含策略邏輯
```

---

## 全域技術決策（不可推翻）

1. **策略同構**：回測與實盤必須調用同一個 `Step()` 實現
2. **策略純函數**：`Step()` 內部禁止計時器、網路請求、資料庫讀寫、任何文件 I/O
3. **API Key 物理隔離**：交易所憑證只存在於 `config.agent.yaml`，永不進入 SaaS 側
4. **GORM Code-First**：schema 真源是 Go struct，只用 AutoMigrate，永不寫 SQL migration 文件
5. **無量綱計算**：價格計算用對數收益率或比率，禁止跨標的比較絕對價格
6. **單一 Postgres**：不分庫，Redis 僅做緩存，不作信號傳遞通道

---

## 驗證命令

```bash
# 確認所有包可正常導入
go list ./...

# 跑全量測試（策略包必須 100% 可測試）
go test ./...

# 靜態分析
go vet ./...
```

---

## 版本記錄

| 日期 | 變更 |
|------|------|
| Phase 0 | 初始化，建立基礎約束 |
