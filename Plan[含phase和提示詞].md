# QuantSaaS 從零複刻完整構建 Plan

> **閱讀說明**
> 這是一份完整的系統構建指南。把每個 Phase 的 Prompt 直接粘貼進 Cursor（或你的 AI 編輯器），按循序執行即可。

---

## 一、系統定位與核心架構

### 系統是什麼

一款面向量化交易投資者的**全天候智慧量化管理工具**：通過華爾街級風控狀態機、遺傳演算法參數尋優，將複雜的動態策略降維成普通人能一鍵託管的 SaaS 財富水庫。

### 三端物理部署

- **`saas`（雲端）**：決策大腦，執行 `Step()`，下發交易指令，不持有任何 API Key
- **`agent`（用戶本地）**：執行手，只負責調用交易所下單並上報結果，不含任何策略代碼
- **`lab`（本地算力機）**：實驗室，專跑 GA 進化與回測，連同一個 Postgres 實例，不下發真實交易

### 全域技術決策（不可推翻的鐵律）

**在開始前，把這六條鐵律貼在顯眼的地方。違反任何一條都要立即停下來。**

1. **策略同構**：回測與實盤必須調用同一個 `Step()` 實現，內部禁止出現 `if isBacktest` 分支
2. **策略純函數**：`Step()` 內部禁止計時器、網路請求、資料庫讀寫、任何檔 I/O
3. **API Key 物理隔離**：交易所憑證只存在於 `config.agent.yaml`，永不進入 SaaS 側
4. **GORM Code-First**：資料庫 schema 真源是 Go struct，只用 AutoMigrate，永不寫 SQL migration 文件
5. **無量綱計算**：價格相關計算使用對數收益率或比率，禁止跨標的比較絕對價格
6. **單一 Postgres**：不分庫，Redis 僅做緩存，不作信號傳遞通道

---

## 二、Sigmoid 動態天平 — 微觀引擎設計哲學

> 這是系統的核心創新，其餘策略機制需自行設計填充。

### 本質：倉位元彈簧系統

Sigmoid 動態天平用 Sigmoid 函數即時計算**目標持倉權重**，通過買賣浮動倉使實際權重趨近目標權重。它是一個與信號來源無關的通用框架——你可以把任何歸一化的市場信號接入它。

### 核心公式：Sigmoid 目標權重

```
CurrentWeight  = FloatBTC × Price / TotalEquity

Signal         = 【你的市場信號，任意歸一化標量，正值傾向減倉，負值傾向加倉】

EffectiveBeta  = max(0.01,  β × MarketBetaMultiplier)
InventoryBias  = clamp(CurrentWeight, 0, 1) − 0.5
Exponent       = EffectiveBeta × Signal + γ × InventoryBias

TargetWeight   = 1 / (1 + e^Exponent),  clamp(0, 1)
```

### 公式解讀

| 場景 | Signal | Exponent 方向 | TargetWeight | 動作 |
|---|---|---|---|---|
| 你的信號看空 | 正 | 增大 | < 0.5 | 減倉 |
| 你的信號看多 | 負 | 減小 | > 0.5 | 加倉 |
| 倉位 > 0.5，γ > 0 | 任意 | 額外增大 | 進一步壓低 | 均值回歸 |
| 倉位 < 0.5，γ > 0 | 任意 | 額外減小 | 進一步拉高 | 均值回歸 |

- **Signal**：你的策略信號，可以是均值回歸信號、動量信號、突破信號，甚至多信號的加權合成——只要歸一化為一個標量即可接入
- **β（激進係數）**：越大調倉越頻繁，市場狀態感知層可在極端行情時動態放大 β
- **γ（倉位偏置係數）**：`γ=0` 時純信號驅動，`γ>0` 時疊加均值回歸力防止倉位極端漂移
- **MarketBetaMultiplier**：來自上層市場狀態感知，行情極端時放大 β 自動加速回應

### GA 在這裡做什麼

Signal 通常由多個因數線性合成：

```
Signal = a × X1 + b × X2 + c × X3 + ...
```

其中 X1、X2、X3 是你選擇的市場特徵（無量綱化後的標量），a、b、c 是對應的權重係數。這些係數就是**染色體的一部分**，由遺傳演算法在歷史資料上搜索最優值。

舉例：如果你設計了三個無量綱特徵 X1（價格偏離類）、X2（動量類）、X3（加速度/突破類），合成信號就是 `a×X1 + b×X2 + c×X3`，GA 搜索的就是讓這個合成信號喂進 Sigmoid 後，在多時段歷史回測中跑贏被動 DCA 基準的最優 (a, b, c)。

你選什麼特徵完全由你的策略邏輯決定——均值回歸、動量、突破、成交量異動、鏈上資料……GA 的搜索機制是完全通用的，它不關心 X 是什麼，只負責找到讓系統在歷史上表現最好的係數組合。連同 β、γ 等 Sigmoid 本身的參數一起，整個微觀引擎的參數空間就是一個高維搜索問題，GA 是求解它的工具。所以為了防止過擬合，你需要在回測中自行加入亂序回測或者蒙特卡洛演算法等內容。因此類內容對性能的影響過於大，本次plan沒有寫入。

### 理論訂單與楔形區過濾

```
DeltaWeight    = TargetWeight − CurrentWeight
TheoreticalUSD = DeltaWeight × TotalEquity
```

粉塵攔截規則（防止無效小額交易）：
- `|TheoreticalUSD| ≥ 最小閾值`：直接下單
- `|TheoreticalUSD| ∈ (0, 最小閾值)`：僅在非安靜態 **且** 滿足楔形突破條件時強制最小訂單，否則歸零
- 楔形突破條件：`|DeltaWeight| ≥ 閾值` 或 `VolatilityRatio ≥ 閾值`
- `VolatilityRatio = clip(MAV短期 / MAV長期, 0.1, 3.0)`，MAV 為平均絕對漲跌（非 ATR）

---
以下是具體的vibe coding分步驟指令
## Phase 0 — 環境初始化與 AI 協作基礎設施

### 目標
在專案根目錄建立 AI 工作約束檔（CLAUDE.md/AGENTS/.cursoruls），讓 Cursor 在每次對話中自動載入專案規範。初始化 Go 項目依賴。

### Context
這一步決定了後續所有 AI 對話的品質。CLAUDE.md 是 AI 的"憲法"，每次對話開始時自動讀入。

### Prompt
```
幫我完成以下兩件事：

第一，在專案根目錄創建各種 約束檔 檔，內容包括以下幾部分：

"唯一功能真源"部分：聲明當前功能只依據 docs/ 下的三份文檔（系統總體拓撲結構、策略數學引擎、進化計算引擎），三份文檔沒有定義的功能不進入實現。

"工作順序"部分：列出四條規則——涉及策略和回測先讀對應文檔；涉及 Go 後端遵守 GORM Code-First 只用 AutoMigrate；涉及價格計算優先無量綱表達；涉及架構邊界保持 SaaS-Strategy-Agent 分工不做預防性解耦。

"核心約束"部分：列出五條鐵律——策略必須滿足複利前置條件；回測與實盤調用同一 Step() 實現；Step() 只在 SaaS 側執行；策略包內部禁止網路資料庫檔 I/O；API Key 只能在 config.agent.yaml。

"代碼目錄"部分：列出 cmd/saas/ cmd/agent/ internal/saas/ internal/agent/ internal/strategy/ internal/strategies/[策略名]/ internal/quant/ internal/adapters/backtest/ 的各自職責說明。

"驗證命令"部分：go list ./... 和 go test ./...

第二，初始化 Go 項目，並安裝以下依賴：gin、gorm + postgres driver、go-redis、golang-jwt、robfig/cron、zap、gorilla/websocket、testify。
第三，為整個專案構建一些基礎的SKILLS，至少包含系統架構師、量化交易數學專家、go後端專家、部署與運維專家
```

### 預期產出
- `CLAUDE.md`
- `go.mod` + `go.sum`
- 基礎目錄骨架
- AGENT SKILL

---

## Phase 1 — 三份真來源文件（需要自己填寫內容）

### 目標
在寫任何代碼之前，用文檔把系統的設計意圖固化。這三份文檔是整個系統的"法律"，是後續所有代碼的唯一依據。其中進化計算引擎已經有原始參考檔，直接參考即可。

> **重要說明**：下面給出的是三份文檔的**結構骨架**，每個標題下的內容需要你自己填寫。這是你的策略設計空間，沒有標準答案。可以用自然語言與AI對話，描述你要的需求，讓AI補齊內容。

### 1A. 系統總體拓撲結構文檔

### Prompt
```
幫我在 docs/ 目錄下創建"系統總體拓撲結構.md"。這份文檔定義系統有哪些物理端、有哪些邏輯模組、狀態如何在它們之間流轉，以及系統的生命週期動作。不含任何具體策略公式。

文檔結構如下，每個章節標題下用三行以上的文字描述清楚這部分的設計決策：

第 0 章：架構哲學

第 1 章：三端物理部署形態（saas 雲端 / agent 用戶本地 / lab 算力機的各自職責與禁區）

第 2 章：app_role 三態行為矩陣（saas/lab/dev 各自開放和限制哪些能力，用表格表示）

第 3 章：邏輯模組與職責邊界（Strategy 策略模組 / Instance 實例模組 / Evolution 進化模組 / Auth 認證模組，明確每個模組的職責邊界和禁區）

第 4 章：全域狀態匯流排（單一 Postgres + Redis 僅緩存；列一張資料所有權表，每類資料的真源在哪一端）

第 5 章：WebSocket 通信協定（消息類型全表，含 auth/heartbeat/command/command_ack/delta_report/report_ack 的方向與觸發時機；TradeCommand 欄位語義；DeltaReport 欄位語義；狀態收斂與天然自愈機制）

第 6 章：系統級生命週期動作（系統初始化流程 / Cron Tick 驅動 Step() 的完整流程 / Agent 斷線重連指數退避 / 優雅停機與狀態快照）

第 7 章：不可推翻的技術決策（把前述六條鐵律以及你自己認為不可推翻的其他約束完整列出）
```

### 1B. 策略數學引擎文檔

