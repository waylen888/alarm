package alarm

import (
	"context"
	"sort"
	"sync"
	"time"
)

const (
	// defaultMaxKeys 每條規則預設可追蹤的 key 上限。1000 對「每台主機/
	// 每條 series 一個 key」這類有界基數綽綽有餘,同時讓標籤爆炸(把
	// 高基數欄位誤當 key)撞上界限並留下一行 log,而不是靜靜吃光記憶體。
	// key 集合本就更大的規則請逐條設 Rule.MaxKeys。
	defaultMaxKeys = 1000
	defaultVanish  = time.Hour
	minRingCap     = 8
	maxRingCap     = MaxWindowPoints
)

// MaxWindowPoints is the hard cap on how many observations one key's window
// can retain. A condition needing more points than this (the n of
// CountInWindow, say) can never be satisfied; validate user input against
// this constant in your configuration layer, since all the engine can do is
// log a warning from SetRules.
const MaxWindowPoints = 4096

// ClampPoints clamps the N of a "consecutive/any N samples" condition to the
// window cap. An N above MaxWindowPoints can never be satisfied, because the
// window cannot hold that many points. Where N is user-configurable,
// approximating at the cap beats never firing: ConsecutiveN then fires
// earlier than configured and AnyN is slightly stricter, but the alert does
// not fail silently. New input should still be validated against
// MaxWindowPoints at the API layer.
func ClampPoints(n int) int { return min(n, MaxWindowPoints) }

// ClampDeltaPoints clamps the N of a "consecutive N adjacent deltas"
// condition (ConsecutiveDeltaN). N deltas need N+1 observations, so applying
// ClampPoints directly would leave an N=MaxWindowPoints rule needing
// MaxWindowPoints+1 points — a window that can never fill, which is exactly
// the silent failure ClampPoints exists to prevent. Hence one less.
func ClampDeltaPoints(n int) int { return min(n, MaxWindowPoints-1) }

// Engine is the alerting state machine. Create one instance per subsystem,
// each with its own Handler. Observe/ObserveEvent may be called from any
// goroutine; Tick is driven either by the caller's existing loop or by Run.
// Delivery is synchronous and ordered — see Handler for the full contract.
type Engine struct {
	// emitMu 送達鎖:罩住「狀態轉移+事件送達」全程,保證轉移順序=
	// 送達順序,且 Observe/Tick 返回時其事件已送達完畢——呼叫端得以
	// 假設 handler 的投影(鏡像等)已完成再繼續。只以狀態鎖序列化
	// 轉移、解鎖後才送達的做法,會讓併發呼叫端(如 Tick 轉 Stale vs
	// Observe 轉 StaleRecover)後發生的轉移先送達。
	// 鎖序 emitMu→mu;handler 於 emitMu 內、mu 外執行。
	emitMu  sync.Mutex
	mu      sync.Mutex
	handler Handler
	rules   map[string]*ruleRuntime

	clock      func() time.Time
	defStale   time.Duration // 0 = 關閉
	defVanish  time.Duration
	defMaxKeys int
	logf       func(format string, args ...any)
}

// Option configures an Engine at construction time.
type Option func(*Engine)

// WithDefaultStale sets the engine-wide default used when a Rule leaves
// StaleAfter unset (0 disables stale detection).
func WithDefaultStale(d time.Duration) Option {
	return func(e *Engine) { e.defStale = d }
}

// WithDefaultVanish sets the engine-wide default used when a Rule leaves
// VanishAfter unset. The built-in default is one hour.
func WithDefaultVanish(d time.Duration) Option {
	return func(e *Engine) { e.defVanish = d }
}

// WithDefaultMaxKeys sets the engine-wide default used when a Rule leaves
// MaxKeys unset. The built-in default is 1000.
func WithDefaultMaxKeys(n int) Option {
	return func(e *Engine) { e.defMaxKeys = n }
}

// WithClock replaces the engine's clock. It is used by Run and Snapshot, and
// exists so tests can drive time deterministically.
func WithClock(now func() time.Time) Option {
	return func(e *Engine) { e.clock = now }
}

