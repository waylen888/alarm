# alarm

可嵌入的 Go 告警狀態機。負責「條件成立 → 告警 → 恢復」這段狀態管理，
讓呼叫端只需要回答兩件事：**什麼算一筆觀測**、**事件發生時要做什麼**。

[English](README.md)

```go
import "github.com/waylen888/alarm"
```

零依賴：本 package 只 import 標準函式庫，往後也會維持這條線。

## 目錄

- [這是什麼](#這是什麼)
- [快速開始](#快速開始)
- [核心概念](#核心概念)
- [狀態機](#狀態機)
- [API](#api)
- [條件目錄](#條件目錄)
- [統計製程管制：spc 子套件](#統計製程管制spc-子套件)
- [Escalate 與 Exit](#escalate-與-exit)
- [熱更新與 Fingerprint](#熱更新與-fingerprint)
- [資料存在性：Stale / Vanish / MaxKeys](#資料存在性stale--vanish--maxkeys)
- [Event.Meta：事件自帶素材](#eventmeta事件自帶素材)
- [併發與生命週期](#併發與生命週期)
- [視窗容量與上限](#視窗容量與上限)
- [生產環境的使用場景](#生產環境的使用場景)
- [限制](#限制)
- [設計不變量](#設計不變量)
- [授權](#授權)

---

## 這是什麼

這是一個函式庫，不是一套服務。沒有抓取迴圈、沒有查詢語言、沒有通知投遞、沒有儲存。
你餵入觀測，它決定每個 key 何時在 OK / Pending / Firing / Stale / 消失之間轉移，
並以 `Event` 呼叫你的 handler。

對照組是 Prometheus Alertmanager 或 Grafana alerting。它們在自己的領域做得很好：
以基礎設施的形式運行，對時序資料庫評估規則，替整個機隊路由通知。
本 package 針對的是另一種情況——你需要的是**在自己的行程內**擁有告警狀態機語意，
資料本來就在記憶體裡，而且不想為此多架一套外部基礎設施。
自我監控的 agent、網路探測程式、log tailer、對自身內部計數器告警的服務都屬於這類。
如果資料本來就在 Prometheus 裡，那就用 Alertmanager。

### 職責邊界

| 引擎負責 | 引擎不負責 |
| --- | --- |
| 條件評估、狀態轉移、去重 | 訊息文案、通知內容 |
| 持續時長（`For`）、恢復阻尼（`ClearFor`） | 告警歷史落庫、通知管道投遞 |
| 多級告警的升降級 | 閾值怎麼比（由 closure 帶入） |
| 資料中斷（`Stale`）／消失（`Vanish`）偵測 | 取樣、抓取、解析 |
| 單一 rule+key 的補發節流（`Reminder`） | 跨規則的通知靜默窗 |
| cardinality 上限（`MaxKeys`） | 收件人解析、寄送聚合 |

### 刻意不做的事

- **不做跨子系統的全域單例**。每個子系統各建一個 `Engine` 實例、各自帶 handler。
  單一子系統內要不要用 package 級實例是該子系統的事——例如以 package 級引擎
  讓告警狀態跨連線重建存活，是合理的做法。
- **一個 key 只承載一條純量序列**。多維度指標請由呼叫端編碼成單一數值
  （把封包遺失率與延遲一起編成 0/1/2 的嚴重度），或拆成不同 rule 共用同一組 key。
- **不管通知層節流**。跨多條規則共用、甚至連初次告警也吞的靜默窗，
  與 per-rule 的 `ClearFor` ＋ `Reminder` 語意並不等價，依「引擎只管狀態」的原則
  整段留在呼叫端。

---

## 快速開始

```go
// 1. 建引擎，handler 決定事件怎麼變成通知
engine := alarm.New(func(ev alarm.Event) {
    switch ev.Kind {
    case alarm.EventFire:
        sendAlert(ev.Key, ev.Value)
    case alarm.EventResolve:
        sendResolved(ev.Key)
    }
})

// 2. 裝規則（可隨設定變更重複呼叫，見「熱更新」）
engine.SetRules([]alarm.Rule{{
    ID: "cpu-high",
    Levels: []alarm.Level{{
        Severity:  alarm.SeverityError,
        Condition: alarm.ConsecutiveN(3, func(v float64) bool { return v > 90 }),
    }},
    StaleAfter:  -1, // 不做資料中斷偵測：收集停止即狀態凍結
    VanishAfter: -1,
}})

// 3. 每輪收集餵入觀測，key 用來區分同一規則下的多個實體
engine.Observe("cpu-high", agentID, cpuPercent, time.Now())
```

需要時間推進（視窗衰減、`Stale`／`Vanish`、`Reminder` 補發）時再加上時鐘來源：

```go
go engine.Run(ctx, 10*time.Second) // 或由呼叫端既有迴圈呼叫 engine.Tick(now)
```

完整可執行的範例見 [`example_test.go`](example_test.go)。

---

## 核心概念

### Rule

一條規則可同時追蹤多個 **key**（多台 agent、多條 series、多條線路），
每個 key 各自獨立走狀態機。

| 欄位 | 型別 | 語意 |
| --- | --- | --- |
| `ID` | `string` | 規則識別。`SetRules` 以此比對 |
| `Levels` | `[]Level` | 至少一個。由高至低評估，取最高成立等級 |
| `For` | `time.Duration` | 條件需持續多久才 Firing（`<=0` 立即） |
| `Fingerprint` | `string` | 條件語意指紋。變更即整條重建，見「熱更新與 Fingerprint」 |
| `Reminder` | `time.Duration` | Firing 期間補發間隔（`<=0` 關閉） |
| `ClearFor` | `time.Duration` | 條件解除需持續多久才 Resolve（flap 阻尼，`<=0` 立即） |
| `Clear` | `Condition` | 次數制恢復條件。設定後 `ClearFor` 不生效 |
| `StaleAfter` | `time.Duration` | 無觀測多久轉 Stale（`0`＝引擎預設，預設關閉；`<0` 關閉） |
| `VanishAfter` | `time.Duration` | 無觀測多久清狀態（`0`＝引擎預設 1 小時；`<0` 關閉） |
| `MaxKeys` | `int` | 本規則的 cardinality 上限（`0`＝引擎預設 1000） |
| `KeepWindowOnStale` | `bool` | 進入 Stale 時保留視窗（預設清空） |

### Level 與多級告警

```go
Levels: []alarm.Level{
    {Severity: alarm.SeverityError, Condition: errCond, Escalate: escCond, Exit: exitCond},
    {Severity: alarm.SeverityWarn,  Condition: warnCond},
}
```

共三個等級：`SeverityInfo`、`SeverityWarn`、`SeverityError`。單級告警給一個 `Level` 即可。
`Escalate` 與 `Exit` 見「Escalate 與 Exit」——用多級告警前請先讀完那一節。

### Condition 與 Window

`Condition` 是**無狀態**的判斷函式，唯一輸入是該 key 的觀測視窗 `Window`。
狀態全由引擎持有，所以規則熱更新時可以安全替換條件。

```go
type Condition interface{ Breach(w Window) bool }

type Window interface {
    Last() (Point, bool)                       // 最新一筆
    LastN(n int) []Point                       // 最新 n 筆，由舊到新
    Points(since time.Duration) []Point        // 回推 since 內的觀測
    Count(since time.Duration) int             // 回推 since 內的筆數
    Delta(since time.Duration) (float64, bool) // counter 差分，遇 reset 歸零重計
}
```

`since` 一律以「評估當下」回推，所以時間視窗型條件會隨時間自然衰減，由 `Tick` 驅動解除。

### Event

引擎的唯一輸出。

```go
type Event struct {
    RuleID   string
    Key      string
    Kind     EventKind
    Severity Severity
    State    State     // 轉移後狀態
    Value    float64   // 命中條件的量測值（次數／速率／最後觀測值，依條件而定）
    Since    time.Time // 本事件所報告狀態的起始時間
    At       time.Time
    Meta     any       // ObserveMeta 附帶的素材，引擎原樣帶回
}
```

---

## 狀態機

```
              條件成立              持續達 For
      OK ─────────────────► Pending ─────────────► Firing
      ▲                        │                     │
      │   條件解除（未達 For）  │                     ├─► Reminder（每 Reminder 補發一次）
      ├────────────────────────┘                     ├─► Escalate / Deescalate（多級內部升降級）
      │        （靜默）                               │
      └──────────── Resolve ─────────────────────────┘
              （ClearFor 或 Clear 滿足）

  任一狀態 ──── 逾 StaleAfter 無觀測 ────► Stale ──── 資料恢復 ────► 還原斷線前狀態
                                            │                    （StaleRecover）
  任一狀態 ──── 逾 VanishAfter 無觀測 ───────┴────────────────────► 清除狀態
                                                        （原本 Firing 才發 Vanish）
```

| State | 意義 |
| --- | --- |
| `StateOK` | 正常，或從未達標 |
| `StatePending` | 條件成立但未滿 `For`，靜默等待（不發事件） |
| `StateFiring` | 告警中 |
| `StateStale` | 逾 `StaleAfter` 無觀測，資料中斷 |

| EventKind | 何時送出 |
| --- | --- |
| `EventFire` | 首次達標（`For` 滿足後）。`Since` 為首次達標時間 |
| `EventEscalate` / `EventDeescalate` | Firing 中等級升高／降低 |
| `EventReminder` | Firing 期間每 `Reminder` 補發一次 |
| `EventResolve` | 條件解除（`ClearFor` 或 `Clear` 滿足後） |
| `EventStale` | 逾 `StaleAfter` 無觀測。`Since` 為最後觀測時間 |
| `EventStaleRecover` | 資料恢復。`Since` 報還原後狀態的起點 |
| `EventVanish` | 逾 `VanishAfter` 清狀態。**僅原本 Firing 的 key 才發**，語意含 resolved |

未告警的 key 消失一律靜默，不發任何事件。

---

## API

各方法的簽章與完整語意以 godoc 為單一真相源：
**[pkg.go.dev/github.com/waylen888/alarm](https://pkg.go.dev/github.com/waylen888/alarm)**。

---

## 條件目錄

| 條件 | 語意 | 所需筆數 |
| --- | --- | --- |
| `Threshold(judge)` | 最後觀測值達標即成立 | 1 |
| `ConsecutiveN(n, judge)` | 最近連續 n 筆皆達標；不足 n 筆不成立 | n |
| `AnyN(n, judge)` | 最近 n 筆任一達標；不足 n 筆不成立 | n |
| `ConsecutiveDeltaN(n, judge)` | 最近 n 組相鄰差分皆達標（counter 每輪增量） | **n+1** |
| `CountInWindow(n, window)` | window 內出現 >= n 次（log 頻率告警） | n，並宣告時間跨度 |
| `RateInWindow(window, judge)` | window 內 counter 每秒增量達標；速率以視窗內首尾觀測的實際時距計算 | 視窗內需 2 筆以上，容量以 `DefaultMinPoints`（64）起算並依跨度擴容 |
| `All(cs...)` / `Any(cs...)` | 組合 | 取子條件最大值 |

閾值語意一律以 closure 帶入，判斷邏輯維持單一真相源：

```go
alarm.Threshold(func(v float64) bool { return v > limit })
```

`ConsecutiveDeltaN` 的差分是直接相減、**不處理 counter reset**，
reset 產生的負差分交由 judge 函式自行判定。
（`RateInWindow` 用的 `Window.Delta` 則會把數值變小視為 reset 歸零重計。）

### 自訂條件

`Condition` 就是本 package 的擴充點。在自己的 package 實作它，引擎待之與內建條件完全相同：

```go
type Condition interface{ Breach(w Window) bool }
```

另有三個選配介面，實作用得上的即可，引擎會據此配置視窗與回報量測值：

| 介面 | 方法 | 告訴引擎 | 未實作時的預設 |
| --- | --- | --- | --- |
| `PointsHinter` | `MinPoints() int` | 判斷所需的最少觀測筆數 | `DefaultMinPoints`（64） |
| `SpanHinter` | `MinSpan() time.Duration` | 判斷涵蓋的時間跨度，視窗會為此擴容 | 無跨度 |
| `Measurer` | `Measure(w Window) float64` | 要填進 `Event.Value` 的量測值 | 最後觀測值 |

有沒有宣告差很多：一個要看 200 筆樣本卻什麼都沒宣告的條件，只會拿到
`DefaultMinPoints`（64）筆的視窗，永遠不可能成立；時間型條件不宣告跨度，
視窗一滿就會被筆數上限靜默截斷。

```go
// 最近 n 筆的平均值超過 limit 即成立。
type meanOver struct {
    n     int
    limit float64
}

func (c meanOver) Breach(w alarm.Window) bool { return c.mean(w) > c.limit }

func (c meanOver) MinPoints() int { return c.n }

func (c meanOver) Measure(w alarm.Window) float64 { return c.mean(w) } // 回報平均值，而非最後一筆

func (c meanOver) mean(w alarm.Window) float64 {
    pts := w.LastN(c.n)
    if len(pts) < c.n {
        return 0
    }
    var sum float64
    for _, p := range pts {
        sum += p.Value
    }
    return sum / float64(len(pts))
}
```

條件實作**必須無狀態**——狀態由引擎持有，規則熱更新才能安全替換條件。

---

## 統計製程管制：spc 子套件

```go
import "github.com/waylen888/alarm/spc"
```

對於日內節奏強烈的指標，閾值本來就是錯的工具。交易系統在 09:00 出現十倍尖峰是開盤，
同樣的值出現在 11:00 則是事故；固定上限除了被調校的那個時段以外，每個時段都是錯的。
`spc` 提供管制圖條件，問的是另一個問題——這筆樣本和這個指標自己近期的行為一致嗎？

但要清楚這換到的是什麼。滾動基線偵測的是**變化**，不是**水位**，所以不需要逐時段調參，
也能用在沒人寫下正常範圍的指標上。可是開盤本身也是一種變化，這些條件一樣會回報它。
消失的是「必須事先知道水位」，沒有消失的是「必須靜音掉你預期中的變化」。

兩個條件，都是普通的 `alarm.Condition`：

```go
spc.Nelson(spc.TrailingRobust(30), 30, spc.Rule2) // 連續九點落在中心線同一側
spc.EWMA(spc.Trailing(50), 50, 0.2, 3)            // 幅度小但持續的偏移
```

八條 [Nelson 規則](https://en.wikipedia.org/wiki/Nelson_rules)（Nelson, 1984）涵蓋劇烈偏移、
持續偏移、趨勢、過度調整與混合分佈。EWMA 負責的是規則 1 永遠看不到、規則 2 要晚九筆樣本
才看得到的小幅持續偏移。中心線與離散度由 `Baseline` 提供：`Fixed` 用於正常範圍已知的指標，
`Trailing` 取前段觀測的平均與標準差，`TrailingRobust` 取中位數與 MAD，
適用於參考期本身可能含有尖峰的情況。

在採用之前值得先知道：

- **搭配滾動基線的告警大約只持續 `ref` 筆觀測。** 持續偏移會隨著滾動進入自己的參考期，
  等它完全佔據參考期，管制圖就讀成「在管制內」，而指標仍然偏移著——也就是說告警會在
  事故進行到一半時自行解除，還附上一個接近零的 sigma 距離當證據。請把 `ClearFor`
  設得至少和你預期的事故長度一樣久，也不要拿 `Event.Value` 去套恢復通知的文案。
  `Fixed` 沒有這個行為。
- **明確指名要用哪幾條規則。** 不指名等於八條全開，在受控製程上大約每 31 筆就誤報一次。
  規則 7 是「基線該重估了」的訊號，不該放進會呼叫人的規則裡。
- **受測點永遠不會進入自己的基線。** `Baseline` 介面只拿得到參考觀測，
  就單次評估而言，這個排除是結構上的。
- **這些是點數，不是時間長度。** 沒有任何地方讀時間戳，所以 `ref` 的意思是「前面 50 筆觀測」。
  `For` 必須小於 `ref × 取樣間隔`，否則規則永遠不會觸發；`KeepWindowOnStale` 請維持 false。
- **`MinPoints` 要把兩段都算進去。** 規則 7 搭配 `Trailing(50)` 基線會讀 65 筆觀測，
  就宣告 65。子套件已經處理好；自己手寫而漏掉這件事的條件，會安靜地永遠不成立。
- **指標必須是有雜訊的。** 基線估不出離散度時，條件會安靜地永遠不成立——
  固定在零的佇列長度會讓 `Trailing` 失效，大多為零的整數計數器會讓 `TrailingRobust` 失效。
  那些情況請用閾值。

統計層——`Mean`、`StdDev`、`Median`、`MAD`、各個 baseline、`Check`、`EWMAStat`——
只處理 `[]float64`，不 import `alarm`，可以單獨使用。與母套件一樣是零依賴。

CUSUM 與季節性基線都刻意不在這裡；理由與實測的誤報率見
[套件文件](https://pkg.go.dev/github.com/waylen888/alarm/spc)。

---

## Escalate 與 Exit

這一節值得細讀。多級告警有一個容易漏掉、又很難查的失效模式，這兩個守門條件就是為它而生。

### 問題

`Levels` 由高至低評估、取最高成立等級。這對**初次分級**是正確的規則，
對把告警**降級**卻是錯的——因為在幾乎所有真實規則集裡，
**較低等級的條件會被滿足較高等級的那些樣本一併涵蓋**。

具體來說。線路監控把每次探測分成 `0`（正常）、`1`（劣化）、`2`（斷線），兩個等級：

```go
warn:  ConsecutiveN(2, func(v float64) bool { return v >= 1 })
error: ConsecutiveN(2, func(v float64) bool { return v >= 2 })
```

凡滿足 `error`（`v >= 2`）的樣本也必然滿足 `warn`（`v >= 1`）。
看一條已經斷線、正開始恢復的線路：

| 樣本 | 視窗（最近 2 筆） | `error` 成立？ | `warn` 成立？ | 最高成立等級 |
| --- | --- | --- | --- | --- |
| `2` | `[2]` | 否（不足 2 筆） | 否 | — |
| `2` | `[2, 2]` | **是** | 是 | error → **Fire（error）** |
| `1` | `[2, 1]` | 否 | **是** | warn → **Deescalate 到 warn** |

最後一列就是 bug。一筆劣化樣本就把告警從 error 降到 warn——
並不是因為線路恢復了，而是因為**前一筆仍在斷線的樣本本身也 `>= 1`**，
它幫忙湊滿了 warn 的條件。只要探測稍微抖一下，告警就會在 error 與 warn 之間來回，每次都通知。

直覺的修法都更糟。把等級改成互斥（`warn` ＝ `v >= 1 && v < 2`）會破壞初次分級——
一條真的斷線、但目前只累積到一筆 `2` 的線路變成兩個等級都不成立。
把 `warn` 的 `ConsecutiveN` 調大，則是把 warn 告警本身鈍化。
這個不對稱是真實存在的：**進入一個等級的條件，與離開它的條件，是兩個不同的條件**，
把兩者混為一談的規則集必然出錯。

### Exit

`Exit` 是降級守門條件，宣告在**要離開的那個等級**上。
Firing 於該等級時，較低等級成立不再足以降級，必須 `Exit` 同時成立：

```go
{
    Severity:  alarm.SeverityError,
    Condition: alarm.ConsecutiveN(2, func(v float64) bool { return v >= 2 }),
    Exit:      alarm.ConsecutiveN(2, func(v float64) bool { return v < 2 }),
},
{
    Severity:  alarm.SeverityWarn,
    Condition: alarm.ConsecutiveN(2, func(v float64) bool { return v >= 1 }),
},
```

同一串樣本重播一次：

| 樣本 | 視窗（最近 2 筆） | 最高成立等級 | error 的 `Exit` 成立？ | 結果 |
| --- | --- | --- | --- | --- |
| `2` | `[2]` | — | — | — |
| `2` | `[2, 2]` | error | — | **Fire（error）** |
| `1` | `[2, 1]` | warn | **否**（`2` 不 `< 2`） | 維持 error，不發事件 |
| `1` | `[1, 1]` | warn | **是** | **Deescalate 到 warn** |

現在告警是依據「較高等級確實已經結束」的證據降級，而不是條件重疊產生的假象。
這正是 `ExampleLevel_exitGuard` 示範的情境。

### Escalate

`Escalate` 是對稱的守門條件，宣告在**要進入的那個等級**上。
Firing 於較低等級時，本等級的 `Condition` 成立不再足以升級，必須 `Escalate` 同時成立。

它存在的理由是：「初次分級」與「把既有告警升級」同樣想要不同的條件。
典型的線路規則是連續 n 筆異常才告警、並以**當前樣本**定級，
所以劣化樣本之中出現一筆 `2` 就足以直接開出 error 等級的告警。
但要把一則已經是 warn 的告警升級，理應要求更多證據：連續 n 筆 error 等級樣本，
且任何較低的樣本都會重置連續計數。`Condition` 表達前者，`Escalate` 表達後者：

```go
{
    Severity:  alarm.SeverityError,
    Condition: alarm.Threshold(func(v float64) bool { return v >= 2 }),
    Escalate:  alarm.ConsecutiveN(3, func(v float64) bool { return v >= 2 }),
}
```

`Escalate` **只作用於 Firing 中的升級路徑**。初次分級（`OK → Firing`）與 `Pending` 不受此限
——這正是把兩者拆開的價值所在，而不只是單純把條件變嚴。

兩個欄位都是選填；`nil` 維持預設行為，也就是最高成立等級直接勝出。

---

## 熱更新與 Fingerprint

`SetRules` 以 **ID 比對**：消失的規則靜默清除狀態（沿用「規則被刪／停用後不發 resolved」慣例），
同 ID 規則保留各 key 狀態。兩種情況會整條重建：

1. 條件所需**視窗容量變大**——筆數不足的舊視窗永遠無法滿足新的 `ConsecutiveN`。
2. **`Fingerprint` 變更**。

重建時只作廢「條件語意狀態」（視窗／Pending／Firing），各 key 的**資料存在性**
（最後觀測時間、Stale 追蹤）會被搬進新 runtime——否則門檻編輯會讓進行中的資料中斷
從 no_data 靜默變 normal，而斷線期間沒有觀測、key 無法由 `Observe` 重建，
中斷將永遠漏偵測。

時間跨度變大**刻意不重建**：跨度外的歷史觀測本來就沒保留，重建拿不回資料，
只會多清掉 Firing 狀態（告警靜默消失）。沿用舊視窗僅是暫時低估，
後續觀測會讓它自動擴容自癒，與規則冷啟動時視窗未滿是同一件事。

### 什麼時候需要 Fingerprint

引擎看不進 `Condition` closure，也看不出觀測值的編碼方式。以下情況**必須**把語意輸入編進指紋：

- **觀測值的編碼依設定而變**。例：在觀測前就依門檻把樣本編成 0/1/2，門檻一改，
  視窗裡的舊編碼就失效，不得與新編碼混評（舊設定的 2 筆 warn ＋ 新設定 1 筆
  會誤湊滿 `ConsecutiveN`）。
- **什麼值會被觀測進來依設定而變**。例：syslog 的查詢內容決定「什麼算一筆命中」，
  查詢一改，歷史觀測的意義就不同了。
- **`For > 0` 且判定門檻可變**。pending 起算點是「條件自何時起持續成立」的既成狀態，
  **不由視窗重算**——換上新條件後沿用舊起算點，形同讓新條件借用舊條件的成立時間
  （門檻由 `>10` 改成 `>100`，已 pending 四分鐘的 key 只要再撐一分鐘就湊滿五分鐘的 `For`）。
  凡影響判定結果的設定（運算子、閾值…）皆須編入。

**不需要**指紋的情況：觀測值是原始量測、判斷邏輯全在 closure 內，且 `For <= 0`。
`SetRules` 會換上新條件，視窗裡的歷史原始值自然被新條件重判。

`For` 本身變更**不入**指紋：「條件已持續 X」與 `For` 取值無關，沿用既有起算點
重新比較新門檻即為正解（縮短立即觸發、延長則續等），重建反而會白清掉進行中的告警。

指紋必須是設定的**確定性序列化**（map 需排序）。

```go
// 頻率型規則：次數、視窗、查詢任一變更即語意變更
func conditionFingerprint(cfg ObserverConfig) string {
    q, _ := json.Marshal(cfg.Query)
    return fmt.Sprintf("%d|%s|%s", cfg.Times, cfg.Window, q)
}

// 觀測值為原始量測的指標型規則：只涵蓋 judge 的輸入
func (r MetricAlarm) ConditionFingerprint() string {
    return string(r.Op) + "|" + strconv.FormatFloat(r.Value, 'g', -1, 64)
}
```

---

## 資料存在性：Stale / Vanish / MaxKeys

「條件語意」與「資料存在性」是兩個正交維度，引擎分開處理。

| 設定 | 語意 |
| --- | --- |
| `StaleAfter: -1` | 關閉資料中斷偵測。收集停止即狀態凍結（適合「不收集是常態或本意」的場景） |
| `StaleAfter: d` | 逾 d 無觀測轉 Stale，發 `EventStale`；資料恢復發 `EventStaleRecover` |
| `VanishAfter: -1` | 關閉消失清理。**注意狀態會累積**，僅適用 key 集合有界的場景 |
| `VanishAfter: d` | 逾 d 無觀測清狀態；原本 Firing 才發 `EventVanish` |
| `MaxKeys: n` | cardinality 上限，超出時新 key 被拒收並 log 一次 |
| `KeepWindowOnStale` | 進入 Stale 時保留視窗 |

進入 Stale 時預設**清空視窗**：資料中斷即連續性中斷，次數型條件（`ConsecutiveN` 等）
不得跨斷線期計數。時間視窗條件（`CountInWindow` 等）靠時間自然滑出，
不適用此語意時設 `KeepWindowOnStale: true`。

`For`／`ClearFor` 的持續時間同屬連續性，也不跨斷線累計：轉 Stale 時未成立的
Pending 降回 OK，恢復後重新累計；Firing 保留，但解除中的計時歸零。

以「逐輪出現與否」管理實體的呼叫端，可用 `Forget` 立即清除消失的 key，
不必等 `VanishAfter` 的時間制寬限。

---

## Event.Meta：事件自帶素材

需要在 handler 組訊息的素材（log 原文、探測封包、觀測當下的設定…），
用 `ObserveMeta` 綁在觀測上，事件會原樣帶回。

**不要在引擎旁另外維護 `key → 素材` 的邊表**——那張表的生命週期與引擎 key 對不齊，
`MaxKeys` 拒收時會殘留，vanish 時會漏清。

素材於 `ObserveMeta` 持鎖期間綁定，排隊中的事件自包含：`ObserveMeta` 返回後立即
`Forget` 或覆寫素材，都不影響已入列事件的內容。

素材生命週期＝key 生命週期（`VanishAfter` 關閉時可長期保留），
因此**只存 handler 需要的最小不可變依賴**，不要抓大型物件圖（整個 session／connection），
否則引擎狀態會釘住其回收。

`Observe`／`Touch` 不更動既有素材，只有 `ObserveMeta`／`ObserveEventMeta` 會更新。

---

## 併發與生命週期

**這段是使用引擎時最容易踩的地方，請完整讀過。**

1. **同步且保序**。引擎在狀態鎖外、送達鎖內呼叫 handler，事件依轉移順序串行送達，
   且 `Observe`／`Tick` **返回時其事件已送達完畢**。呼叫端可以安全假設
   handler 的投影（dashboard 鏡像等）已更新，再繼續往下做。

2. **handler 內只能呼叫讀取類 API**（`State`／`Has`／`Snapshot`）。
   產生事件的方法（`Observe*`／`Touch`／`Tick`／`Run`）與規則變更
   （`SetRules`／`Forget`／`ForgetRule`）都與送達同鎖序列化，從 handler 呼叫會**自我死鎖**。

3. **handler 會阻塞同一引擎的所有觀測路徑**。落庫、發通知這類長時間操作，
   請**自行以保序佇列非同步化**：單一 worker 依序消化 channel 是常見寫法，
   兼顧不阻塞與保序。把每個事件各丟一個 goroutine 是不行的——那等於默默丟掉
   引擎費了不少力氣提供的保序保證。

4. **需要「轉移當下」素材的一律用 `ObserveMeta` 附帶**，不要在 handler 讀共享可變狀態。
   即使持有送達鎖，`Tick` 促發的事件（`Reminder`／`Stale`／`Vanish`）送達時，
   仍可能與**其他 goroutine**對那份共享狀態的寫入交錯，handler 讀到的東西可能已經變了。
   `Event.Meta` 是唯一能拿到「轉移當下」世界樣貌的管道。

`Observe`、`ObserveEvent`、`Touch` 可由任意 goroutine 呼叫，`Tick` 與它們併發亦安全。

---

## 視窗容量與上限

- 初始容量由條件宣告的 `minPoints` 決定，下限 8、上限 `MaxWindowPoints`（4096）。
- 宣告了 `minSpan` 的時間視窗條件：視窗滿而最舊觀測仍落在跨度內時**自動倍增擴容**
  （至上限），條件不需預估取樣頻率，時間視窗也不會被筆數上限靜默截斷。
- 條件所需筆數超過 `MaxWindowPoints` 即**永遠不可能成立**，`SetRules` 會 log 警告。

使用者可設次數的呼叫端，請在建規則時夾限，讓超界設定近似觸發而非靜默失效：

```go
alarm.ClampPoints(n)      // 連續／任一 N 筆型條件
alarm.ClampDeltaPoints(n) // 差分型條件（要 N 組差分就需要 N+1 筆，故上限少一）
```

新的使用者輸入仍應在 API 層以 `MaxWindowPoints` 驗證後才落地。

---

## 生產環境的使用場景

這具引擎在生產環境驅動過六套性質相異的告警子系統，合計八種規則形態。
下表的價值在於「工作負載形態 → 所需引擎特性」的對應。

| 場景 | 條件 | 使用到的引擎特性 |
| --- | --- | --- |
| syslog 頻率告警 | `CountInWindow(times, window)` | 時間視窗；`Reminder` 每視窗補發一次；查詢／次數／視窗入指紋 |
| 多級嚴重度的線路監控 | `ConsecutiveN` ＋ `Threshold` 組合 | 多級告警；`Escalate`／`Exit` 守門；`Clear` 次數制恢復；門檻入指紋 |
| 連線數監控 | `ConsecutiveN(threshold, judge)` | 取不到值時不觀測、狀態凍結而非誤告警 |
| 通用指標規則告警 | `Threshold(judge)` ＋ `For` | 單規則多 series；`VanishAfter` 寬限；`MaxKeys`；`Touch` 處理 counter 首輪 |
| 每主機系統指標監控 | `ConsecutiveN` / `ConsecutiveDeltaN` | gauge 與 counter 兩種語意並存；`ClampPoints`／`ClampDeltaPoints` |
| 每程序存活監控（約 10 萬 key） | `Threshold(v >= 1)` | package 級引擎，狀態跨連線重建存活；十萬級 `MaxKeys`；`Forget` 立即清除 |
| ICMP／agent 可達性監控 | `Threshold(v >= 1)` ＋ `For`（＝逾時） | 事件驅動；`ObserveMeta` 帶探測素材；`Run` 自行 Tick |
| 閾值判斷 routine | `AnyN(threshold, judge)` | 規則常駐、觀測會跳過某些輪次；跳過的輪次不得清空視窗 |

---

## 限制

- **一個 key 只承載一條純量序列**。多維度指標需由呼叫端編碼成單一數值，或拆成多條規則。
- **沒有跨規則靜默**。`Reminder` 與 `ClearFor` 只作用於單一 rule+key。
  跨規則、或連初次告警都要吞的靜默窗，屬於呼叫端的責任。
- **不做通知投遞**。引擎只發事件；路由、排版、聚合、收件人解析都由呼叫端負責。
- **不做持久化**。狀態在記憶體裡，不跨重啟。`Snapshot` 是為了讓你在重啟後對齊外部投影，
  但引擎本身是冷啟動——重啟前處於 Firing 的 key，要等條件重新成立才會再次告警。
- **沒有內建統計型條件**。內建條件都是閾值型與次數型，更精細的判斷請自行實作 `Condition`。

---

## 設計不變量

改動本 package 前請確認這些性質仍然成立：

1. **送達即完成**：`Observe`／`Tick` 返回時，其產生的事件已全部送達 handler。
2. **轉移順序＝送達順序**：由送達鎖罩住「狀態轉移＋事件送達」全程保證。
   鎖序固定為「送達鎖 → 狀態鎖」，handler 在送達鎖內、狀態鎖外執行。
3. **評估時鐘單調**：遲到的舊觀測只「插入資料」，不「倒轉時間」。
   以歷史時間評估會讓視窗上界排除較新的真實命中，造成假 resolve／refire 震盪。
4. **視窗保時間序**：觀測在唯一寫入點依時間戳插入，亂序輸入不會湊出假視窗；視窗計數有上界。
5. **條件無狀態**：所有狀態由引擎持有，條件才能安全熱替換。
6. **未告警即靜默**：未曾 Firing 的 key，無論消失或規則被刪都不發事件。
7. **視窗的時間上界是不對稱的**：`Points` 與 `Count` 會排除「評估當下」之後的觀測，
   `Last` 與 `LastN` 則不會，一律回傳手上最新的點。目前沒有任何路徑會以過去的時間評估
   ——這正是由不變量 3 保證的——所以透過引擎觀察不到這個差異。但透過 `Condition` 觀察得到，
   而它是公開的擴充點：條件不得假設 `Last`／`LastN` 與 `Points`／`Count` 有相同的上界。
   要嘛在這裡把兩邊都補上界，要嘛維持不變量 3 不被破壞。
8. **視窗容量只增不減**：重新裝上所需筆數較少的規則時，既有視窗仍保留較大的容量。
   這是刻意的：視窗過大只是多花記憶體，視窗過小則會讓條件永遠不可能成立。
9. **零依賴**：只 import 標準函式庫。

---

## 授權

MIT，見 [LICENSE](LICENSE)。