### Prompt
```
幫我在 docs/ 目錄下創建"策略數學引擎.md"。這份文檔是策略邏輯的數學規格書，定義 Step() 函數的完整輸入輸出契約，以及內部各層的計算邏輯。

文檔結構如下：

第 0 章：引擎身份（純函數無狀態轉換器；唯一入口 Step(StrategyInput)→StrategyOutput；鐵律複述）

第 1 章：資產結構三態（Portfolio State）
——DeadBTC（宏觀底倉，只進不出）/ FloatBTC（微觀浮動倉，可買賣）/ ColdSealedBTC（冷封存，永不釋放）
——TotalEquity 公式、ReserveFloor 公式、SpendableUSDT 公式、CurrentMicroWeight 公式（這四個是通用架構，保留）
——micro_reserve_pct 參數語義與預設值（可進化）

第 2 章：市場狀態感知層
【核心設計空間一：如何對市場狀態分類？分幾類？每類用什麼特徵判斷？】
必須定義"各種態（牛/熊/安靜）"的判斷邏輯（安靜態下微觀粉塵訂單歸零）。

第 3 章：信號與目標函數框架（可選的 MPC 概念層描述）

第 4 章：宏觀引擎
【宏觀引擎如何決定何時買、買多少？】
需要你設計具體的DCA策略，宏觀引擎關注的是長期的趨勢，這一點請注意。

第 5 章：微觀引擎
直接按本文檔第二章"Sigmoid 動態天平"的公式和解釋填入。
包含：信號公式、Sigmoid 目標權重公式、理論訂單計算、楔形區過濾規則。

第 6 章：DeadBTC 釋放規則
定義一下再什麼情況下，可以把宏觀引擎買入的btc轉為浮動倉位（即可以賣出）

第 7 章：可進化參數契約（Chromosome）
【你的核心設計空間三：哪些參數交給 GA 去搜索？】
必須包含：欄位清單（名稱/類型/語義/預設值/邊界）、硬邊界約束、結構約束（如 EMA 順序約束、時間週期相對論鎖）
必須分清：哪些是染色體（參與代內交叉變異），哪些是出生點參數（Epoch 級凍結，不進入基因組）

```

### 1C. 進化計算引擎文檔

### Prompt
```
幫我在 docs/ 目錄下創建"進化計算引擎.md"。這份文檔定義 GA 遺傳演算法引擎的完整規格——它是一個純計算黑盒，不關心具體策略的內部結構。

文檔結構如下：

第 0 章：核心定位（純計算執行緒；通過抽象介面驅動種群生命週期；不 import 任何具體策略實現）

第 1 章：探索視窗定義
1.1 多時段坩堝（四個評估窗口：全量歷史/5年/2年/6個月，適應度權重分別為 0.40/0.30/0.20/0.10；嚴禁未來資料洩露）
1.2 基因空間的三個語義操作：Sample（隨機採樣）/ Clamp（修復越界）/ Validate（驗證合法）
1.3 出生點凍結（Epoch 級，全種群共用，不參與代內交叉變異）

第 2 章：進化生命週期動作
種群初始化策略（精英繼承 10% + 強化變異 40% + 完全隨機 50%；index 0 始終為當前種子冠軍原樣）
併發適應度評估（固定大小 worker pool，上限 = min(NumCPU, PopulationSize)）
錦標賽選擇（TournamentSize=3，隨機抽取取最優）
均勻交叉（Uniform Crossover，每維度 50% 概率獨立從兩父代選取）
加性高斯變異（每維度獨立 Bernoulli 概率決定是否變異，變異量為正態分佈）
精英保留（Top N 個個體直接進入下一代）
收斂檢測 + 變異斜坡（Mutation Ramp）：連續 N 代無改善則放大變異概率和幅度，上限觸及且仍無改善才 Early Stop
基因組指紋緩存（FNV-1a-64 雜湊，精度 1e-6，命中緩存則跳過重複回測評估）

第 3 章：適應度黑盒（Fitness Function）
多視窗坩堝分數公式：Alpha = ROI策略 - ROI被動DCA；SliceScore = Alpha - 1.5 × max(0, MaxDD策略 - MaxDD被動DCA)
Fatal 判斷：MaxDD ≥ 88% 時 SliceScore = -99999（硬否決，立即返回）
加權匯總：ScoreTotal = 0.40×全量 + 0.30×5年 + 0.20×2年 + 0.10×6個月
Ghost DCA 基準定義（種子資本買入 + 每自然月月初注資全買，作為被動對照組）
Modified Dietz 收益率（剔除注資跳變對 NAV 的影響）
級聯短路（按 6m→2y→5y→全量 順序評估，fatal 立即退出跳過後續長窗口）

第 4 章：EvolvableStrategy 介面（8-verb 契約）
StrategyID / Sample / Mutate / Crossover / Fingerprint / Evaluate / DecodeElite / EncodeResult
說明每個動詞的職責，強調引擎對染色體內部欄位完全不可見

第 5 章：EvaluablePlan 唯讀上下文
Epoch 啟動時構建，整個世代內不可變，包含：標的資訊 / 出生點快照 / 四個坩堝視窗 / 預計算的 DCA 基線

第 6 章：結果交付與基因角色三態
challenger（進化產出，等待人工審批）→ champion（當前活躍冠軍）→ retired（歷史存檔）
人工 Promote 流程（DB 事務：舊 champion→retired，challenger→champion；Redis 緩存立即失效）

第 7 章：HTTP 觸發契約
POST /api/v1/evolution/tasks 的參數列表（pop_size, max_generations, spawn_mode）
GET /api/v1/evolution/tasks 的返回結構
```

---

## Phase 2 — 基礎設施層（Config + DB + Auth）

### 目標
搭建系統的物理基礎：配置載入、GORM 資料庫模型定義（全量 AutoMigrate）、Redis 用戶端、JWT 工具。

### Context
GORM Code-First 的核心意義：所有資料庫結構變更都通過修改 Go struct 來完成，AutoMigrate 自動同步，完全不存在手寫 SQL 檔的概念。這是整個專案的 schema 管理哲學。

### Prompt
```
請閱讀 docs/系統總體拓撲結構.md，然後為我實現 Go 項目的基礎設施層。

總體約束：模組使用 gin 框架，ORM 為 GORM + postgres driver，日誌使用 zap，所有資料庫模型只使用 GORM struct tag 定義，不寫任何 SQL 檔。

請實現以下內容：

一、internal/saas/config/config.go
定義 Config 結構體（包含 AppRole / Database / Redis / JWT / Server 五個子配置），實現從 config.yaml 檔載入的函數。AppRole 取值為 "saas" / "lab" / "dev"。同時創建一份 config.yaml 範本，不含任何金鑰，金鑰欄位留空並注明需通過環境變數注入。

二、internal/saas/store/models.go
用 GORM struct 定義所有核心資料模型：
- User（用戶與訂閱計畫）
- StrategyTemplate（策略範本註冊表，包含 ID/Name/Version/IsSpot 欄位，以及 Manifest JSON blob）
- StrategyInstance（策略實例，包含狀態欄位 RUNNING/STOPPED/ERROR，以及關聯的 Template 和 User）
- PortfolioState（實例帳戶快照，包含 USDTBalance/DeadBTC/FloatBTC/ColdSealedBTC/TotalEquity/LastProcessedBarTime）
- RuntimeState（策略運行時狀態，JSON blob 欄位，由 Step() 產出後持久化）
- SpotLot（倉位元 lot 記錄，包含 LotType 欄位取值 DEAD_STACK/FLOATING/COLD_SEALED，Amount/CostPrice/CreatedAt/IsColdSealed）
- TradeRecord（成交記錄，包含 ClientOrderID/Action/Engine/Symbol/FilledQty/FilledPrice/Fee）
- SpotExecution（原始成交明細，pending→filled/failed 狀態機）
- AuditLog（審計日誌，EventType + Payload JSON blob）
- GeneRecord（基因庫，包含 StrategyID/Role challenger-champion-retired/ParamPack JSON/ScoreTotal/MaxDrawdown）
- EvolutionTask（進化任務，包含 Status/Progress/Config JSON）
- KLine（歷史 K 線，包含 Symbol/Interval/OpenTime/OHLCV 欄位，在 Symbol+Interval+OpenTime 上建唯一索引）

三、internal/saas/store/db.go
實現 NewDB(cfg) 函數：建立 Postgres 連接，對以上所有模型執行 AutoMigrate，返回封裝好的 DB 物件。

四、internal/saas/store/redis.go
實現 Redis 連接封裝，提供 Get/Set/Del 三個基礎方法。用途說明：冠軍基因緩存（key: champion:{strategyID}）、會話緩存，不用於信號傳遞。

五、internal/saas/auth/service.go
實現 SignToken(userID uint, role string) 和 ParseToken(tokenStr string) 兩個函數，使用 golang-jwt 庫。
```

---

## Phase 3 — 量化數學基礎層（internal/quant）

### 目標
實現所有策略共用的數學工具庫：基礎統計函數、資產三態管理、Ghost DCA 基準、以及微觀引擎的 Sigmoid 動態天平。

### 3A. 基礎數學工具

#### Prompt
```
請實現 internal/quant/ 目錄下的數學基礎工具，這些是所有策略共用的底層函數。

鐵律：所有價格相關計算必須無量綱化，禁止在函數內部跨標的比較絕對價格。

math.go：實現以下無狀態純函數
- EMA：對任意 float64 序列計算指數平滑均線，輸入序列 + 週期，返回最新一個值
- StdDev：對任意 float64 序列計算樣本標準差，輸入序列 + 週期，返回最新視窗的標準差
- MAVAbsChange：最近 L 根收盤的平均絕對漲跌（不是 ATR，不依賴 High/Low），公式為最近 L 根收盤兩兩絕對差值之和除以 L-1
- ClipFloat64：將 float64 值夾緊到 [lo, hi] 區間
- RoundToUSDT：將 float64 四捨五入到兩位小數

data.go：定義通用資料結構
- Bar：一根 K 線，包含 OpenTime/Open/High/Low/Close/Volume
- StrategyInput：策略輸入快照，具體欄位參考 docs/策略數學引擎.md 第 8 章
- StrategyOutput：策略輸出意圖集，具體欄位參考 docs/策略數學引擎.md 第 8 章
- PortfolioSnapshot：帳戶快照，包含 USDTBalance/DeadBTC/FloatBTC/ColdSealedBTC

closes.go：實現 ACL 降級工具函數
- ExtractCloses：從 []Bar 中提取收盤價序列 []float64
- ExtractTimestamps：從 []Bar 中提取時間戳記序列 []int64
說明：現貨策略的 OHLCV 降級必須在這裡完成，策略內核代碼禁止直接依賴 Bar 結構體
```

### 3B. 資產倉位管理

#### Prompt
```
請實現 internal/quant/lot.go，提供倉位元 lot 的管理邏輯。

SpotLot 結構體包含：LotType（DEAD_STACK / FLOATING / COLD_SEALED 三態）、Amount、CostPrice、CreatedAt、IsColdSealed 布林標誌。

需要實現以下功能：
- 計算 DeadBTC 總量（IsColdSealed=false 的 DEAD_STACK 的 Amount 之和）
- 計算 FloatBTC 總量（FLOATING 的 Amount 之和）
- 軟釋放（Soft Release）：從 DEAD_STACK lot 中，篩選出老化超過指定月數的非 ColdSealed lot，按最大釋放比例限制，將不超過"可賣出缺口"的數量轉換為 FLOATING 類型，保留原始成本
- 硬釋放（Hard Release）：當微觀引擎產出賣出意圖但 FloatBTC 不足時，從 DEAD_STACK（非 ColdSealed，不限老化時間）中補足差額轉為 FLOATING

鐵律：ColdSealedBTC 標記的 lot 任何情況下不可被釋放。
```

### 3C. Sigmoid 動態天平（微觀引擎）