// WithLogf injects a logging function. The engine depends on no logging
// package; without this option its diagnostics are discarded.
func WithLogf(logf func(format string, args ...any)) Option {
	return func(e *Engine) { e.logf = logf }
}

// New creates an Engine that delivers events to h. A nil handler is allowed:
// the engine still tracks state, it just emits nothing.
func New(h Handler, opts ...Option) *Engine {
	e := &Engine{
		handler:    h,
		rules:      map[string]*ruleRuntime{},
		clock:      time.Now,
		defVanish:  defaultVanish,
		defMaxKeys: defaultMaxKeys,
		logf:       func(string, ...any) {},
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

type ruleRuntime struct {
	rule       Rule
	keys       map[string]*keyState
	ringCap    int           // 初始視窗容量(筆數制,由條件 MinPoints 決定)
	needPoints int           // 條件宣告所需筆數(未夾限),> maxRingCap 代表條件不可能成立
	span       time.Duration // 條件所需時間跨度(0=無時間視窗條件),視窗依此動態擴容
	overCap    bool          // MaxKeys 已滿,只 log 一次
	overSpan   bool          // 視窗達 maxRingCap 仍不足跨度,只 log 一次
}

type keyState struct {
	ring     *ring
	state    State
	severity Severity

	// Stale 前的狀態,資料恢復時還原
	prevState    State
	prevSeverity Severity

	since        time.Time // 進入現行狀態的時間
	pendingSince time.Time // 首次達標時間(Pending 起點,Fire 事件的 Since)
	clearSince   time.Time // Firing 中條件首次解除時間(zero=未在解除中)
	lastObserve  time.Time
	lastNotify   time.Time // 最後一次 Fire/Escalate/Reminder,用於補發節流
	meta         any       // 最近一次 ObserveMeta 附帶的素材,事件以 Event.Meta 帶回

	// awaitValue Touch 聲明「本輪無有效值」後為真,下一筆真觀測清除:
	// 期間 Tick 不得用視窗裡的舊觀測促發條件或補發 reminder(如
	// counter reset 期間的 Pending 被舊 rate 促發成假告警);
	// stale/vanish 屬存在性偵測,不受影響。
	awaitValue bool
}

func newRuleRuntime(r Rule) *ruleRuntime {
	// Levels 由高至低排序,評估時取最高成立等級。r 是 Rule 的複本,但
	// r.Levels 只複製了 slice header,底層陣列仍與呼叫端共用——就地排序
	// 會把呼叫端自己的 []Level 重排。先複製再排序,newRuleRuntime 因此
	// 不會寫穿到呼叫端持有的記憶體。
	levels := make([]Level, len(r.Levels))
	copy(levels, r.Levels)
	r.Levels = levels
	sort.SliceStable(r.Levels, func(i, j int) bool {
		return r.Levels[i].Severity > r.Levels[j].Severity
	})
	ringCap := minRingCap
	var span time.Duration
	conds := make([]Condition, 0, len(r.Levels)+1)
	for _, lv := range r.Levels {
		conds = append(conds, lv.Condition)
		if lv.Exit != nil {
			conds = append(conds, lv.Exit)
		}
		if lv.Escalate != nil {
			conds = append(conds, lv.Escalate)
		}
	}
	if r.Clear != nil {
		conds = append(conds, r.Clear)
	}
	for _, c := range conds {
		if n := minPointsOf(c); n > ringCap {
			ringCap = n
		}
		if d := minSpanOf(c); d > span {
			span = d
		}
	}
	needPoints := ringCap
	if ringCap > maxRingCap {
		ringCap = maxRingCap
	}
	return &ruleRuntime{rule: r, keys: map[string]*keyState{}, ringCap: ringCap, needPoints: needPoints, span: span}
}

func (rr *ruleRuntime) staleAfter(e *Engine) time.Duration {
	if rr.rule.StaleAfter != 0 {
		return max(rr.rule.StaleAfter, 0) // <0 關閉
	}
	return e.defStale
}

func (rr *ruleRuntime) vanishAfter(e *Engine) time.Duration {
	if rr.rule.VanishAfter != 0 {
		return max(rr.rule.VanishAfter, 0)
	}
	return e.defVanish
}

func (rr *ruleRuntime) maxKeys(e *Engine) int {
	if rr.rule.MaxKeys > 0 {
		return rr.rule.MaxKeys
	}
	return e.defMaxKeys
}

// SetRules hot-reloads the rule set. Rules are matched by ID; a rule that is
// no longer present has its state cleared silently, following the convention
// that deleting or disabling a rule does not emit a resolved event. A rule
// that keeps its ID keeps its per-key state, except in two cases where the
// whole rule is rebuilt: when the conditions now need a larger window (an old
// window holding too few points could never satisfy the new ConsecutiveN and
// friends), and when Fingerprint changed (the meaning of the observed values
// changed, so old observations must not count towards the new condition — see
// Rule.Fingerprint). A rebuild invalidates only condition-semantic state; each
// key's data availability (last observation time, Stale tracking) is carried
// into the new runtime.
//
// SetRules is serialised with event delivery. A rule change landing in the
// window between "a transition produced events" and "the handler finished
// running" would let in-flight events project rule state that has already
// been torn down or rebuilt, producing false alerts or a projection stuck
// firing with nobody left to clear it. By the time SetRules returns, delivery
// in flight has completed, so whatever the caller does next to realign its
// projection is guaranteed to see the final state. Forget and ForgetRule
// behave the same way.
//
// A rule is deliberately NOT rebuilt when only the required time span grows:
// observations older than the span were never retained, so a rebuild cannot
// recover them and would only additionally discard Firing state, making an
// alert disappear silently. Reusing the old window merely underestimates for
// a while and heals itself as later observations grow it — the same situation
// as a rule whose window has not filled yet after a cold start.
func (e *Engine) SetRules(rules []Rule) {
	e.emitMu.Lock()
	defer e.emitMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	next := make(map[string]*ruleRuntime, len(rules))
	for _, r := range rules {
		if len(r.Levels) == 0 {
			e.logf("[alarm] rule %s has no levels, skipped", r.ID)
			continue
		}
		fresh := newRuleRuntime(r)
		if fresh.needPoints > maxRingCap {
			e.logf("[alarm] rule %s condition needs %d points, exceeds window cap %d and can never be satisfied", r.ID, fresh.needPoints, maxRingCap)
		}
		if old, ok := e.rules[r.ID]; ok {
			if old.ringCap >= fresh.ringCap && old.rule.Fingerprint == r.Fingerprint {
				old.rule = fresh.rule
				old.span = fresh.span
				old.overCap = false
				old.overSpan = false
				next[r.ID] = old
				continue
			}
			carryAvailabilityLocked(old, fresh)
		}
		next[r.ID] = fresh
	}
	e.rules = next
}

// carryAvailabilityLocked 整條重建(容量增/指紋變)時,把各 key 的
// 「資料存在性」搬進新 runtime:條件語意狀態(視窗/Pending/Firing)
// 隨語意作廢,但 lastObserve 與 Stale 追蹤不屬條件語意——門檻編輯
// 不得讓進行中的資料中斷從 no_data 靜默變 normal,且斷線期間無觀測、
// key 無法由 Observe 重建,中斷將永遠漏偵測。Stale 的 prevState 降為
// OK(舊條件的告警不跨語意保留,與靜默清除慣例一致):資料恢復時
// 靜默 recover,新條件從零累積;原 Firing 鍵成為帶 lastObserve 的
// OK 鍵,斷線中仍會正常轉 Stale。呼叫端持 mu。
func carryAvailabilityLocked(old, fresh *ruleRuntime) {
	for key, ks := range old.keys {
		nk := &keyState{
			ring:        newRing(fresh.ringCap),
			since:       ks.since,
			lastObserve: ks.lastObserve,
			meta:        ks.meta,
			awaitValue:  ks.awaitValue,
		}
		if ks.state == StateStale {
			nk.state = StateStale
			nk.prevState = StateOK
		}
		fresh.keys[key] = nk
	}
}

// Observe feeds one observation into the engine. An unknown ruleID is
// ignored silently, since racing with SetRules is normal. Observe leaves any
// existing Event.Meta payload untouched; only ObserveMeta updates it.
func (e *Engine) Observe(ruleID, key string, v float64, at time.Time) {
	e.observe(ruleID, key, v, at, false, nil)
}

// ObserveMeta is Observe with a payload attached. The payload is stored on
// the key and handed back verbatim as Event.Meta on every subsequent event
// for that key (Fire, Resolve, Reminder, Stale, Vanish and the rest). It is
// bound while this call holds the lock, so queued events are self-contained:
// calling Forget or overwriting the payload right after ObserveMeta returns
// does not change events already queued.
func (e *Engine) ObserveMeta(ruleID, key string, v float64, at time.Time, meta any) {
	e.observe(ruleID, key, v, at, true, meta)
}

func (e *Engine) observe(ruleID, key string, v float64, at time.Time, setMeta bool, meta any) {
	e.emitMu.Lock()
	defer e.emitMu.Unlock()
	e.mu.Lock()
	rr, ok := e.rules[ruleID]
	if !ok {
		e.mu.Unlock()
		return
	}
	ks, ok := rr.keys[key]
	if !ok {
		if len(rr.keys) >= rr.maxKeys(e) {
			if !rr.overCap {
				rr.overCap = true
				e.logf("[alarm] rule %s exceeds max keys %d, new key %q dropped", ruleID, rr.maxKeys(e), key)
			}
			e.mu.Unlock()
			return
		}
		ks = &keyState{ring: newRing(rr.ringCap), since: at}
		rr.keys[key] = ks
	}
	// 評估時鐘單調:遲到舊觀測只「插入資料」,不得「倒轉時間」——
	// 以歷史時間評估時,視窗上界會排除較新的真實命中,Firing 的頻率
	// 條件被誤判解除、下一次 Tick 又重新觸發,產生假 resolve/refire
	// 震盪。資料仍以真實時戳入環(保序插入),評估一律站在該 key
	// 已知的最新時刻;單調餵入時 evalAt==at,行為不變。
	evalAt := at
	if ks.lastObserve.After(evalAt) {
		evalAt = ks.lastObserve
	}

	evs := staleRecoverLocked(ruleID, key, ks, evalAt)
	if !ks.awaitValue && ks.ring.n > 0 && rr.span > 0 {
		// 預結算:插入新觀測前,先以本次觀測的時間推進舊狀態(等效
		// 「先 Tick(at) 再觀測」)。時間視窗條件的過期必須先於新觀測
		// 結算——新命中本身會讓條件重新成立,不先結算的話,視窗短於
		// Tick 間隔時,Firing 間隙中的孤立命中被靜默吞掉(單次模式該
		// 命中永遠不通知)。
		// 僅限時間視窗條件(span>0,成立性隨時間衰減):樣本型條件
		// (Threshold/ConsecutiveN)的「舊樣本仍成立」不是新資訊,
		// 預結算只會用過期樣本促發 For——DurationSec 邊界的正常樣本
		// 反而觸發假 Fire+Resolve,且破壞「逾時前靜默取消」語意。
		// 另兩種情況跳過:Touch 凍結中(awaitValue,舊值不得被時間
		// 促發)、視窗為空(零證據不得結算——stale 恢復當下視窗已清,
		// 跨斷線保留的 Firing 不得因此被誤 resolve)。
		// 預結算事件帶舊 meta(描述舊狀態的了結),語意正確。
		evs = append(evs, e.evalLocked(rr, ruleID, key, ks, evalAt)...)
	}

	if setMeta {
		ks.meta = meta
	}
	ks.awaitValue = false
	e.growForSpanLocked(rr, ruleID, key, ks, evalAt)
	ks.ring.push(Point{Time: at, Value: v})
	// freshness 取單調最大:亂序餵入(如 syslog 跨 stream 合併)的遲到
	// 舊樣本不得倒退 lastObserve,否則 Tick 的 idle 被灌水、誤發
	// stale/vanish。遲到樣本仍視同「資料在到達」,stale 恢復照常。
	if at.After(ks.lastObserve) {
		ks.lastObserve = at
	}

	evs = append(evs, e.evalLocked(rr, ruleID, key, ks, evalAt)...)
	e.mu.Unlock()
	e.emit(evs)
}

// growForSpanLocked 視窗滿、且將被覆寫的最舊觀測仍落在時間視窗條件所需
// 跨度內時,倍增容量(至 maxRingCap),讓時間視窗不因筆數上限被截斷;
// 條件因此不需預估取樣頻率。呼叫端持鎖。
func (e *Engine) growForSpanLocked(rr *ruleRuntime, ruleID, key string, ks *keyState, at time.Time) {
	if rr.span <= 0 || !ks.ring.full() {
		return
	}
	oldest, ok := ks.ring.oldest()
	if !ok || at.Sub(oldest.Time) > rr.span {
		return
	}
	if c := ks.ring.capacity(); c < maxRingCap {
		ks.ring.grow(min(c*2, maxRingCap))
	} else if !rr.overSpan {
		rr.overSpan = true
		e.logf("[alarm] rule %s key %q window span exceeds max capacity %d, oldest points dropped", ruleID, key, maxRingCap)
	}
}

// staleRecoverLocked 於收到存在訊號時還原 Stale 前狀態並產生恢復事件;呼叫端持鎖。
func staleRecoverLocked(ruleID, key string, ks *keyState, at time.Time) []Event {
	if ks.state != StateStale {
		return nil
	}
	ks.state = ks.prevState
	ks.severity = ks.prevSeverity
	// 先定新狀態起點再組事件:Since 契約=「本事件所報告狀態的起始
	// 時間」,恢復事件報告的是還原後的狀態,自恢復當下起算——與緊接
	// 的 Snapshot 一致(進 stale 時原狀態起點已被覆寫,無從回報更早)。
	ks.since = at
	ev := Event{
		RuleID: ruleID, Key: key, Kind: EventStaleRecover,
		Severity: ks.severity, State: ks.state, Since: ks.since, At: at, Meta: ks.meta,
	}
	return []Event{ev}
}

// Touch records that a key still exists at time at but has no evaluable
// value this round — a counter's first sample, or the round after a reset.
// It only advances the last-observation time to suppress Stale and Vanish; it
// does not push to the window and does not evaluate conditions.
//
// The freeze lasts until the next real observation: until then Tick must not
// use the stale observations still in the window to drive a condition, since
// those values are no longer trustworthy and a For timeout must not fire an
// alert based on them. A key that does not exist yet is created with empty
// state, subject to MaxKeys; a key in Stale is treated as data recovering.
func (e *Engine) Touch(ruleID, key string, at time.Time) {
	e.emitMu.Lock()
	defer e.emitMu.Unlock()
	e.mu.Lock()
	rr, ok := e.rules[ruleID]
	if !ok {
		e.mu.Unlock()
		return
	}
	ks, ok := rr.keys[key]
	if !ok {
		if len(rr.keys) >= rr.maxKeys(e) {
			if !rr.overCap {
				rr.overCap = true
				e.logf("[alarm] rule %s exceeds max keys %d, new key %q dropped", ruleID, rr.maxKeys(e), key)
			}
			e.mu.Unlock()
			return
		}
		ks = &keyState{ring: newRing(rr.ringCap), since: at}
		rr.keys[key] = ks
	}
	if at.After(ks.lastObserve) { // freshness 單調,理由見 observe
		ks.lastObserve = at
	}
	ks.awaitValue = true
	evs := staleRecoverLocked(ruleID, key, ks, at)
	e.mu.Unlock()
	e.emit(evs)
}

// Forget silently removes one key's state, emitting nothing regardless of
// what state it was in. It is for callers that track entities by whether they
// appeared in the current round and want a disappeared key cleared at once,
// without waiting out the VanishAfter grace period. Serialised with delivery
// (see SetRules).
func (e *Engine) Forget(ruleID, key string) {
	e.emitMu.Lock()
	defer e.emitMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	rr, ok := e.rules[ruleID]
	if !ok {
		return
	}
	if _, ok := rr.keys[key]; ok {
		delete(rr.keys, key)
		rr.overCap = false
	}
}

// Has reports whether the rule currently holds any state for key, whether
// merely observed or alerting. Event-driven callers use it to avoid creating
// state for healthy keys when only a key that has alerted needs a clearing
// observation.
func (e *Engine) Has(ruleID, key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	rr, ok := e.rules[ruleID]
	if !ok {
		return false
	}
	_, ok = rr.keys[key]
	return ok
}

// State returns the current state and severity of a rule/key pair; ok is
// false when either the rule or the key is unknown. A handler may call it
// from inside an event callback to check a sibling rule's live state — say
// whether the other dimension of the same link has also gone Stale. A batch
// of transitions (from Tick, for instance) completes entirely under one lock
// hold before events are delivered one by one, so even the first event
// delivered already sees the post-transition state of the rest.
func (e *Engine) State(ruleID, key string) (State, Severity, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	rr, ok := e.rules[ruleID]
	if !ok {
		return StateOK, 0, false
	}
	ks, ok := rr.keys[key]
	if !ok {
		return StateOK, 0, false
	}
	return ks.state, ks.severity, true
}

// ForgetRule silently clears every key's state under one rule, keeping the
// rule itself. Use it when the meaning of the condition changes (a rewritten
// query, say): observations in the old window must not count towards the new
// condition, and old Firing state must not suppress the new condition's first
// alert. Same semantics as a rule disappearing — no resolved event.
// Serialised with delivery (see SetRules).
func (e *Engine) ForgetRule(ruleID string) {
	e.emitMu.Lock()
	defer e.emitMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	rr, ok := e.rules[ruleID]
	if !ok {
		return
	}
	rr.keys = map[string]*keyState{}
	rr.overCap = false
}

// ObserveEvent feeds one event-style observation, such as a log line
// matching, with the value fixed at 1.
func (e *Engine) ObserveEvent(ruleID, key string, at time.Time) {
	e.Observe(ruleID, key, 1, at)
}

// ObserveEventMeta is ObserveEvent with a payload attached; see ObserveMeta
// for the payload semantics.
func (e *Engine) ObserveEventMeta(ruleID, key string, at time.Time, meta any) {
	e.ObserveMeta(ruleID, key, 1, at, meta)
}

// evalLocked 條件評估與狀態轉移,Observe 與 Tick 共用;呼叫端持鎖。
func (e *Engine) evalLocked(rr *ruleRuntime, ruleID, key string, ks *keyState, now time.Time) []Event {
	w := ks.ring.view(now)
	level, breached := evalLevels(rr.rule.Levels, w)

	ev := func(kind EventKind, sev Severity, since time.Time) Event {
		// 量測值:命中時用命中等級的條件;解除時用最高等級條件的量測
		// (如差分條件在 resolve 時仍應報差分而非原始累積值)。
		cond := rr.rule.Levels[0].Condition
		if breached {
			cond = level.Condition
		}
		val := measureOf(cond, w)
		return Event{
			RuleID: ruleID, Key: key, Kind: kind,
			Severity: sev, State: ks.state, Value: val, Since: since, At: now,
			Meta: ks.meta,
		}
	}

	switch ks.state {
	case StateOK:
		if !breached {
			return nil
		}
		ks.pendingSince = now
		if rr.rule.For > 0 {
			ks.state = StatePending
			ks.severity = level.Severity
			ks.since = now
			return nil // Pending 靜默
		}
		ks.state = StateFiring
		ks.severity = level.Severity
		ks.since = now
		ks.lastNotify = now
		return []Event{ev(EventFire, level.Severity, ks.pendingSince)}

	case StatePending:
		if !breached {
			ks.state = StateOK
			ks.since = now
			return nil // 未滿 For 即解除,靜默退回
		}
		ks.severity = level.Severity
		if now.Sub(ks.pendingSince) < rr.rule.For {
			return nil
		}
		ks.state = StateFiring
		ks.since = now
		ks.lastNotify = now
		return []Event{ev(EventFire, level.Severity, ks.pendingSince)}

	case StateFiring:
		if breached {
			ks.clearSince = time.Time{}
			if level.Severity == ks.severity {
				return nil
			}
			kind := EventEscalate
			if level.Severity > ks.severity {
				if level.Escalate != nil && !level.Escalate.Breach(w) {
					return nil // 本等級的升級判定未成立,維持原等級
				}
			}
			if level.Severity < ks.severity {
				if exit := exitOf(rr.rule.Levels, ks.severity); exit != nil && !exit.Breach(w) {
					return nil // 現行等級的降級判定未成立,維持原等級
				}
				kind = EventDeescalate
			}
			ks.severity = level.Severity
			ks.lastNotify = now
			return []Event{ev(kind, level.Severity, ks.since)}
		}
		if rr.rule.Clear != nil {
			if !rr.rule.Clear.Breach(w) {
				return nil // 條件解除但未達恢復判定,維持 Firing
			}
		} else if rr.rule.ClearFor > 0 {
			if ks.clearSince.IsZero() {
				ks.clearSince = now
				return nil
			}
			if now.Sub(ks.clearSince) < rr.rule.ClearFor {
				return nil
			}
		}
		sev := ks.severity
		ks.state = StateOK
		ks.severity = 0
		ks.clearSince = time.Time{}
		ks.since = now
		return []Event{ev(EventResolve, sev, now)}
	}
	return nil
}

func evalLevels(levels []Level, w Window) (Level, bool) {
	for _, lv := range levels { // 已依 severity 由高至低排序
		if lv.Condition.Breach(w) {
			return lv, true
		}
	}
	return Level{}, false
}

// exitOf 回傳指定等級的降級判定;等級不存在或未設定回 nil。
func exitOf(levels []Level, sev Severity) Condition {
	for _, lv := range levels {
		if lv.Severity == sev {
			return lv.Exit
		}
	}
	return nil
}

// Tick advances time: it re-evaluates time-dependent conditions as their
// windows decay, detects Stale and Vanish, and emits due Reminders. Drive it
// from the caller's existing loop, or use Run.
func (e *Engine) Tick(now time.Time) {
	e.emitMu.Lock()
	defer e.emitMu.Unlock()
	e.mu.Lock()
	var evs []Event
	for ruleID, rr := range e.rules {
		stale := rr.staleAfter(e)
		vanish := rr.vanishAfter(e)
		for key, ks := range rr.keys {
			idle := now.Sub(ks.lastObserve)

			if vanish > 0 && idle >= vanish {
				firing := ks.state == StateFiring ||
					(ks.state == StateStale && ks.prevState == StateFiring)
				delete(rr.keys, key)
				rr.overCap = false
				if firing { // 未告警 key 消失維持靜默
					sev := ks.severity
					if ks.state == StateStale {
						sev = ks.prevSeverity
					}
					evs = append(evs, Event{
						RuleID: ruleID, Key: key, Kind: EventVanish,
						Severity: sev, State: StateOK, Since: ks.since, At: now,
						Meta: ks.meta,
					})
				}
				continue
			}

			if ks.state == StateStale {
				continue // 無新資料,不重評條件
			}

			if stale > 0 && idle >= stale {
				ks.prevState, ks.prevSeverity = ks.state, ks.severity
				// For/ClearFor 的持續時間同屬連續性,不得跨斷線累計:
				// 未成立的 Pending 降回 OK,恢復後重新累計;Firing 保留,
				// 但解除中的 clearSince 歸零,恢復後重新起算 ClearFor。
				if ks.prevState == StatePending {
					ks.prevState, ks.prevSeverity = StateOK, 0
				}
				ks.pendingSince = time.Time{}
				ks.clearSince = time.Time{}
				ks.state = StateStale
				ks.since = now
				// 資料中斷即連續性中斷:清空視窗,恢復後的次數型條件
				// (ConsecutiveN 等)不得跨斷線期計數。時間視窗條件靠
				// 自然滑出,不適用此語意時以 KeepWindowOnStale 保留。
				if !rr.rule.KeepWindowOnStale {
					ks.ring.reset()
				}
				evs = append(evs, Event{
					RuleID: ruleID, Key: key, Kind: EventStale,
					Severity: ks.prevSeverity, State: StateStale,
					Since: ks.lastObserve, At: now, Meta: ks.meta,
				})
				continue
			}

			if ks.awaitValue {
				// Touch 聲明本輪無有效值:下一筆真觀測前,不得用視窗裡
				// 的舊觀測促發條件或補發 reminder(見 keyState.awaitValue);
				// 上方的 stale/vanish 存在性偵測不受影響。
				continue
			}

			// 評估時鐘單調(同 observe):遲到觀測或設備時鐘偏移使某 key
			// 的 lastObserve 領先本次 tick 的 now 時,不得以較早的 now 評估
			// ——時間視窗會排除領先 now 的真實命中,Firing 的時間視窗條件
			// (CountInWindow 等)被誤判解除,下一筆觀測又重觸發,產生假
			// resolve/refire 震盪。條件評估一律站在該 key 已知最新時刻;
			// 存在性偵測(上方 idle 的 stale/vanish)仍用真實 now。
			evalAt := now
			if ks.lastObserve.After(evalAt) {
				evalAt = ks.lastObserve
			}

			evs = append(evs, e.evalLocked(rr, ruleID, key, ks, evalAt)...)

			if ks.state == StateFiring && rr.rule.Reminder > 0 &&
				now.Sub(ks.lastNotify) >= rr.rule.Reminder {
				ks.lastNotify = now
				w := ks.ring.view(evalAt)
				level, breached := evalLevels(rr.rule.Levels, w)
				val := 0.0
				if breached {
					val = measureOf(level.Condition, w)
				}
				evs = append(evs, Event{
					RuleID: ruleID, Key: key, Kind: EventReminder,
					Severity: ks.severity, State: StateFiring, Value: val,
					Since: ks.since, At: now, Meta: ks.meta,
				})
			}
		}
	}
	e.mu.Unlock()
	e.emit(evs)
}

// Run calls Tick at a fixed interval until ctx is done. It blocks, so run it
// in its own goroutine.
func (e *Engine) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.Tick(e.clock())
		}
	}
}