#### Prompt
```
請實現 internal/quant/micro_engine.go，包含微觀引擎的完整邏輯，核心是 Sigmoid 動態天平。

這是系統最核心的創新點：用 Sigmoid 函數實現動態目標倉位元權重，同時支持均值回歸、趨勢跟蹤、倉位回饋三種機制的疊加。實現時請嚴格遵循以下數學規格，不得改變公式結構：

輸入結構體包含以下欄位：
- 收盤價序列（[]float64）和當前價格
- 當前微觀倉位權重（FloatBTC × Price / TotalEquity）
- 總權益 TotalEquity
- 來自染色體的可進化參數：你認為適合你策略的信號參數、sigma_floor（σ 最小值）、beta（Sigmoid 激進係數）、gamma（倉位偏置係數）
- 來自市場狀態感知層的參數：BetaMultiplier（動態放大係數）、IsQuiet（是否安靜態）

輸出結構體包含：TargetWeight / Signal（調試用）/ TheoreticalUSD / OrderUSD（經過濾後的實際訂單金額）/ VolatilityRatio（調試用）

計算步驟如下，請嚴格按照順序實現：

第一步：計算 EMA（窗長固定為不可進化常量，你在代碼中定為常量 MicroSignalEMABars）和 σ（窗長固定為不可進化常量 MicroSignalStdDevBars）。σ 取 max(實際標準差, sigma_floor)。如果 σ=0 則跳過本次決策。

第二步：計算無量綱信號。可以是你定的任何計算方式。

第三步：計算 Sigmoid 目標權重。EffectiveBeta = max(0.01, beta × BetaMultiplier)。InventoryBias = clamp(CurrentWeight, 0, 1) - 0.5。Exponent = EffectiveBeta × Signal + gamma × InventoryBias。TargetWeight = 1/(1+exp(Exponent))，clamp 到 [0,1]。

第四步：計算理論訂單。DeltaWeight = TargetWeight - CurrentWeight。TheoreticalUSD = DeltaWeight × TotalEquity。

第五步：計算 VolatilityRatio。使用固定常量 MicroVolRatioLongBars 和 MicroVolRatioShortBars 計算兩個時間視窗的 MAVAbsChange，比值 clip 到 [0.1, 3.0]。數據不足時預設為 1.0。

第六步：楔形區過濾。|TheoreticalUSD| >= 最小訂單閾值時直接以原值下單。在 (0, 最小訂單閾值) 範圍內，當非安靜態且滿足楔形突破條件（|DeltaWeight| 超過倉位變動閾值 OR VolatilityRatio 超過波動率閾值）時強制下最小訂單（保持符號），否則 OrderUSD = 0。具體閾值數值在你的文檔或染色體中定義。

請在函數注釋中寫入 Sigmoid 動態天平的設計哲學：Signal 是外力（市場信號），InventoryBias 是彈簧恢復力，Beta 是彈簧剛度，Gamma 決定是否啟用彈簧，VolatilityRatio 楔形過濾控制安靜期粉塵。
```

### 3D. 市場狀態感知層

#### Prompt
```
請實現 internal/quant/market_state.go。

【這是你需要填充自己策略的地方，有很多經典的模型可以使用，比如瑪律科夫之類的】

先閱讀 docs/策略數學引擎.md 第 2 章，瞭解我對市場狀態分類的設計。

MarketState 結構體必須包含以下欄位（這是與宏觀/微觀引擎的介面契約，不可更改）：
- State string（你的狀態枚舉值）
- TimeDilationMultiplier float64（給宏觀引擎，1.0 為正常，>1.0 擴展時間窗口）
- BetaMultiplier float64（給微觀 Sigmoid，1.0 為正常，>1.0 加速調倉）
- IsQuiet bool（true 時微觀粉塵訂單歸零）

ComputeMarketState 函數的輸入和內部分類邏輯，請完全按照 docs/策略數學引擎.md 第 2 章的規格實現。
```

### 3E. 宏觀引擎

#### Prompt
```
請實現 internal/quant/macro_engine.go。

【這是你需要填充自己策略的地方。宏觀引擎負責長期建倉節奏，典型設計包括但不限於：固定週期定投、基於市場狀態加速/減速的動態定投、價格偏離均值觸發的加倉等。關鍵約束是只買不賣，且要有死線兜底機制防止資金長期閒置。】

先閱讀 docs/策略數學引擎.md 第 4 章，瞭解你自己對宏觀引擎的設計。

MacroDecisionInput 結構體需要包含的欄位，以及 ComputeMacroDecision 函數的完整邏輯，請完全按照 docs/策略數學引擎.md 第 4 章的規格實現。

記住宏觀引擎鐵律：只產出 BUY 意圖，絕對不產出 SELL 意圖；訂單金額需與 SpendableUSDT 做 clamp；小於最小訂單閾值 10.1 USDT 的訂單不執行。
```

### 3F. Chromosome 可進化參數結構體

#### Prompt
```
請實現 internal/quant/genome.go，定義可進化參數的完整結構體、邊界約束和輔助函數。

先閱讀 docs/策略數學引擎.md 第 7 章，瞭解我的染色體設計。

需要實現：
一、Chromosome 結構體（欄位、類型、json tag 完全按照文檔第 7 章定義）

二、HardBounds 常量，定義每個欄位的合法數值範圍 [min, max]

三、ClampChromosome 函數：將所有欄位 clamp 到合法範圍，同時修復結構約束（EMA 順序約束、相對論鎖等），文檔第 7 章有定義。變異後必須調用此函數。

四、DefaultSeedChromosome 變數：產品預設冠軍種子值，作為 GA 冷開機時的初始個體和 JSON 解碼失敗時的回退值。具體值按文檔定義填寫。

五、SpawnPoint 結構體（出生點）：包含 Policy（資金政策，含月度注資/死線比例/釋放閾值等）和 Risk（風險邊界，含手續費率/全域止損等）。這部分參數不參與代內交叉變異，不進入基因組指紋。
```

### 3G. Ghost DCA 基準

#### Prompt
```
請實現 internal/quant/ghost_dca.go，提供被動 DCA 基準模擬器，供 GA 適應度評估時作為對照組使用。

GhostDCAConfig 包含初始資本和每月注資金額兩個欄位。

SimulateGhostDCA 函數邏輯：以第一根 bar 的收盤價買入全部初始資本的 BTC；之後每個自然月月初，將 MonthlyInject USDT 全部用於買入 BTC；全程記錄 NAV 曲線用於計算最大回撤。

返回 GhostDCAResult，包含 FinalEquity / TotalInjected / MaxDrawdown / ROI 四個欄位。

ROI 使用 Modified Dietz 方法計算：剔除注資跳變對 NAV 的影響。公式為：(期末權益 - 期初權益 - 現金流之和) / (期初權益 + Σ(現金流_i × 加權因數_i))，其中加權因數 = (總天數 - 注資發生日) / 總天數。

同時在此檔中實現 MaxDrawdown 計算函數：基於 NAV 曲線，計算峰值到穀底的最大相對回撤。
```

---

## Phase 4 — 策略模組（Step() 主函數）

### 目標
實現策略的主函數 `Step()`，這是整個系統的決策核心，回測和實盤共用同一個實現。

### Context
策略模組是一個純函數包。整個包的唯一對外入口是 `Step()`。包內部不能有任何 I/O。所有 OHLCV 降級（把 Bar 序列變成 []float64）必須在調用 Step() 之前完成，策略內核只消費 []float64。

### Prompt
```
請閱讀 docs/策略數學引擎.md，然後實現 internal/strategies/[策略名]/ 目錄下的策略模組。

整個模組的檔結構建議如下：
- manifest.go：策略中繼資料（ID/Name/Version/IsSpot/Description）
- params.go：策略參數解析，包含從 ParamPack JSON 解析出 Chromosome + SpawnPoint 的函數
- state.go：RuntimeState 結構體，定義需要跨 tick 持久化的策略運行時狀態欄位（按文檔中你的設計填寫）
- macro.go：調用 quant.ComputeMacroDecision 的薄包裝，注入文檔規定的上下文參數
- micro.go：調用 quant.ComputeMicroDecisionV4 的薄包裝
- dead_release.go：實現 DeadBTC 軟釋放和硬釋放的決策邏輯（調用 lot.go 的工具函數）
- step.go：主函數 Step(input quant.StrategyInput, params Params) quant.StrategyOutput，按以下順序組裝：
  1. 資料視窗充足性檢查（不足則返回空輸出）
  2. 從 input.Closes 和 Portfolio 快照計算 TotalEquity / SpendableUSDT / CurrentMicroWeight
  3. 調用 ComputeMarketState 得到市場狀態超參
  4. 調用宏觀引擎得到宏觀訂單意圖
  5. 調用微觀引擎（Sigmoid 動態天平）得到微觀訂單意圖
  6. 調用釋放規則得到底倉釋放意圖
  7. 更新 RuntimeState（更新 LastProcessedBar 及你在文檔中定義的其他持久化狀態）
  8. 組裝並返回 StrategyOutput

鐵律檢查清單（實現完成後逐一確認）：
- Step() 函數體內沒有任何 http / sql / os / time.Now() 調用
- Step() 函數體內沒有 if isBacktest 或類似分支
- 策略包的任何檔都沒有 import 網路/資料庫相關包
- 只使用 input.Closes []float64，沒有直接使用 quant.Bar
```

---

## Phase 5 — 遺傳演算法進化引擎（GA Engine）

> **這是系統的演算法核心，詳細程度高於其他模組。**

### 架構關係

```
EvolutionEngine（調度器，不知道染色體欄位名）
    ↓ 通過 EvolvableStrategy 8-verb 介面
[YourStrategy]Evolvable（策略側適配器）
    ↓ 調用
RunBacktest（回測適配器）
    ↓ 調用
Step()（策略純函數，與實盤相同）
```

### 5A. EvolvableStrategy 介面 + 策略側實現

#### Prompt
```
請閱讀 docs/進化計算引擎.md，然後實現 internal/saas/ga/evolvable.go 和 internal/saas/ga/[策略名]_evolvable.go。

evolvable.go 定義以下內容：
- Gene = any（不透明載體類型別名，引擎通過介面操作，不讀取內部欄位）
- DCABaseline 結構體（FinalEquity / TotalInjected / MaxDrawdown 三個欄位，Epoch 啟動時預計算 Ghost DCA 結果）
- EvaluablePlan 結構體（Pair/TemplateName/Spawn/LotStep/LotMin/Windows/DCABaselines/AggregateCache，Epoch 啟動時構建，整個世代內唯讀）
- EvolvableStrategy 介面，包含 8 個方法：StrategyID / Sample / Mutate / Crossover / Fingerprint / Evaluate / DecodeElite / EncodeResult

[策略名]_evolvable.go 為具體策略實現該介面，方法說明：

Sample：從染色體的合法邊界內均勻隨機採樣一個 Chromosome，調用 ClampChromosome 修復結構約束後返回

Mutate：對每個染色體欄位以獨立的 Bernoulli 概率 prob 決定是否變異，變異量為 NormFloat64() × 該欄位的步長 × scale，變異後調用 ClampChromosome

Crossover：對兩個父代 Chromosome 的每個欄位以 0.5 概率獨立選擇來源（均勻交叉），組裝後調用 ClampChromosome 修復結構約束

Fingerprint：對所有染色體欄位進行精度為 1e-6 的量化後，用 FNV-1a-64 雜湊生成唯一指紋，相同參數（精度 1e-6 內）的染色體應產生相同雜湊

Evaluate：執行多窗口坩堝評估。按 plan.Windows 昇冪（短→長）依次調用 RunBacktest，計算每個視窗的 Alpha 和 SliceScore，MaxDD >= 88% 時立即返回 fatal=-99999（級聯短路），否則按 plan.Windows[i].Weight 加權匯總返回 ScoreTotal

DecodeElite：從 ParamPack JSON 中解碼出 Chromosome，如果 raw 為空或解碼失敗則返回 DefaultSeedChromosome

EncodeResult：將冠軍 Chromosome 和 SpawnPoint 序列化為 ParamPack JSON blob 存入資料庫

注意包位置約束：此檔放在 internal/saas/ga/ 包下，而非策略包內，原因是避免策略包→ga包→策略包的導入迴圈。
```

### 5B. GA 主引擎

#### Prompt
```
請閱讀 docs/進化計算引擎.md，然後實現 internal/saas/ga/engine.go。

EvolutionEngine 結構體欄位包含：
- evolvable EvolvableStrategy（策略適配器介面，引擎唯一的策略通信管道）
- 對 genomeStore 和 db 的依賴
- 以下可配置超參（括弧內為預設值）：PopSize(300) / MaxGenerations(25) / EliteCount(8) / MutationProbability(0.15) / MutationScale(1.0) / MutationProbabilityMax(0.55) / MutationScaleMax(3.0) / MutationRampFactor(1.25) / EarlyStopPatience(5) / EarlyStopMinDelta(0.001) / TournamentSize(3)

EpochConfig 結構體包含：PopSize / MaxGenerations / LotStepSize / LotMinQty / OnProgress 進度回呼函數 / SpawnPointOverride（非 nil 時覆蓋冠軍或默認的出生點）

RunEpoch 函數完整邏輯（嚴格按文檔 docs/進化計算引擎.md 第 2 章實現）：

步驟一：構建 EvaluablePlan。從資料庫拉取歷史 K 線，調用 BuildCrucibleWindows 構建四個坩堝窗口；對每個窗口調用 SimulateGhostDCA 預計算 DCA 基線；封裝為 EvaluablePlan（Epoch 內不可變）。

步驟二：種群初始化。從資料庫載入精英基因清單（通過 DecodeElite 解碼）。index 0 始終為當前種子冠軍原樣。其餘個體按比例分配：約 10% 為精英原樣、約 40% 為精英加強化變異（固定 prob=0.15 scale=1.5）、約 50% 為完全隨機（通過 Sample 生成）。無精英時 index 0 為預設種子，其餘全部隨機。

步驟三：併發評估初始種群（見 evaluatePopulation 函數說明）。

步驟四：主進化迴圈（遍歷 MaxGenerations 代）：
- 按適應度降冪排序種群
- 收斂檢測：當代最優 - 歷史最優 < EarlyStopMinDelta，則 patienceCount++；否則更新歷史最優，patienceCount=0
- 觸發變異斜坡：patienceCount >= EarlyStopPatience 時，mutProb *= MutationRampFactor（上限 MutationProbabilityMax），mutScale *= MutationRampFactor（上限 MutationScaleMax）
- Early Stop：mutProb 和 mutScale 均已觸及上限且仍無改善時退出迴圈
- 調用進度回檔 OnProgress（當前代、最佳適應度、當前變異參數）
- 產生下一代：精英保留（Top EliteCount 直接入下一代），其餘通過 tournamentSelect + Crossover + Mutate 產生

步驟五：取最優個體，調用 EncodeResult 序列化，寫入資料庫（Role = "challenger"），返回 EpochResult。

evaluatePopulation 函數：併發評估整個種群，帶指紋緩存去重。
- Workers = min(runtime.NumCPU(), len(population))
- 用帶緩衝 channel 作為任務佇列，每個 worker 從佇列取任務
- 對每個基因先計算 Fingerprint，命中緩存則直接複用，否則調用 evolvable.Evaluate
- 用 sync.Map 存儲 fingerprint→fitness 的緩存
- 所有 worker 完成後返回 []float64 分數陣列

tournamentSelect 函數：從種群中隨機抽取 TournamentSize 個不同個體，返回適應度最高者。
```

### 5C. 坩堝窗口構建

#### Prompt
```
請實現 internal/quant/crucible.go，定義多時段坩堝切片的資料結構和構建邏輯。

CrucibleWindow 結構體：Label（"6m"/"2y"/"5y"/"full"）/ Weight（適應度權重）/ Bars（含 warmup 首碼的 K 線切片）/ EvalStartMs（評估區間起點時間戳記，warmup 之後的第一根 bar）

CrucibleResult 結構體：Window 標籤 / Score 分數 / ROI / MaxDD / Alpha（相對 Ghost DCA 的超額收益）

BuildCrucibleWindows 函數：輸入全量歷史 bars（按時間昇冪）和 warmupDays（指標預熱天數，建議 1200 天），輸出四個窗口切片，按 bar 數量昇冪排列（短→長，匹配級聯短路順序）。

四個視窗的構建規則：
- "6m"：評估區間從最新 bar 往前 183 天，評估區間之前再加 warmupDays 天的 warmup 首碼，Weight = 0.10
- "2y"：評估區間 730 天，同上加 warmup，Weight = 0.20
- "5y"：評估區間 1825 天，同上加 warmup，Weight = 0.30
- "full"：評估區間使用資料庫中最早可用 bar 至最新 bar，不設人工天數上限，Weight = 0.40

嚴禁未來資料洩露：每個視窗的評估區間必須從 EvalStartMs 開始，warmup 資料必須在 EvalStartMs 之前，任何情況下不得讓評估區間的計算看到 EvalStartMs 之後的資料。
```

### 5D. 進化任務服務與 HTTP Handler

#### Prompt
```
請閱讀 docs/進化計算引擎.md 第 7 章，然後實現進化任務的服務層和 HTTP Handler。

internal/saas/epoch/service.go：

EpochService 結構體持有 db / EvolutionEngine / logger，以及一個互斥鎖保護的 currentTask 指標（同時只允許運行一個進化任務）。

CreateAndRunTask 函數：
- 檢查是否已有任務在運行，是則返回錯誤
- 解析 CreateTaskRequest（pop_size / max_generations / spawn_mode / spawn_point）
- 在 DB 創建 EvolutionTask 記錄（Status="running"）
- 非同步啟動 runEpoch goroutine（不阻塞 HTTP 回應）
- 返回任務記錄

spawn_mode 處理邏輯：
- "inherit"：從 DB 載入當前 champion 的 SpawnPoint，沒有則用系統預設值
- "random_once"：調用 RandomSpawnPoint() 採樣一次並凍結（整個 Epoch 共用）
- "manual"：使用請求體中的 spawn_point 欄位

internal/saas/api/handler_evolution.go：實現以下三個 Handler：
- POST /api/v1/evolution/tasks：創建並啟動進化任務，僅 lab/dev 模式可用
- GET /api/v1/evolution/tasks：返回當前任務狀態 + 歷次 challenger 清單（含 ScoreTotal / MaxDrawdown / 各視窗分數）
- POST /api/v1/evolution/tasks/:taskID/promote：人工審批晉升，在 DB 事務中執行：當前 champion→retired，challenger→champion；然後刪除 Redis champion 緩存 key
- GET /api/v1/genome/champion：返回當前冠軍基因包，優先從 Redis 緩存讀取，cache miss 則從 DB 載入並寫入緩存
```

---

## Phase 6 — 實例生命週期 + Cron Tick 驅動

### 目標
實現策略實例的創建/啟停/刪除，以及 cron 調度器驅動的 `Step()` 執行迴圈。

### Prompt
```
請閱讀 docs/系統總體拓撲結構.md 第 6 章，然後實現實例生命週期管理模組。

internal/saas/instance/manager.go：

實例狀態機：STOPPED → RUNNING（Start），RUNNING → STOPPED（Stop），任何狀態 → DELETED（Delete），RUNNING → ERROR（異常）

Tick 函數（由 cron 每分鐘掃描 RUNNING 實例時調用）：
步驟一：冪等桶去重檢查。從交易所公開 API 拉取最新 K 線（按實例的 t_micro 聚合週期），獲取最新已完成 bar 的時間戳記，如果該時間戳記 <= PortfolioState.LastProcessedBarTime，則跳過本次 tick（同一聚合桶已處理）。
步驟二：從 DB 讀取實例的 PortfolioState（帳戶快照）和 RuntimeState（策略內部狀態）。
步驟三：從 DB 或 Redis 載入當前冠軍參數包，解析為策略 Params。
步驟四：ACL 外圈處理——將 []Bar 提取為 closes []float64 和 timestamps []int64。
步驟五：構建 StrategyInput（含 Portfolio 快照 + closes + timestamps + 文檔要求的其他參數）。
步驟六：調用 [策略名].Step() 獲取 StrategyOutput（這是唯一調用 Step() 的地方，與回測完全相同的函數）。
步驟七：持久化 RuntimeState。
步驟八：處理底倉釋放意圖——只更新 SaaS 側帳本中的 lot 分類，不向 Agent 下發任何指令，必須寫 AuditLog。
步驟九：將 StrategyOutput 中的宏觀/微觀訂單意圖翻譯為 TradeCommand，格式為 client_order_id = inst{id}-{engine}-{ts}，在 DB 寫入 pending SpotExecution 記錄，通過 WebSocket Hub 下發給對應 Agent。
步驟十：更新 LastProcessedBarTime。

如果 Agent 當前未連接，步驟九記錄警告日誌並跳過下發，等待下次 tick 重試。

internal/saas/cron/scheduler.go：
啟動 cron 基礎掃描（每分鐘一次），遍歷所有 RUNNING 實例，為每個實例併發啟動 Tick goroutine。
```

---

## Phase 7 — LocalAgent（本地執行端）

### 目標
實現極簡的本地執行二進位：從 SaaS 接收 TradeCommand，調用交易所 API，上報 DeltaReport。

### Prompt
```
請閱讀 docs/系統總體拓撲結構.md 第 5 章，然後實現 internal/agent/ 目錄下的 LocalAgent。

鐵律：Agent 不含任何策略代碼；API Key 只存在於 config.agent.yaml；此檔必須在 .gitignore 中。

internal/agent/config/config.go：
AgentConfig 結構體包含 SaaSURL / Email / Password / Exchange 四個欄位。
Exchange 子結構包含 Name / APIKey / SecretKey / Passphrase / Sandbox。
創建 config.agent.yaml 範本（所有金鑰欄位標注為"填寫你的真實值"），並在 .gitignore 中排除此文件。

internal/agent/exchange/bitget.go：
封裝 Bitget REST API v2 現貨下單介面，只需實現兩個方法：
- PlaceOrder(cmd TradeCommand) (Execution, error)：買入時以 QuoteOrderQty 指定 USDT 金額的市價買單，賣出時以 Quantity 指定 BTC 數量的市價賣單
- GetBalances() ([]Balance, error)：獲取帳戶中所有資產的可用和凍結餘額

internal/agent/ws/client.go：
AgentClient 主迴圈，含完整的自動重連邏輯：
- 重連策略：初始等待 1 秒，每次翻倍，最大等待 5 分鐘
- 每次連接建立後的流程：
  1. 調用 SaaS REST API /api/v1/auth/login 獲取 JWT
  2. 建立到 SaaS /ws/agent 的 WebSocket 連接
  3. 立即發送 auth 消息（攜帶 JWT）
  4. 等待 auth_result 確認
  5. 立即發送初始 DeltaReport（當前 Bitget 餘額快照，client_order_id 為空）
  6. 進入消息迴圈
- 消息迴圈處理：
  收到 command 消息時：立即發 command_ack（不等執行完成），然後 goroutine 非同步執行下單，完成後發 delta_report（含 client_order_id + 成交明細 + 當前餘額）
  收到 heartbeat_ack 時：忽略
  每 30 秒發送一次 heartbeat 消息
```