// Snapshot returns one Event per key currently Firing or Stale, sorted by
// rule ID then key. It is intended for dashboards and for reconciling an
// external projection after a restart.
func (e *Engine) Snapshot() []Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.clock()
	var out []Event
	for ruleID, rr := range e.rules {
		for key, ks := range rr.keys {
			if ks.state != StateFiring && ks.state != StateStale {
				continue
			}
			kind := EventFire
			sev := ks.severity
			if ks.state == StateStale {
				kind = EventStale
				sev = ks.prevSeverity
			}
			var val float64
			w := ks.ring.view(now)
			if level, breached := evalLevels(rr.rule.Levels, w); breached {
				val = measureOf(level.Condition, w)
			} else if p, ok := w.Last(); ok {
				val = p.Value
			}
			out = append(out, Event{
				RuleID: ruleID, Key: key, Kind: kind,
				Severity: sev, State: ks.state, Value: val,
				Since: ks.since, At: now, Meta: ks.meta,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RuleID != out[j].RuleID {
			return out[i].RuleID < out[j].RuleID
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// emit 依轉移順序送出事件;呼叫端持 emitMu(不持 mu),handler 內
// 可安全使用引擎的讀取類 API。
func (e *Engine) emit(evs []Event) {
	if e.handler == nil {
		return
	}
	for _, ev := range evs {
		e.handler(ev)
	}
}