---

## Phase 8 — WebSocket Hub（SaaS 側）

### Prompt
```
請閱讀 docs/系統總體拓撲結構.md 第 5 章，然後實現 internal/saas/ws/ 目錄下的 WebSocket Hub。

設計原則：雲端只信上報，端側無腦執行。

ws/hub.go：連接管理中心
- 用 sync.Map 維護 userID → AgentConn 的映射（每個用戶最多一個 Agent 連接）
- SendToAgent(userID, cmd TradeCommand) error：向指定使用者的 Agent 發送指令，如 Agent 未連接返回錯誤
- HandleConnection(c *gin.Context)：處理新的 WebSocket 連接（路由：GET /ws/agent），流程如下：
  1. HTTP 升級為 WebSocket 連接
  2. 設置 10 秒超時等待第一條消息
  3. 驗證第一條消息必須是 auth 類型，解析並驗證 JWT
  4. 驗證通過後註冊連接，發送 auth_result 成功
  5. 進入消息迴圈：heartbeat → 回 heartbeat_ack；delta_report → 調用 processDeltaReport
  6. 連接斷開時從 Map 中移除

ws/portfolio.go：DeltaReport 處理邏輯
processDeltaReport 函數流程：
1. 根據 client_order_id 找到對應的 pending SpotExecution 記錄，更新為 filled 狀態
2. 根據 SpotExecution 的 LotType 欄位更新 PortfolioState：DEAD_STACK 成交更新 DeadBTC，FLOATING 成交更新 FloatBTC
3. 寫入 TradeRecord
4. 用 DeltaReport.Balances 更新 PortfolioState 中的餘額快照（真實資料來自交易所）
5. 寫審計日誌
6. 發送 report_ack
注意：DeltaReport 中 client_order_id 為空時（Agent 重連初始快照），只更新餘額快照，不更新 lot 記錄。
```

---

## Phase 9 — REST API 路由

### Prompt
```
請實現 internal/saas/api/routes.go 以及各 Handler 檔。

路由結構如下，中介軟體說明：所有 /api/v1/ 下的非 auth 路由需要 JWT 鑒權；lab/evolution 相關路由額外需要 app_role = lab 或 dev 的檢查。

公開路由（無需 JWT）：
POST /api/v1/auth/register
POST /api/v1/auth/login

使用者路由（需要 JWT）：
GET  /api/v1/strategies             列出所有策略範本
GET  /api/v1/strategies/:id         獲取策略範本詳情
GET  /api/v1/instances              列出使用者的策略實例
POST /api/v1/instances              創建實例（需檢查訂閱配額）
POST /api/v1/instances/:id/start    啟動實例
POST /api/v1/instances/:id/stop     停止實例
DELETE /api/v1/instances/:id        刪除實例
GET  /api/v1/instances/:id/lots     獲取實例倉位元詳情
GET  /api/v1/instances/:id/trades   獲取實例成交歷史
GET  /api/v1/dashboard              獲取帳戶總覽資料
GET  /api/v1/agents/status          當前 Agent 連接狀態

Lab 專屬路由（需要 JWT + lab/dev role）：
POST /api/v1/evolution/tasks        創建並啟動進化任務
GET  /api/v1/evolution/tasks        查詢任務狀態和歷次 challenger 清單
POST /api/v1/evolution/tasks/:id/promote  人工審批晉升 challenger
POST /api/v1/backtests              觸發單次回測（指定參數包）
GET  /api/v1/backtests/:id          獲取回測結果
GET  /api/v1/genome/champion        獲取當前冠軍基因包
GET  /api/v1/genome/challengers     列出歷次 challenger

WebSocket 路由：
GET  /ws/agent    Agent 長連接（走 Hub.HandleConnection）
```

---

## Phase 10 — 系統入口（cmd 層）

### Prompt
```
請實現 cmd/saas/main.go 和 cmd/agent/main.go。

cmd/saas/main.go 的啟動順序：
1. 讀取 config.yaml，初始化 zap logger
2. 建立 DB 連接，執行 AutoMigrate（所有模型一次性完成）
3. 建立 Redis 連接
4. 初始化 WebSocket Hub
5. 初始化實例管理器，從 DB 恢復所有 RUNNING 狀態的實例到記憶體
6. 初始化 GA 進化引擎（lab/dev 模式時實際可用，saas 模式時路由層攔截）
7. 啟動 Cron 調度器
8. 啟動 Gin HTTP 伺服器
9. 監聽 SIGTERM/SIGINT，收到信號後執行優雅停機：
   - 停止接受新請求和新 cron 任務
   - 等待正在運行的 tick 完成（超時 30s）
   - 持久化所有活躍實例的 RuntimeState 快照
   - 關閉所有 WebSocket 連接
   - 關閉 DB 和 Redis 連接

cmd/agent/main.go 的啟動順序：
1. 讀取 config.agent.yaml，初始化 logger
2. 初始化 Bitget 交易所用戶端（用設定檔中的 API Key）
3. 初始化 AgentClient，啟動主連接迴圈（含自動重連）
4. 監聽 SIGTERM/SIGINT 優雅退出
```

---

## Phase 11 — 測試與驗證

### Prompt
```
請為以下關鍵模組編寫單元測試，測試目標是驗證行為正確性，不是追求覆蓋率數字。

一、Sigmoid 動態天平測試（internal/quant/micro_engine_test.go）
驗證以下性質（每條寫一個獨立測試用例）：
- 當 Signal > 0 時，TargetWeight 必須 < 0.5
- 當 Signal < 0 時，TargetWeight 必須 > 0.5
- 當 CurrentWeight = 0.5 且 Gamma > 0 時，TargetWeight = 0.5（倉位恰好在中性點時偏置為零）
- 相同輸入兩次調用，結果完全一致（純函數確定性）
- IsQuiet=true 時，|TheoreticalUSD| < 10.1 的情況 OrderUSD 必須為 0
- IsQuiet=false 且滿足楔形突破條件時，|TheoreticalUSD| < 10.1 的情況 OrderUSD 必須等於 ±10.1

二、回測確定性測試（internal/adapters/backtest/adapter_test.go）
用真實歷史資料（或生成的合成資料），相同參數跑兩次回測，斷言所有輸出欄位完全一致。

三、GA 引擎行為測試（internal/saas/ga/engine_test.go）
- 精英保留驗證：一代進化後，上一代 Top N 個體必須出現在新種群中
- 變異斜坡驗證：模擬 EarlyStopPatience 代無改善後，mutProb 應恰好等於初始值 × MutationRampFactor
- Fatal 個體驗證：score=-99999 的個體經過 1000 次錦標賽選擇，被選中次數應極少（< 5%）

四、WebSocket 協議測試（internal/saas/ws/hub_test.go）
- 未認證連接 10 秒後應自動斷開
- 發送有效 DeltaReport 後，對應實例的 PortfolioState 應正確更新

最後運行完整測試套件並修復所有失敗：
go test ./... -race -timeout 300s
```

---

## Phase 12 — Web 前端

### 設計基調

**視覺主題：宇宙暗夜終端（Cosmic Dark Terminal）**

寫在 `web-frontend/` 目錄。整體風格以沉浸式深空背景為底，融合量化終端的資訊密度與現代 SaaS 的設計質感。核心視覺語言如下：

**背景與氛圍**

頁面底層顏色為接近純黑的 `#020617`（deep navy-black），三個半透明光暈疊加在背景上營造空間感：珊瑚色 `#ff8c6b`（左上區域）、天空藍 `#0ea5e9`（右下區域）、青綠色 `#2dd4bf`（中央區域），均施加 100–140px 的 blur 並以 `mix-blend-screen` 模式疊加。背景全域覆蓋噪點紋理（fractal noise SVG，`mix-blend-color-dodge`，約 15% 透明度），模擬高端量化工具的顆粒感。兩個幾何裝飾形狀浮動在背景上層：左側五邊形（青綠色邊框 + 漸變填充）、右側平行四邊形（珊瑚色邊框 + 漸變填充），配合慢速浮動動畫（motion/react）。支持 `prefers-reduced-motion`：若使用者偏好減少動效，降級為靜態背景（移除所有 motion 動畫，保留視覺效果）。

**主色板（Tailwind 自訂 token）**

- 強調色（Accent）：`#2dd4bf`（青綠 / teal）——啟動態導航、邊框高亮、進度條
- 暖色（Warm）：`#ff8c6b`（珊瑚橙）——品牌 icon、警告/注意
- 信息色（Info）：`#0ea5e9`（天空藍）——圖表輔助線、標籤
- 成功色：`#34d399`（綠）——盈利、健康狀態
- 危險色：`#f87171`（紅）——虧損、錯誤狀態
- 警告色：`#fbbf24`（黃）——警告狀態
- 文本主色：`#e2e8f0`（slate-200）
- 文本次色：`#94a3b8`（slate-400）
- 文本弱色：`#64748b`（slate-500）

**字體**

- 介面字體：Inter（`font-sans`）
- 數位 / 代碼字體：JetBrains Mono（`font-mono`）——所有金額、比例、時間戳記均使用

**卡片與元件風格**

玻璃擬態（glassmorphism）：`backdrop-blur`、`border border-white/[0.04]`、`bg-white/[0.02]` 或 `bg-slate-900/40`。啟動態導航項：青綠色邊框 `border-[#2dd4bf]/10` + 背景 `bg-[#2dd4bf]/[0.06]` + 文字 `text-[#2dd4bf]`。細捲軸：寬度 6px，顏色 `#334155`，軌道透明。

**動效原則**

Transition 使用 `duration-150`，平滑但不拖遝。數字更新無翻轉動畫（保持可讀性優先）。頁面內容區滾動流暢，側邊欄固定不滾動。

---

**UI 文案原則（面向使用者的語言）**

所有技術術語必須翻譯為用戶可理解的商業化描述，使用者介面中不得出現：
- 希臘字母裸露（β、γ 等，如必須展示須加括弧說明）
- 內部狀態機術語（如 `DEAD_STACK`、`S3_Panic`）
- 無上下文的數學指標名（如 `TheoreticalUSD`、`VolatilityRatio`）

參考替換規則：

| 內部術語 | 使用者介面展示 |
|---|---|
| DeadStack / DeadBTC | 長期持倉 |
| FloatStack / FloatBTC | 活躍倉位 |
| ColdSealed | 封存資產 |
| TotalEquity | 總資產 |
| SpendableUSDT | 可用資金 |
| Step() 觸發 | 策略決策 |
| RUNNING | 運行中 |
| STOPPED | 已暫停 |
| ERROR | 異常 |
| challenger | 候選參數 |
| champion | 當前最優參數 |
| GA 進化任務 | 參數優化 |
| MarketState | 市場環境 |

---

**技術選型**

- 框架：React 18 + TypeScript + Vite
- 樣式：Tailwind CSS v4（`@import 'tailwindcss'`，`@theme` 定義自訂 token）
- 組件庫：shadcn/ui（暗色主題）
- 動效：motion/react（`framer-motion` 的 ESM 版）
- 圖表：Recharts（NAV 曲線、持倉比例等）
- 狀態管理：Zustand（`authStore`、`systemStatusStore`）
- 服務端狀態 / 請求緩存：TanStack Query（`@tanstack/react-query`）
- HTTP 封裝：原生 fetch，統一 error 類（`ApiRequestError`）
- 路由：React Router v6（`BrowserRouter` + `<Routes>`）
- 國際化：自實現輕量 i18n（`useI18n` hook + `locale` store + JSON 語言包）
- 圖示：lucide-react

---

### 頁面地圖與跳轉關係

```
/login                    登錄頁（AuthScaffold 佈局）
/register                 註冊頁（AuthScaffold 佈局）

/ (AppShell 佈局：左側邊欄 + 頂部狀態列 + 主內容區)
├── /                     Dashboard 總覽（實例卡片 + NAV 曲線 + 策略旅程卡片）
│   └── ?instance=:id     URL 參數選中指定實例，自動啟動對應卡片
│
├── /templates            策略範本目錄（Template 卡片清單，品牌色區分策略類型）
│
├── /instances            實例清單（所有實例狀態概覽，支援刪除）
│   └── /instances/new    創建實例（策略範本選擇 + 資金配額填寫）
│
├── /evolution            進化實驗室（僅 strategies feature 開啟時可見）
│   ├── Tab: optimize     進化任務觸發 + 進度監控 + 候選參數審批
│   └── Tab: library      基因庫（歷次 challenger/champion 歷史記錄）
│
├── /agents               Agent 管理（連接狀態 + API Key 配置入口）
│
├── /backtesting          回測觸發與結果展示（僅 backtesting feature 開啟）
│
└── /settings             帳戶設置（底部導航項）
```

**AppShell 佈局結構**

左側邊欄（Sidebar）：寬度在移動端收窄為純圖示模式（w-16），桌面端展開為圖示 + 文字（w-64）。品牌 icon 區（珊瑚色 `Activity` 圖示）固定在頂部。導航項從 `navItems` 配置中讀取，通過 `hasFeature()` 函數控制是否渲染（feature flag 按 app_role 下發）。`Settings` 固定在側邊欄底部（`placement: 'footer'`）。

頂部狀態列（Topbar）：展示引擎運行狀態（running / paused / halted 三態）、API 連接狀態（Agent 是否線上）、用戶郵箱 + 登出入口。每 30 秒輪詢 `GET /api/v1/system/status`，若需要人工 Reconcile 則彈出 ReconciliationModal。

主內容區：`min-h-0 flex-1 overflow-y-auto p-4 lg:p-6`，內容最大寬度 1800px 居中，使用 Bento Grid（`.qs-bento-grid`，4 列回應式網格）組織卡片。

---

### 12A. 專案腳手架與主題系統

#### Prompt
```
請在專案根目錄的 web-frontend/ 下初始化前端專案，完成視覺主題配置，並搭建 AppShell 佈局骨架。

技術棧要求：
React 18 + TypeScript + Vite，Tailwind CSS v4，shadcn/ui（暗色），motion/react，TanStack Query，Zustand，React Router v6，lucide-react，JetBrains Mono + Inter 字體（Google Fonts）。

視覺主題核心要求：

1. 全域背景色：#020617（純黑深藍）
2. 在 @theme 塊中定義以下 CSS 變數 token（供 Tailwind 使用）：
   - qs-bg: #0f1115
   - qs-surface: rgba(255,255,255,0.04)
   - qs-accent: #2dd4bf（青綠，導航啟動、進度條、高亮邊框）
   - qs-danger: #f87171
   - qs-warn: #fbbf24
   - qs-safe: #34d399
   - font-sans: 'Inter'
   - font-mono: 'JetBrains Mono'

3. 全域動態背景元件（AppBackground）：
   - 三個大光暈球（Framer Motion 慢速迴圈動畫）：
     * 左上：#ff8c6b 珊瑚色，blur-[120px]，mix-blend-screen
     * 右下：#0ea5e9 天空藍，blur-[140px]，mix-blend-screen
     * 中央：#2dd4bf 青綠色，blur-[100px]，mix-blend-screen
   - 動態浮動粒子（20 個白色小點，從底部飄向頂部，迴圈動畫）
   - fractal noise 噪點紋理疊加（opacity 約 0.15，mix-blend-color-dodge）
   - 兩個幾何裝飾形狀（五邊形：青綠色；平行四邊形：珊瑚色），慢速浮動
   - 支持 prefers-reduced-motion：檢測到時改用 StaticBackdrop（僅靜止光暈，無粒子動畫）

4. 細捲軸樣式（.custom-scrollbar）：寬度 6px，顏色 #334155，軌道透明。

5. Bento Grid 系統（.qs-bento-grid）：
   - 移動端：1 列
   - 平板（md）：2 列
   - 桌面（lg）：4 列，支援子元素通過 col-span-{1..4} 控制跨列

AppShell 佈局骨架要求：

左側 Sidebar：
- 固定高度 h-screen，左邊框 border-r-2 border-[#0a0f1c]，背景 bg-[#020617]/40，backdrop-blur-xl
- 品牌區：高 h-16，內含 Activity 圖示（#ff8c6b 珊瑚色，帶 glow shadow），右側品牌名稱文字（桌面端顯示，移動端隱藏）
- 導航區：從 navItems 配置陣列渲染 NavLink，通過 hasFeature() 過濾；啟動樣式：青綠色文字 + 邊框 + 內發光背景；非啟動：slate-500 文字，hover 略亮
- Settings 固定在底部 footer 區域

頂部 Topbar：
- 高 h-16，右側展示三個狀態指示器：API Key 是否配置、Agent 是否線上、引擎運行狀態（running/paused/halted 三態，顏色分別為綠/黃/紅）
- 右側展示用戶郵箱 + 下拉登出按鈕

AuthProvider（src/app/AuthProvider.tsx）：
- 應用啟動時從本機存放區恢復 JWT，調用 GET /api/v1/auth/me 驗證有效性
- 提供 user、loading、login、logout 方法

AppRouter（src/app/router.tsx）：
- /login 和 /register 使用 AuthScaffold（居中佈局，背景複用 AppBackground）
- / 根路由使用 AppShell，內嵌 AuthGate（未登錄重定向 /login）
- 各功能路由通過 hasFeature() 決定是否渲染，無許可權時 Navigate 回首頁
```

---

### 12B. 認證頁（Login + Register）

#### Prompt
```
請實現 src/features/auth/LoginPage.tsx 和 RegisterPage.tsx，佈局使用 AuthScaffold（居中，背景為 AppBackground，支援暗色光暈效果）。

視覺要求：
- 卡片寬度 400px，背景半透明（玻璃擬態：border border-white/10, backdrop-blur-xl, bg-slate-900/60）
- 頂部：Activity 圖示（珊瑚色 #ff8c6b）+ 系統名稱（大號字，slate-200）+ 簡短 slogan
- 表單字段：郵箱 + 密碼（登錄），郵箱 + 密碼 + 確認密碼（註冊）
- 輸入框：暗色背景 bg-slate-900/80，border-slate-700，focus 時邊框變為青綠色 #2dd4bf
- 提交按鈕：全寬，青綠色背景，載入中顯示 Loader2 旋轉圖示並禁用
- 錯誤提示：紅色小字，顯示在按鈕下方
- 底部切換連結："沒有帳號？註冊" / "已有帳號？登錄"

交互邏輯：
- 登錄/註冊成功後通過 AuthProvider 的 login() 方法存儲 JWT，跳轉至 /
- 表單提交期間禁用所有輸入和按鈕
- 字母間距使用 tracking-wider，按鈕文字 uppercase

對接介面：POST /api/v1/auth/login 和 POST /api/v1/auth/register
```

---

### 12C. Dashboard 總覽頁

#### Prompt
```
請實現 src/features/dashboard/DashboardPage.tsx，這是用戶登錄後的主頁。

頁面整體採用 Bento Grid（.qs-bento-grid）佈局，卡片使用玻璃擬態風格（半透明邊框，backdrop-blur）。

左側實例選擇區（桌面端約占 1/4 寬度）：
- 列出所有策略實例，以卡片清單形式展示
- 每張實例卡片：實例名稱 + 交易對 + 狀態徽章（運行中/已暫停/異常，顏色對應青綠/灰/紅）
- 點擊卡片選中該實例，啟動態用青綠色左邊框高亮
- 卡片底部：啟動/暫停按鈕（運行中顯示暫停，已暫停顯示啟動），點擊直接調用 PATCH /api/v1/instances/:id
- 頁面頂部有"前往配置"入口（Settings 圖示按鈕），跳轉至 ConfigFormSheet（側滑面板）
- 底部有"新建實例"按鈕，跳轉 /instances/new
- URL 參數 ?instance=:id 支援直接選中指定實例（頁面載入時自動啟動）

右側主展示區（約占 3/4 寬度，縱向排列）：

上方：策略概況卡片（StrategyOverviewCard）
- 展示已選實例的總資產、資產分佈（長期持倉 / 活躍倉位 / 可用資金 / 封存資產）
- 顯示實例當前狀態、最後決策時間
- 數位使用 font-mono

中部：PnL 淨值曲線（PnLChart 元件，Recharts AreaChart）
- X 軸：時間，Y 軸：總資產（USDT）
- 折線色為青綠 #2dd4bf，面積半透明填充
- 支持時間範圍切換（7天/30天/90天），當前選中項用青綠色高亮
- 資料來源：GET /api/v1/dashboard/equity-snapshots?instance_id=:id
- 載入中顯示 Skeleton（PnLChartSkeleton）

下方：策略旅程卡片（StrategyJourneyCard）
- 展示當前實例的策略運行關鍵里程碑（首次執行時間、累計決策次數、本月成交次數等）
- 使用者友好語言，不暴露內部欄位名

數據輪詢：實例清單每 60 秒刷新，equity 曲線每 60 秒刷新（TanStack Query refetchInterval）。
```

---

### 12D. 策略範本頁 + 實例管理頁

#### Prompt
```
請實現以下三個頁面：

一、src/features/strategies/TemplatesPage.tsx（/templates）
展示所有可用的策略範本，以卡片網格排列。每張範本卡片包含：
- 策略名稱（用戶可理解的商業化名稱，如"動態均衡策略"，而非 lunar-btc）
- 支援的交易對與交易所
- 簡短策略描述（不超過 2 行，聚焦用戶價值，不描述內部演算法）
- 策略色彩標識（每個策略有專屬的品牌色，用於卡片左邊框/頂部裝飾條）
- 是否支持進化優化的標籤
- "創建實例"按鈕，點擊跳轉 /instances/new?template=:id

範本資料來自本地 strategyCatalog.ts 設定檔（靜態配置，無需從後端獲取），該檔定義每個策略的 UI 展示屬性：名稱、描述、色彩、支持的 feature。

二、src/features/strategies/InstanceListPage.tsx（/instances）
展示當前使用者的所有實例，以清單形式排列（非網格）。每行包含：
- 實例名稱 + 策略類型 + 交易對
- 狀態徽章
- 總資產（font-mono）
- 創建時間（相對時間，如"3天前"）
- 操作列：跳轉 Dashboard（?instance=:id）、跳轉進化頁（/evolution?instance=:id）、刪除按鈕（需二次確認）
刪除時顯示 Trash2 圖示，點擊後出現內聯確認按鈕（避免 modal 打斷流程）。
頂部有"創建新實例"按鈕跳轉 /instances/new。

三、src/features/strategies/InstanceCreatePage.tsx（/instances/new）
分步表單（兩步）：
第一步：選擇策略範本（展示與 TemplatesPage 相似的卡片，選中後高亮，點擊下一步）
第二步：填寫實例配置：
  - 實例名稱（自訂標識）
  - 初始資金配額（USDT）
  - 月度注資金額（USDT，可選）
  - 封存資產量（可選，"永不釋放"的底倉）
  - 風險偏好（最大可用回撤，滑塊選擇）
提交後調用 POST /api/v1/instances，成功後跳轉 /（Dashboard），URL 帶 ?instance=:id 自動聚焦新實例，並通過 React Router location.state 展示成功 notice。
```

---

### 12E. 進化實驗室頁

#### Prompt
```
請實現 src/features/strategies/EvolutionPage.tsx（/evolution）。

頁面頂部：實例選擇器（下拉或卡片清單，僅顯示支援進化的實例），URL 參數 ?instance=:id 支援直接選中。

頁面主體分兩個 Tab（optimize / library）：

Tab: optimize（參數優化）
分三個區域從上到下排列：

1. 進化控制台（EvolutionPanel 組件）
   - 如無運行中任務：展示"啟動新一輪優化"按鈕，點擊後展開參數配置區
     * 種群大小（10-500，默認300）
     * 最大代數（5-50，默認25）
     * 參數繼承方式（單選：繼承當前最優 / 隨機探索 / 手動指定）
     * 手動指定時：JSON textarea 輸入區，帶格式提示
     * 提交按鈕調用 POST /api/v1/evolution/tasks
   - 如有運行中任務：展示任務進度卡
     * 當前代 / 最大代進度條（青綠色）
     * 當前最優評分（數字，font-mono）
     * 最大回撤（紅色顯示，font-mono）
     * 每 5 秒輪詢 GET /api/v1/evolution/tasks 刷新
     * "終止任務"按鈕（需確認）

2. 任務佇列視圖（TaskQueueView 元件）：歷史任務清單，每項展示狀態、評分、耗時。

3. 冠軍基因展示：當前 champion 的核心指標（綜合評分、各視窗分數、最大回撤），用青綠色邊框卡片高亮，頂部標注"當前最優參數"。附"應用到實例"按鈕，點擊後跳轉 Dashboard 對應實例。

Tab: library（基因庫）
展示所有歷史基因記錄（GenomeLibrary 元件）：
- 卡片清單，按時間倒序
- 每張卡片：角色標籤（候選參數/當前最優/歷史歸檔）、產出時間、綜合評分、各視窗分數（6m/2y/5y/全量）、最大回撤
- 當前 champion 用青綠色邊框高亮
- challenger 卡片有"晉升為最優"按鈕，點擊後有二次確認彈窗（說明晉升影響），調用 POST /api/v1/evolution/tasks/:id/promote
- 可跳轉回測頁查看該基因的完整回測報告（/backtesting?genome=:id）

資料來源：GET /api/v1/evolution/tasks、GET /api/v1/evolution/genomes
```

---

### 12F. Agent 管理頁 + 回測頁

#### Prompt
```
一、src/features/agents/AgentsPage.tsx（/agents）

頁面展示 LocalAgent 的連接狀態與配置指引。

上方：連接狀態卡片
- 大號狀態指示器：線上（青綠色脈衝圓點 + "執行端已連接"）/ 離線（灰色 + "執行端未連接，交易將暫停"）
- 最後心跳時間（font-mono）
- Agent 版本號（如已連接）
- 資料來源：SystemStatusContext 中的 api_connected 欄位，每 30 秒自動刷新

中間：Agent 配置說明區
以步驟卡片形式引導使用者完成 LocalAgent 配置：
Step 1: 下載 LocalAgent 二進位（提供下載連結占位元）
Step 2: 創建 config.agent.yaml（提供配置範本，高亮提示：API Key 僅存本地，永不上傳）
Step 3: 運行 Agent，確認連接

下方：API Key 配置檢查
展示當前 SaaS 側檢測到的 API 配置狀態（api_configured 欄位）。
注意：API Key 本身永不展示在前端，僅顯示"已配置"/"未配置"。

二、src/features/backtesting/BacktestingPage.tsx（/backtesting）

頂部：實例選擇器（下拉，僅顯示有 champion 基因的實例）

觸發區（表單卡片）：
- 參數來源（單選）：使用當前最優參數 / 使用指定候選參數（下拉選 challenger）/ 自訂 JSON
- 點擊"開始回測"，調用 POST /api/v1/backtests，顯示載入動畫

結果展示區（完成後渲染）：
- 概要指標卡片（Stats Cards）：總收益率 / 相對 DCA 的 Alpha / 最大回撤 / 夏普比率（如有），使用 font-mono
- NAV 曲線（Recharts AreaChart，青綠色）
- Ghost DCA 基準線疊加（虛線，slate-500 色），直觀對比策略 vs 被動 DCA 的差距
- 各時間段分解：6個月/2年/5年/全量 評分卡片

資料來源：POST /api/v1/backtests（觸發），GET /api/v1/backtests/:id（輪詢結果，每3秒一次直至完成）
```

---

### 12G. 萬用群組件庫與服務層

#### Prompt
```
請實現以下萬用群組件和服務層，供各頁面複用。

一、src/shared/ui/Card.tsx
基礎玻璃擬態卡片元件：border border-white/[0.04]，bg-slate-900/20，backdrop-blur，rounded-xl。
接受 className 進行樣式擴展。

二、src/shared/ui/StatusBadge.tsx（或 StatusPill）
狀態徽章：接受 status（running/stopped/error/halted）映射為顏色（青綠/灰/紅/紅）和用戶友好文案（運行中/已暫停/異常/已中斷）。圓角 pill 樣式，帶實心小圓點。

三、src/shared/ui/skeletons/
常用 Skeleton 占位元組件：PnLChartSkeleton（灰色矩形占位區）、CardSkeleton、TableSkeleton。
背景色使用 bg-slate-800/40，配合 animate-pulse。

四、src/shared/services/
HTTP 服務層，統一封裝 fetch 請求：
- 所有請求自動附加 Authorization: Bearer {token}（從 authStore 讀取）
- 回應非 2xx 時拋出 ApiRequestError（含 status 和 message）
- 401 時觸發 authStore.logout() 並跳轉 /login
- 按業務域分檔：instances.ts / dashboard.ts / evolution.ts / backtests.ts / system.ts

五、src/shared/config/features.ts
Feature flag 系統：
- AppFeature 類型：'dashboard' | 'strategies' | 'agents' | 'risk' | 'backtesting' | 'settings'
- hasFeature(feature: AppFeature): boolean，從初始化 API 回應或 app_role 判斷

六、src/shared/config/navigation.ts
navItems 陣列配置：每項包含 to（路由）、labelKey（i18n key）、icon（LucideIcon）、placement（main/footer）、feature（可選，用於 hasFeature 過濾）、end（用於 NavLink 精確匹配）

七、src/i18n/
輕量 i18n 實現：
- I18nProvider 包裹應用，提供 locale（zh/en）切換
- useI18n() hook 返回 t(key) 翻譯函數和當前 locale
- 語言包 JSON 檔：zh.json 和 en.json，覆蓋所有 nav、common、頁面內文案
- 所有面向使用者的字元串通過 t() 獲取，不硬編碼

八、src/stores/authStore.ts（Zustand）
- 存儲 user（含 email、role）和 JWT token
- 應用啟動時自動從 localStorage 恢復 token 並驗證（GET /api/v1/auth/me）
- 提供 login(token, user) / logout() / loading 狀態
```

---

### 驗收要點

- 所有金額保留 2 位小數，BTC / 標的資產數量保留 6 位小數，均使用 font-mono
- 頁面全域搜索：不得在用戶可見文案中出現 `DeadBTC`、`FloatBTC`、`Step()`、`VolatilityRatio`、`TheoreticalUSD`、`lunar` 等內部術語
- 主視覺背景（三色光暈 + 噪點 + 幾何裝飾）必須在所有頁面一致呈現
- Sidebar 啟動態必須使用青綠色（`#2dd4bf`）高亮，非啟動態使用 slate-500
- 支持 `prefers-reduced-motion`：減動效模式下背景光暈靜止、無浮動粒子
- 桌面端（lg 1024px+）Sidebar 展開為圖示+文字（w-64），移動端收窄為純圖示（w-16）
- Feature flag 保證無許可權路由自動重定向至首頁，不顯示 404
- 前端構建命令：`cd web-frontend && npm run build`，產物輸出至 `web-frontend/dist/`
- SaaS 後端新增靜態檔服務，將 `dist/` 託管在 `/` 根路徑，API 路由保持 `/api/v1/` 首碼

---

## Phase 13 — Docker 部署配置

### Prompt
```
請創建生產部署所需的全部設定檔。

saas.Dockerfile：多階段構建，builder 階段使用 golang:1.21 編譯 cmd/saas/main.go，最終鏡像基於 alpine，只複製編譯好的二進位檔案和 config.yaml。

agent.Dockerfile：同上，編譯 cmd/agent/main.go，最終鏡像只包含 agent 二進位。

docker-compose.yml：適用于本地開發和 lab 模式，包含三個服務：
- postgres（postgres:15，創建 quantsaas 資料庫，持久化到 named volume）
- redis（redis:7-alpine）
- saas（從 saas.Dockerfile 構建，APP_ROLE 通過環境變數注入，預設 dev，depends_on postgres 和 redis，暴露 8080 埠）

說明：agent 二進位設計為使用者在本地直接運行（而不是容器），因為它需要訪問使用者本地的 config.agent.yaml 金鑰文件。

.gitignore 必須包含：
config.agent.yaml
*.env
*.exe（Windows 編譯產物）
bin/（本地編譯輸出目錄）
```

---

## 完整驗收檢查清單

構建完成後，逐項確認以下檢查項：

**架構鐵律（用 grep 驗證）：**
```bash
# 確認策略包內無 isBacktest 分支
grep -r "isBacktest" internal/strategies/      # 期望：無結果

# 確認 SaaS 側無 API Key 欄位
grep -r "api_key\|secret_key\|passphrase" internal/saas/  # 期望：無結果

# 確認策略內核不依賴 Bar 結構體
grep -r "quant\.Bar" internal/strategies/      # 期望：無結果

# 確認策略包內無 I/O 調用
grep -r "http\.\|sql\.\|os\.Open\|time\.Now" internal/strategies/  # 期望：無結果
```

**功能驗證：**
- `go build ./...` 無錯誤
- `go test ./... -race` 全部通過
- 相同參數回測兩次，所有輸出欄位完全一致
- GA 在 test_mode（Pop=10, Gen=3）下能完整運行並寫入 challenger 記錄
- 同一 bar 時間戳記，Cron Tick 對同一實例不會重複推進（冪等驗證）

**安全檢查：**
- `config.agent.yaml` 在 `.gitignore` 中
- `git status` 確認 `config.agent.yaml` 未被追蹤
- 未鑒權的 WebSocket 連接在 10 秒內斷開
- app_role=saas 時訪問 lab 專屬路由返回 403

---

## 關鍵參數參考表

| 模組 | 參數 | 預設值 | 含義 |
|---|---|---|---|
| GA | PopSize | 300 | 種群大小 |
| GA | MaxGenerations | 25 | 最大代數 |
| GA | EliteCount | 8 | 精英保留數量 |
| GA | TournamentSize | 3 | 錦標賽參與人數 |
| GA | MutationProbability | 0.15 | 初始變異概率 |
| GA | MutationScale | 1.0 | 初始變異幅度 |
| GA | MutationProbabilityMax | 0.55 | 變異概率上限 |
| GA | MutationScaleMax | 3.0 | 變異幅度上限 |
| GA | MutationRampFactor | 1.25 | 斜坡放大倍率 |
| GA | EarlyStopPatience | 5 | 無改善代數觸發斜坡 |
| GA | EarlyStopMinDelta | 0.001 | 改善閾值 |
| 適應度 | Fatal 回撤閾值 | 88% | 超過則硬否決 |
| 適應度 | DD 超額懲罰係數 | 1.5× | 超額回撤的罰分倍率 |
| 微觀 | EMA/σ 窗長 | 21 根 | 不可進化的固定常量 |
| 微觀 | VolRatio 長窗口 | 112 根 | 楔形過濾分母 |
| 微觀 | VolRatio 短窗口 | 16 根 | 楔形過濾分子 |
| 微觀 | 最小訂單閾值 | 10.1 USDT | 粉塵攔截邊界 |
| 系統 | 心跳間隔 | 30 秒 | Agent → SaaS |
| 系統 | 重連初始等待 | 1 秒 | 指數退避起點 |
| 系統 | 重連最大等待 | 5 分鐘 | 指數退避上限 |
| 系統 | Auth 超時 | 10 秒 | 未認證連接自動斷開 |
| 帳本 | micro_reserve_pct 默認 | 0.25 | 微觀層資金保留比例 |

---

## ⚙️ [OPTIONAL] Phase 13 — AI 多維信號層（LLM 輔助開單）

> **此 Phase 為可選擴展，不影響主系統運行。**
> 在核心系統（Phase 0–12）穩定運行之後，再考慮接入。
> 核心鐵律不變：AI 信號的生成必須發生在 `Step()` 調用之前的 cron 外圈，LLM 調用結果以快照形式注入 `StrategyInput`，`Step()` 內部保持純函數。

---

### 設計思路

當前 Sigmoid 動態天平的 `Signal` 由純技術指標驅動。本 Phase 引入一個獨立的 **AI 信號層**，讓 LLM 週期性地閱讀三類外部資訊，生成一個三維信號向量，作為獨立的附加項疊加進 Sigmoid 的 Exponent，與技術信號共同決定目標倉位元。

**三個維度的定義：**

| 維度 | 含義 | 取值範圍 | 信號方向 |
|---|---|---|---|
| S_market（行情面） | AI 對近期價格結構、關鍵位元、宏觀趨勢的判斷 | [-1, 1] | 正值 = 傾向減倉，負值 = 傾向加倉 |
| S_news（消息面） | AI 對近期新聞標題/公告的利多/利空評估 | [-1, 1] | 同上 |
| S_sentiment（情緒面） | AI 對市場情緒指標（恐貪指數、資金費率等）的綜合判斷 | [-1, 1] | 同上 |

**與 Sigmoid 動態天平的聯動方式：**

AI 三維信號向量加權合成為一個標量 `AISignal`，以獨立項疊加進 Exponent：

```
AISignal = w1 × S_market + w2 × S_news + w3 × S_sentiment

Exponent = EffectiveBeta × TechSignal
         + AIBeta × AISignal
         + γ × InventoryBias
```

其中 `w1 / w2 / w3 / AIBeta` 均為可進化參數，加入染色體，由 GA 搜索各維度的實際貢獻權重。當 GA 發現某個維度對收益無貢獻時，對應權重會自然收斂到 0。

**更新頻率與緩存策略：**
AI 信號每 4 小時更新一次，結果存入 Redis，TTL 為 4 小時。cron tick 讀緩存，cache miss 時觸發一次 LLM 調用並寫入緩存。LLM 調用失敗時降級為 `[0, 0, 0]` 中性向量，系統繼續正常運行，不因 AI 服務不可用而中斷交易。

---

### 資料來源設計

| 維度 | 資料來源 | 說明 |
|---|---|---|
| 行情面 | 系統自身 K 線資料庫 | 從已有 KLine 表提取近期 OHLCV 摘要 + 關鍵均線位置，無需外部 API |
| 消息面 | CryptoPanic API | 免費層提供近 24 小時加密貨幣新聞標題與來源，按標的過濾 |
| 情緒面 | Alternative.me Fear & Greed Index API | 免費，提供當日恐貪指數；可疊加 Coinglass 資金費率 API |

---

### Context（寫給 AI 的背景）

這一層的核心難點不在技術實現，而在 **Prompt Engineering**：如何讓 LLM 穩定地輸出結構化的三維評分，而不是自由發揮。LLM 返回的內容必須經過嚴格的解析和範圍夾緊，任何解析失敗都應靜默降級，不拋出異常影響主流程。

---

### Prompt

```
請閱讀 docs/策略數學引擎.md，理解 Sigmoid 動態天平的 Exponent 結構，然後實現 AI 多維信號層。

架構約束（必須遵守）：
- AI 信號的 LLM 調用發生在 cron tick 中，Step() 調用之前
- Step() 是純函數，不得在內部發起任何網路請求
- AISignalVector 作為欄位加入 StrategyInput 結構體，以快照形式注入

請實現以下內容：

一、資料獲取層 internal/saas/ai/collector.go
實現三個採集函數，各自獨立，互不依賴：

MarketSummary：從 KLine 表讀取指定標的近期 K 線（近 7 天日線 + 近 24 小時小時線），計算並格式化以下摘要字串：當前價格、距近期高點/低點的百分比距離、多條 EMA 的相對位置（價格高於/低於均線多少百分比）。輸出為一段結構化文字，供後續 LLM 調用使用。

NewsSummary：調用 CryptoPanic API（https://cryptopanic.com/api/v1/posts/）獲取指定標的近 24 小時新聞標題列表，拼接為 bullet list 格式字串。API Key 存在 config.yaml 中（非 agent 配置）。獲取失敗時返回空字串。

SentimentSummary：調用 Alternative.me Fear & Greed API（https://api.alternative.me/fng/）獲取當日恐貪指數（數值 + 文字描述），格式化為一行文字。獲取失敗時返回 "Fear & Greed: unavailable"。

二、LLM 評分服務 internal/saas/ai/scorer.go
實現 ScoreAISignal 函數，接收三個摘要字串，調用 Claude API（claude-haiku-4-5-20251001 模型，低成本），返回 AISignalVector{SMarket, SNews, SSentiment float64}。

System prompt 要求 LLM 扮演"資深加密貨幣量化分析師"，根據輸入資訊對三個維度各自打分，遵循以下規則：
- 分數範圍 [-1.0, 1.0]，正值表示當前信號傾向降低倉位元（偏空），負值表示傾向提升倉位（偏多），0 表示中性
- 必須以純 JSON 格式返回，格式為 {"s_market": 0.0, "s_news": 0.0, "s_sentiment": 0.0}，不得包含任何額外文字
- 當資訊不足或不確定時，對應維度輸出 0

回應解析：嚴格解析 JSON，若解析失敗或欄位缺失則返回 [0, 0, 0] 中性向量；將所有值 clamp 到 [-1.0, 1.0]。LLM 調用超時設為 15 秒。

三、緩存服務 internal/saas/ai/cache.go
實現 GetCachedSignal 和 SetCachedSignal 兩個函數，使用 Redis 存儲 AISignalVector，key 格式為 ai_signal:{symbol}，TTL 為 4 小時。序列化方式使用 JSON。

四、信號編排服務 internal/saas/ai/service.go
實現 FetchAISignal(ctx, symbol) AISignalVector 函數：
先查 Redis 緩存，命中則直接返回；
未命中則串列調用 MarketSummary + NewsSummary + SentimentSummary，拼接後調用 ScoreAISignal；
寫入緩存後返回；
任何環節出錯均靜默降級，返回 [0, 0, 0]，寫 warn 日誌，不返回 error 給上層。

五、注入 StrategyInput
在 quant/data.go 的 StrategyInput 結構體中新增欄位 AISignalVector（三個 float64 欄位 SMarket/SNews/SSentiment）。在 cron tick 的 Tick 函數中，Step() 調用之前，調用 FetchAISignal 並將結果寫入 StrategyInput。

六、擴展 Chromosome
在 Chromosome 結構體中新增四個可進化參數：
- W1（行情面權重）：邊界 [-2, 2]，默認 0（初始中性，由 GA 自行發現貢獻）
- W2（消息面權重）：邊界 [-2, 2]，默認 0
- W3（情緒面權重）：邊界 [-2, 2]，默認 0
- AIBeta（AI 信號整體激進係數）：邊界 [0, 3]，默認 0

七、擴展微觀引擎
在 MicroDecisionInput 結構體中新增 AISignalVector 和對應的權重參數欄位。
在 ComputeMicroDecisionV4 函數中，在現有 Exponent 計算之後疊加 AI 信號項：
AISignal = W1 × SMarket + W2 × SNews + W3 × SSentiment
Exponent += AIBeta × AISignal
其他邏輯不變。

八、config.yaml 新增配置項
在 config.yaml 中新增 ai 配置塊，包含 claude_api_key（從環境變數 ANTHROPIC_API_KEY 注入）和 cryptopanic_api_key（從環境變數 CRYPTOPANIC_API_KEY 注入）。兩個 key 均不得硬編碼，不得出現在任何代碼檔中。
```

---

### 驗收要點

- `grep -r "ANTHROPIC_API_KEY\|api_key" internal/` 中不應出現硬編碼金鑰
- LLM 服務宕機時，回測和實盤均應正常繼續（AISignalVector = [0,0,0]）
- GA 回測中 AISignalVector 由

