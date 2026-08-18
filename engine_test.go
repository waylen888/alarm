package alarm

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder 收集 Handler 事件供斷言。
type recorder struct{ evs []Event }

func (r *recorder) handle(ev Event) { r.evs = append(r.evs, ev) }
func (r *recorder) kinds() []string {
	out := make([]string, len(r.evs))
	for i, ev := range r.evs {
		out[i] = ev.Kind.String()
	}
	return out
}
func (r *recorder) reset() { r.evs = nil }

func kindsEqual(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func newTestEngine(t *testing.T, rules ...Rule) (*Engine, *recorder) {
	t.Helper()
	rec := &recorder{}
	e := New(rec.handle, WithClock(func() time.Time { return bt }))
	e.SetRules(rules)
	return e, rec
}

func TestFireAndResolveImmediate(t *testing.T) {
	e, rec := newTestEngine(t, Rule{
		ID:     "r",
		Levels: []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	})

	e.Observe("r", "k", 90, at(0))
	e.Observe("r", "k", 95, at(5)) // 已 Firing,不重複告警
	e.Observe("r", "k", 50, at(10))

	if !kindsEqual(rec.kinds(), "fire", "resolve") {
		t.Fatalf("events = %v; want [fire resolve]", rec.kinds())
	}
	fire := rec.evs[0]
	if fire.Severity != SeverityError || fire.State != StateFiring || fire.Value != 90 {
		t.Fatalf("fire event = %+v", fire)
	}
	if !fire.Since.Equal(at(0)) {
		t.Fatalf("fire.Since = %v; want %v", fire.Since, at(0))
	}
}

func TestForPending(t *testing.T) {
	rule := Rule{
		ID:     "r",
		For:    30 * time.Second,
		Levels: []Level{{Severity: SeverityWarn, Condition: Threshold(gt(80))}},
	}

	t.Run("fire after For via observe", func(t *testing.T) {
		e, rec := newTestEngine(t, rule)
		e.Observe("r", "k", 90, at(0))
		e.Observe("r", "k", 91, at(15)) // 未滿 For,靜默
		if len(rec.evs) != 0 {
			t.Fatalf("pending should be silent, got %v", rec.kinds())
		}
		e.Observe("r", "k", 92, at(30))
		if !kindsEqual(rec.kinds(), "fire") {
			t.Fatalf("events = %v; want [fire]", rec.kinds())
		}
		if !rec.evs[0].Since.Equal(at(0)) {
			t.Fatalf("fire.Since = %v; want first breach %v", rec.evs[0].Since, at(0))
		}
	})

	t.Run("lapse during pending is silent", func(t *testing.T) {
		e, rec := newTestEngine(t, rule)
		e.Observe("r", "k", 90, at(0))
		e.Observe("r", "k", 50, at(10)) // 未滿 For 即解除
		e.Observe("r", "k", 90, at(20)) // 重新起算
		e.Observe("r", "k", 90, at(40))
		if len(rec.evs) != 0 {
			t.Fatalf("got %v; want none (For restarted at 20s)", rec.kinds())
		}
		e.Observe("r", "k", 90, at(50))
		if !kindsEqual(rec.kinds(), "fire") {
			t.Fatalf("events = %v; want [fire]", rec.kinds())
		}
	})

	t.Run("fire via tick", func(t *testing.T) {
		e, rec := newTestEngine(t, rule)
		e.Observe("r", "k", 90, at(0))
		e.Tick(at(30))
		if !kindsEqual(rec.kinds(), "fire") {
			t.Fatalf("events = %v; want [fire]", rec.kinds())
		}
	})
}

func TestCountInWindowDecayViaTick(t *testing.T) {
	// 事件型規則:10 分鐘內 >=3 次;無資料是常態,關閉 stale/vanish
	e, rec := newTestEngine(t, Rule{
		ID:          "log",
		Levels:      []Level{{Severity: SeverityWarn, Condition: CountInWindow(3, 10*time.Minute)}},
		StaleAfter:  -1,
		VanishAfter: -1,
	})

	e.ObserveEvent("log", "k", at(0))
	e.ObserveEvent("log", "k", at(60))
	if len(rec.evs) != 0 {
		t.Fatalf("2 events should not fire, got %v", rec.kinds())
	}
	e.ObserveEvent("log", "k", at(120))
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("events = %v; want [fire]", rec.kinds())
	}
	if rec.evs[0].Value != 3 {
		t.Fatalf("fire.Value = %v; want count 3", rec.evs[0].Value)
	}
	rec.reset()

	e.Tick(at(300)) // 事件仍在視窗內
	if len(rec.evs) != 0 {
		t.Fatalf("still in window, got %v", rec.kinds())
	}
	e.Tick(at(800)) // 全部滑出 10 分鐘視窗 → 解除
	if !kindsEqual(rec.kinds(), "resolve") {
		t.Fatalf("events = %v; want [resolve]", rec.kinds())
	}
}

func TestSeverityEscalation(t *testing.T) {
	e, rec := newTestEngine(t, Rule{
		ID: "r",
		Levels: []Level{
			{Severity: SeverityWarn, Condition: Threshold(gt(80))},
			{Severity: SeverityError, Condition: Threshold(gt(95))},
		},
	})

	e.Observe("r", "k", 85, at(0))  // warn fire
	e.Observe("r", "k", 99, at(10)) // escalate to error
	e.Observe("r", "k", 85, at(20)) // deescalate to warn
	e.Observe("r", "k", 50, at(30)) // resolve

	if !kindsEqual(rec.kinds(), "fire", "escalate", "deescalate", "resolve") {
		t.Fatalf("events = %v", rec.kinds())
	}
	if rec.evs[0].Severity != SeverityWarn || rec.evs[1].Severity != SeverityError ||
		rec.evs[2].Severity != SeverityWarn || rec.evs[3].Severity != SeverityWarn {
		t.Fatalf("severities wrong: %+v", rec.evs)
	}
}

func TestClearForFlapDamping(t *testing.T) {
	e, rec := newTestEngine(t, Rule{
		ID:       "r",
		ClearFor: 30 * time.Second,
		Levels:   []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	})

	e.Observe("r", "k", 90, at(0))  // fire
	e.Observe("r", "k", 50, at(10)) // 開始解除計時
	e.Observe("r", "k", 90, at(20)) // 未滿 ClearFor 又達標 → 取消解除,靜默
	e.Observe("r", "k", 50, at(30)) // 重新起算
	e.Observe("r", "k", 50, at(50))
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("events = %v; want only [fire] (flap absorbed)", rec.kinds())
	}
	e.Observe("r", "k", 50, at(60)) // 解除滿 30s → resolve
	if !kindsEqual(rec.kinds(), "fire", "resolve") {
		t.Fatalf("events = %v; want [fire resolve]", rec.kinds())
	}
}

func TestReminder(t *testing.T) {
	e, rec := newTestEngine(t, Rule{
		ID:       "r",
		Reminder: time.Minute,
		Levels:   []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	})

	e.Observe("r", "k", 90, at(0))
	e.Tick(at(30)) // 未到補發間隔
	e.Tick(at(60))
	e.Tick(at(90))
	e.Tick(at(120))
	if !kindsEqual(rec.kinds(), "fire", "reminder", "reminder") {
		t.Fatalf("events = %v; want [fire reminder reminder]", rec.kinds())
	}
	rec.reset()
	e.Observe("r", "k", 50, at(130)) // resolve 後不再補發
	e.Tick(at(300))
	if !kindsEqual(rec.kinds(), "resolve") {
		t.Fatalf("events = %v; want [resolve]", rec.kinds())
	}
}

func TestStaleAndRecover(t *testing.T) {
	e, rec := newTestEngine(t, Rule{
		ID:         "r",
		StaleAfter: time.Minute,
		Levels:     []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	})

	e.Observe("r", "k", 90, at(0)) // fire
	e.Tick(at(30))                 // 未逾 stale
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("events = %v", rec.kinds())
	}
	e.Tick(at(60)) // 逾 stale
	if !kindsEqual(rec.kinds(), "fire", "stale") {
		t.Fatalf("events = %v; want [fire stale]", rec.kinds())
	}
	if !rec.evs[1].Since.Equal(at(0)) {
		t.Fatalf("stale.Since = %v; want last observe %v", rec.evs[1].Since, at(0))
	}
	rec.reset()

	e.Tick(at(120)) // 已 Stale,不重複
	if len(rec.evs) != 0 {
		t.Fatalf("stale should not repeat, got %v", rec.kinds())
	}

	e.Observe("r", "k", 90, at(150)) // 資料恢復,原 Firing 保留、不重複 fire
	if !kindsEqual(rec.kinds(), "stale_recover") {
		t.Fatalf("events = %v; want [stale_recover]", rec.kinds())
	}
	// Since 契約:恢復事件報告還原後的狀態,自恢復當下起算,與緊接的
	// Snapshot 一致(不得沿用 stale 期間的起點)。
	if !rec.evs[0].Since.Equal(at(150)) {
		t.Fatalf("stale_recover.Since = %v; want recovery time %v", rec.evs[0].Since, at(150))
	}
	rec.reset()
	e.Observe("r", "k", 50, at(160))
	if !kindsEqual(rec.kinds(), "resolve") {
		t.Fatalf("events = %v; want [resolve] (firing preserved across stale)", rec.kinds())
	}
}

func TestVanish(t *testing.T) {
	e, rec := newTestEngine(t, Rule{
		ID:          "r",
		StaleAfter:  time.Minute,
		VanishAfter: 5 * time.Minute,
		Levels:      []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	})

	e.Observe("r", "k1", 90, at(0)) // firing
	e.Observe("r", "k2", 50, at(0)) // ok
	rec.reset()

	e.Tick(at(60)) // 兩個 key 都 stale(k2 原 OK 也發,handler 自行取捨)
	rec.reset()

	e.Tick(at(300)) // 逾 vanish:k1 原 Firing → vanish;k2 未告警 → 靜默清除
	if !kindsEqual(rec.kinds(), "vanish") {
		t.Fatalf("events = %v; want [vanish]", rec.kinds())
	}
	if rec.evs[0].Key != "k1" || rec.evs[0].Severity != SeverityError {
		t.Fatalf("vanish event = %+v", rec.evs[0])
	}
	if snap := e.Snapshot(); len(snap) != 0 {
		t.Fatalf("snapshot after vanish = %v; want empty", snap)
	}
}

func TestMaxKeys(t *testing.T) {
	e, rec := newTestEngine(t, Rule{
		ID:      "r",
		MaxKeys: 2,
		Levels:  []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	})

	e.Observe("r", "k1", 90, at(0))
	e.Observe("r", "k2", 90, at(0))
	e.Observe("r", "k3", 90, at(0)) // 超出上限,丟棄
	fires := 0
	for _, ev := range rec.evs {
		if ev.Kind == EventFire {
			fires++
		}
	}
	if fires != 2 {
		t.Fatalf("fires = %d; want 2 (k3 dropped)", fires)
	}
}

func TestForgetRule(t *testing.T) {
	rule := Rule{
		ID:     "r",
		Levels: []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	}
	e, rec := newTestEngine(t, rule)

	e.Observe("r", "k", 90, at(0)) // firing
	rec.reset()

	e.ForgetRule("r") // 靜默清狀態,不發 resolve
	if len(rec.evs) != 0 {
		t.Fatalf("forget should be silent, got %v", rec.kinds())
	}
	if snap := e.Snapshot(); len(snap) != 0 {
		t.Fatalf("snapshot should be empty after forget, got %+v", snap)
	}

	// 舊 Firing 已清除,再次達標重新 fire
	e.Observe("r", "k", 90, at(10))
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("events = %v; want [fire]", rec.kinds())
	}

	e.ForgetRule("unknown") // 未知規則靜默忽略
}

func TestSetRulesRemovalIsSilent(t *testing.T) {
	rule := Rule{
		ID:     "r",
		Levels: []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	}
	e, rec := newTestEngine(t, rule)

	e.Observe("r", "k", 90, at(0)) // firing
	rec.reset()

	e.SetRules(nil) // 規則移除 → 靜默清狀態,不發 resolve
	e.Tick(at(600))
	if len(rec.evs) != 0 {
		t.Fatalf("removed rule should be silent, got %v", rec.kinds())
	}

	// 同 ID 規則保留狀態:重新 SetRules 後仍 Firing,不重複 fire
	e2, rec2 := newTestEngine(t, rule)
	e2.Observe("r", "k", 90, at(0))
	e2.SetRules([]Rule{rule})
	e2.Observe("r", "k", 90, at(10))
	if !kindsEqual(rec2.kinds(), "fire") {
		t.Fatalf("events = %v; want [fire] only (state preserved)", rec2.kinds())
	}
}

func TestSnapshot(t *testing.T) {
	e, _ := newTestEngine(t, Rule{
		ID:         "r",
		StaleAfter: time.Minute,
		Levels:     []Level{{Severity: SeverityWarn, Condition: Threshold(gt(80))}},
	})

	e.Observe("r", "a", 90, at(0))
	e.Observe("r", "b", 50, at(0))

	snap := e.Snapshot()
	if len(snap) != 1 || snap[0].Key != "a" || snap[0].State != StateFiring {
		t.Fatalf("snapshot = %+v; want firing key a only", snap)
	}
}

func TestTouchSuppressesVanishWithoutEval(t *testing.T) {
	e, rec := newTestEngine(t, Rule{
		ID:          "r",
		VanishAfter: 2 * time.Minute,
		Levels:      []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	})

	e.Observe("r", "k", 90, at(0)) // fire
	rec.reset()

	// 每 90 秒 Touch 一次(counter reset 期間仍存在,只是無值可評)
	e.Touch("r", "k", at(90))
	e.Tick(at(150)) // 距最後 Touch 60s < vanish 2m → 不消失、不解除
	e.Touch("r", "k", at(180))
	e.Tick(at(280))
	if len(rec.evs) != 0 {
		t.Fatalf("Touch 應抑制 vanish 且不觸發任何評估事件, got %v", rec.kinds())
	}
	if snap := e.Snapshot(); len(snap) != 1 || snap[0].State != StateFiring {
		t.Fatalf("應維持 Firing, got %+v", snap)
	}

	// 停止 Touch → 逾時 vanish
	e.Tick(at(300 + 120))
	if !kindsEqual(rec.kinds(), "vanish") {
		t.Fatalf("events = %v; want [vanish]", rec.kinds())
	}
}

func TestTouchCreatesStateAndRecovers(t *testing.T) {
	e, rec := newTestEngine(t, Rule{
		ID:         "r",
		StaleAfter: time.Minute,
		Levels:     []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	})

	e.Touch("r", "k", at(0)) // 建立空狀態,不評估
	if len(rec.evs) != 0 {
		t.Fatalf("Touch 不應產生事件, got %v", rec.kinds())
	}

	e.Observe("r", "k2", 90, at(0)) // firing
	rec.reset()
	e.Tick(at(60)) // k、k2 皆 stale
	rec.reset()
	e.Touch("r", "k2", at(90)) // Touch 視同資料恢復
	if !kindsEqual(rec.kinds(), "stale_recover") {
		t.Fatalf("events = %v; want [stale_recover]", rec.kinds())
	}
}

func TestForgetIsSilent(t *testing.T) {
	e, rec := newTestEngine(t, Rule{
		ID:     "r",
		Levels: []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	})

	e.Observe("r", "k", 90, at(0)) // fire
	rec.reset()

	e.Forget("r", "k")
	if len(rec.evs) != 0 {
		t.Fatalf("Forget 不應發事件, got %v", rec.kinds())
	}
	if snap := e.Snapshot(); len(snap) != 0 {
		t.Fatalf("Forget 後不應有狀態, got %+v", snap)
	}

	// 重新出現視同全新 key,重新觸發
	e.Observe("r", "k", 90, at(10))
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("events = %v; want [fire]", rec.kinds())
	}
}

func TestObserveUnknownRule(t *testing.T) {
	e, rec := newTestEngine(t)
	e.Observe("nope", "k", 90, at(0)) // 不 panic、靜默
	if len(rec.evs) != 0 {
		t.Fatalf("got %v; want none", rec.kinds())
	}
}

func TestClearConditionCountBasedRecovery(t *testing.T) {
	// 連續 2 筆達標才告警、連續 3 筆正常才恢復(次數制恢復語意)。
	e, rec := newTestEngine(t, Rule{
		ID: "r",
		Levels: []Level{
			{Severity: SeverityError, Condition: ConsecutiveN(2, gt(80))},
		},
		Clear: ConsecutiveN(3, func(v float64) bool { return v <= 80 }),
	})

	e.Observe("r", "k", 90, at(0))
	e.Observe("r", "k", 90, at(10)) // fire
	e.Observe("r", "k", 50, at(20)) // 正常 1 筆,未達恢復
	e.Observe("r", "k", 50, at(30)) // 正常 2 筆
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("events = %v; want only [fire] (clear needs 3 normals)", rec.kinds())
	}
	e.Observe("r", "k", 90, at(40)) // 異常打斷恢復計數(未連續 2 筆,不重新告警)
	e.Observe("r", "k", 50, at(50))
	e.Observe("r", "k", 50, at(60))
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("events = %v; want only [fire] (clear run restarted)", rec.kinds())
	}
	e.Observe("r", "k", 50, at(70)) // 連續 3 筆正常 → resolve
	if !kindsEqual(rec.kinds(), "fire", "resolve") {
		t.Fatalf("events = %v; want [fire resolve]", rec.kinds())
	}
	if rec.evs[1].Severity != SeverityError {
		t.Fatalf("resolve severity = %v; want error", rec.evs[1].Severity)
	}
}

func TestClearConditionRingCap(t *testing.T) {
	// Clear 需要的視窗筆數大於 Level 條件時,ringCap 必須依 Clear 配置,
	// 否則視窗不足永遠無法恢復。
	e, rec := newTestEngine(t, Rule{
		ID: "r",
		Levels: []Level{
			{Severity: SeverityError, Condition: ConsecutiveN(2, gt(80))},
		},
		Clear: ConsecutiveN(minRingCap+4, func(v float64) bool { return v <= 80 }),
	})

	e.Observe("r", "k", 90, at(0))
	e.Observe("r", "k", 90, at(1)) // fire
	for i := 0; i < minRingCap+4; i++ {
		e.Observe("r", "k", 50, at(2+i))
	}
	if !kindsEqual(rec.kinds(), "fire", "resolve") {
		t.Fatalf("events = %v; want [fire resolve]", rec.kinds())
	}
}

func TestStaleResetsWindow(t *testing.T) {
	// 進入 Stale 清空視窗:恢復後 ConsecutiveN 不得把斷線前的觀測併入計數。
	e, rec := newTestEngine(t, Rule{
		ID:         "r",
		StaleAfter: time.Minute,
		Levels:     []Level{{Severity: SeverityError, Condition: ConsecutiveN(3, gt(80))}},
	})

	e.Observe("r", "k", 90, at(0)) // 累積 2 筆異常(未達 3,未告警)
	e.Observe("r", "k", 90, at(10))
	e.Tick(at(120)) // 逾 stale → 視窗清空
	if !kindsEqual(rec.kinds(), "stale") {
		t.Fatalf("events = %v; want [stale]", rec.kinds())
	}
	rec.reset()

	e.Observe("r", "k", 90, at(180)) // 恢復後第 1 筆:若視窗未清會湊滿 3 筆誤觸發
	if !kindsEqual(rec.kinds(), "stale_recover") {
		t.Fatalf("events = %v; want [stale_recover] only", rec.kinds())
	}
	rec.reset()
	e.Observe("r", "k", 90, at(190))
	e.Observe("r", "k", 90, at(200)) // 斷線後重新連續 3 筆 → fire
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("events = %v; want [fire]", rec.kinds())
	}
}

func TestStaleResetsContinuityTimers(t *testing.T) {
	// 資料中斷即連續性中斷:For/ClearFor 的持續時間不得跨斷線期累計,
	// 恢復後第一筆達標觀測不能沿用斷線前累積的時間立即 fire/resolve。
	t.Run("pending For restarts after stale", func(t *testing.T) {
		e, rec := newTestEngine(t, Rule{
			ID:         "r",
			For:        30 * time.Second,
			StaleAfter: time.Minute,
			Levels:     []Level{{Severity: SeverityWarn, Condition: Threshold(gt(80))}},
		})
		e.Observe("r", "k", 90, at(0)) // Pending 起算
		e.Tick(at(90))                 // 逾 stale
		if !kindsEqual(rec.kinds(), "stale") {
			t.Fatalf("events = %v; want [stale]", rec.kinds())
		}
		rec.reset()

		e.Observe("r", "k", 91, at(120)) // 恢復:若沿用舊 pendingSince 會立即 fire
		if !kindsEqual(rec.kinds(), "stale_recover") {
			t.Fatalf("events = %v; want [stale_recover] only, For must restart", rec.kinds())
		}
		rec.reset()
		e.Observe("r", "k", 92, at(150)) // 恢復後重新累滿 For
		if !kindsEqual(rec.kinds(), "fire") {
			t.Fatalf("events = %v; want [fire]", rec.kinds())
		}
		if !rec.evs[0].Since.Equal(at(120)) {
			t.Fatalf("fire.Since = %v; want %v (restarted after recovery)", rec.evs[0].Since, at(120))
		}
	})

	t.Run("clearFor restarts after stale", func(t *testing.T) {
		e, rec := newTestEngine(t, Rule{
			ID:         "r",
			ClearFor:   30 * time.Second,
			StaleAfter: time.Minute,
			Levels:     []Level{{Severity: SeverityWarn, Condition: Threshold(gt(80))}},
		})
		e.Observe("r", "k", 90, at(0))  // fire
		e.Observe("r", "k", 50, at(10)) // 條件解除,ClearFor 起算
		e.Tick(at(90))                  // 逾 stale
		if !kindsEqual(rec.kinds(), "fire", "stale") {
			t.Fatalf("events = %v; want [fire stale]", rec.kinds())
		}
		rec.reset()

		e.Observe("r", "k", 50, at(120)) // 恢復:若沿用舊 clearSince 會立即 resolve
		if !kindsEqual(rec.kinds(), "stale_recover") {
			t.Fatalf("events = %v; want [stale_recover] only, ClearFor must restart", rec.kinds())
		}
		rec.reset()
		e.Observe("r", "k", 50, at(150)) // 恢復後重新累滿 ClearFor
		if !kindsEqual(rec.kinds(), "resolve") {
			t.Fatalf("events = %v; want [resolve]", rec.kinds())
		}
	})
}

func TestCountWindowIgnoresFuturePoints(t *testing.T) {
	// 亂序餵入:晚處理的舊條目評估時,已入環的未來點不得計入其視窗,
	// 否則相隔甚遠的兩筆會誤湊滿短視窗(如 1 秒視窗、相隔 10 秒)。
	e, rec := newTestEngine(t, Rule{
		ID:          "r",
		Levels:      []Level{{Severity: SeverityWarn, Condition: CountInWindow(2, time.Second)}},
		StaleAfter:  -1,
		VanishAfter: -1,
	})
	e.ObserveEvent("r", "k", at(10))
	e.ObserveEvent("r", "k", at(0)) // 舊條目後到:評估當下 at(0),at(10) 是未來點
	if len(rec.evs) != 0 {
		t.Fatalf("out-of-order pair must not satisfy 1s window, got %v", rec.kinds())
	}
}

func TestTouchFreezesTimeEvaluation(t *testing.T) {
	// Touch=本輪無有效值:Pending 不得被 Tick 用視窗裡的舊觀測促發
	// (counter reset 期間的假告警);下一筆有效超標經 Observe 依已
	// 累積的時長立即促發(不重算 For,對齊原逐樣本評估行為)。
	e, rec := newTestEngine(t, Rule{
		ID:          "r",
		For:         30 * time.Second,
		StaleAfter:  -1,
		VanishAfter: -1,
		Levels:      []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	})

	e.Observe("r", "k", 90, at(0)) // Pending 起算
	e.Touch("r", "k", at(10))      // reset:本輪無有效值
	e.Tick(at(40))                 // 逾 For,但視窗最後值已失效,不得促發
	if len(rec.evs) != 0 {
		t.Fatalf("touched pending must not be promoted by Tick, got %v", rec.kinds())
	}

	e.Observe("r", "k", 91, at(50)) // 下一筆有效超標:累積時長已滿,立即促發
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("events = %v; want [fire]", rec.kinds())
	}
	if !rec.evs[0].Since.Equal(at(0)) {
		t.Fatalf("fire.Since = %v; want %v (pending accumulated across touch)", rec.evs[0].Since, at(0))
	}
	rec.reset()

	// 真觀測解除凍結後,Tick 促發照常(事件驅動的呼叫端依賴 fire-via-Tick)。
	e.Observe("r", "k", 10, at(60))
	if !kindsEqual(rec.kinds(), "resolve") {
		t.Fatalf("events = %v; want [resolve]", rec.kinds())
	}
	rec.reset()
	e.Observe("r", "k", 95, at(70)) // Pending
	e.Tick(at(110))                 // 有效觀測在後:Tick 促發正常
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("events = %v; want [fire] via tick after real observation", rec.kinds())
	}
}

func TestSetRulesBlocksUntilDeliveryCompletes(t *testing.T) {
	// 規則變更與送達序列化:SetRules 插進「事件已產生、handler 執行中」
	// 的窗口會讓在途事件投影已拆除/重建的規則(假告警、鏡像卡死);
	// 必須等在途送達完成才返回。
	block := make(chan struct{})
	entered := make(chan struct{})
	e := New(func(ev Event) {
		close(entered)
		<-block
	})
	rule := Rule{ID: "r", Levels: []Level{{Severity: SeverityWarn, Condition: Threshold(gt(80))}}}
	e.SetRules([]Rule{rule})

	go e.Observe("r", "k", 90, at(0))
	<-entered // handler 執行中(持送達鎖)

	done := make(chan struct{})
	go func() {
		e.SetRules([]Rule{})
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("SetRules must block until in-flight delivery completes")
	case <-time.After(50 * time.Millisecond):
	}

	close(block)
	<-done
}

func TestSetRulesFingerprintChangeRebuilds(t *testing.T) {
	// 指紋變更=觀測值語意變更:同 ID 也要整條重建,用舊語意產生的
	// 視窗觀測不得計入新條件;指紋相同則保留狀態。
	rule := func(fp string) Rule {
		return Rule{
			ID: "r", Fingerprint: fp,
			Levels: []Level{{Severity: SeverityError, Condition: ConsecutiveN(3, gt(0))}},
		}
	}
	e, rec := newTestEngine(t, rule("v1"))
	e.Observe("r", "k", 1, at(0))
	e.Observe("r", "k", 1, at(10)) // 舊語意下累積 2 筆

	e.SetRules([]Rule{rule("v1")}) // 指紋不變:視窗保留
	e.Observe("r", "k", 1, at(20))
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("same fingerprint should keep window, got %v", rec.kinds())
	}
	rec.reset()

	e.SetRules([]Rule{rule("v2")}) // 指紋變更:條件狀態作廢(Firing 靜默清除),存在性保留
	if snap := e.Snapshot(); len(snap) != 0 {
		t.Fatalf("snapshot = %+v; want no firing after fingerprint rebuild", snap)
	}
	if !e.Has("r", "k") {
		t.Fatal("key availability must be carried across rebuild")
	}
	e.Observe("r", "k", 1, at(30))
	e.Observe("r", "k", 1, at(40))
	if len(rec.evs) != 0 {
		t.Fatalf("2 obs under new fingerprint must not fire, got %v", rec.kinds())
	}
	e.Observe("r", "k", 1, at(50))
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("3 obs under new fingerprint should fire, got %v", rec.kinds())
	}
}

func TestObserveSettlesExpiredWindowFirst(t *testing.T) {
	// 視窗短於 Tick 間隔時,Firing 間隙中的孤立命中不得被吞:新命中
	// 前先以其時間結算過期狀態(等效先 Tick 再觀測)——否則新命中
	// 本身讓 CountInWindow 重新成立、狀態停留 Firing 無事件,單次
	// 模式的該命中永遠不通知。
	e, rec := newTestEngine(t, Rule{
		ID:          "r",
		StaleAfter:  -1,
		VanishAfter: -1,
		Levels:      []Level{{Severity: SeverityWarn, Condition: CountInWindow(1, time.Second)}},
	})

	e.ObserveEvent("r", "k", at(0)) // fire
	e.ObserveEvent("r", "k", at(5)) // 1s 視窗早已過期:先 resolve 再 fire
	if !kindsEqual(rec.kinds(), "fire", "resolve", "fire") {
		t.Fatalf("events = %v; want [fire resolve fire] (isolated hit must re-fire)", rec.kinds())
	}

	rec.reset()
	e.ObserveEvent("r", "k", at(5)) // 視窗內的連發:去重,無事件
	if len(rec.evs) != 0 {
		t.Fatalf("hit within window must dedup, got %v", rec.kinds())
	}
}

func TestLateObservationKeepsFreshness(t *testing.T) {
	// 遲到舊樣本不得倒退 lastObserve:Tick 的 idle 會被灌水,
	// 對仍有資料到達的 key 誤發 stale/vanish。
	e, rec := newTestEngine(t, Rule{
		ID:          "r",
		StaleAfter:  time.Minute,
		VanishAfter: -1,
		Levels:      []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	})
	e.Observe("r", "k", 10, at(100))
	e.Observe("r", "k", 10, at(40)) // 遲到舊點
	e.Tick(at(130))                 // 距最新觀測僅 30s,未逾 stale
	if len(rec.evs) != 0 {
		t.Fatalf("late old sample must not rewind freshness, got %v", rec.kinds())
	}
}

func TestLevelEscalateGuard(t *testing.T) {
	// 升級守門(與 Exit 對稱):Firing 於較低等級時,較高等級 Condition
	// 成立不足以升級,需 Escalate 亦成立;初次分級(OK→Firing)不受限。
	rule := Rule{
		ID: "r", StaleAfter: -1, VanishAfter: -1,
		Levels: []Level{
			{
				Severity:  SeverityError,
				Condition: All(ConsecutiveN(2, gt(0)), Threshold(gt(1))),
				Escalate:  ConsecutiveN(2, gt(1)),
			},
			{Severity: SeverityWarn, Condition: ConsecutiveN(2, gt(0))},
		},
		Clear: ConsecutiveN(2, func(v float64) bool { return v <= 0 }),
	}

	t.Run("initial mixed classification unaffected", func(t *testing.T) {
		e, rec := newTestEngine(t, rule)
		e.Observe("r", "k", 1, at(0))
		e.Observe("r", "k", 2, at(10)) // 連續 2 筆異常+當前=error → 初次即 Error
		if !kindsEqual(rec.kinds(), "fire") || rec.evs[0].Severity != SeverityError {
			t.Fatalf("events = %v (sev=%v); want initial Error fire", rec.kinds(), rec.evs)
		}
	})

	t.Run("escalation needs guard satisfied", func(t *testing.T) {
		e, rec := newTestEngine(t, rule)
		e.Observe("r", "k", 1, at(0))
		e.Observe("r", "k", 1, at(10)) // fire Warning
		if !kindsEqual(rec.kinds(), "fire") || rec.evs[0].Severity != SeverityWarn {
			t.Fatalf("events = %v; want warning fire", rec.kinds())
		}
		rec.reset()

		e.Observe("r", "k", 2, at(20)) // 單筆 error:Condition 成立但守門未過
		if len(rec.evs) != 0 {
			t.Fatalf("single error must not escalate, got %v", rec.kinds())
		}
		e.Observe("r", "k", 1, at(30)) // warn 樣本重置連續計數
		e.Observe("r", "k", 2, at(40)) // error 重新累計第 1 筆
		if len(rec.evs) != 0 {
			t.Fatalf("warn must reset the consecutive-error run, got %v", rec.kinds())
		}
		e.Observe("r", "k", 2, at(50)) // 連續 2 筆 error → 升級
		if !kindsEqual(rec.kinds(), "escalate") {
			t.Fatalf("events = %v; want [escalate]", rec.kinds())
		}
	})
}

func TestPreSettlementOnlyForTimeWindowConditions(t *testing.T) {
	// 樣本型條件(無時間衰減)不預結算:For 邊界的正常樣本必須讓
	// Pending 靜默取消——否則預結算用過期樣本促發,DurationSec=300
	// 的指標超標至 t=240、t=300 正常,會發出假的 Fire+Resolve 對。
	e, rec := newTestEngine(t, Rule{
		ID:          "r",
		For:         300 * time.Second,
		StaleAfter:  -1,
		VanishAfter: -1,
		Levels:      []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	})

	e.Observe("r", "k", 90, at(0))
	e.Observe("r", "k", 90, at(240))
	e.Observe("r", "k", 10, at(300)) // 邊界的正常樣本:靜默取消,零事件
	if len(rec.evs) != 0 {
		t.Fatalf("boundary normal sample must silently cancel pending, got %v", rec.kinds())
	}

	// 對照:持續超標到邊界照常促發。
	e.Observe("r", "k", 90, at(310))
	e.Observe("r", "k", 90, at(620))
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("events = %v; want [fire] when breach persists", rec.kinds())
	}
}

func TestLateObservationDoesNotRewindEvaluation(t *testing.T) {
	// 遲到舊觀測不得把狀態機拉回歷史時間評估:視窗上界會排除較新的
	// 真實命中,Firing 的頻率條件被誤判解除、Tick 再重新觸發,產生
	// 假 resolve/refire 震盪。
	e, rec := newTestEngine(t, Rule{
		ID:          "r",
		StaleAfter:  -1,
		VanishAfter: -1,
		Levels:      []Level{{Severity: SeverityWarn, Condition: CountInWindow(2, 10*time.Second)}},
	})

	e.ObserveEvent("r", "k", at(20))
	e.ObserveEvent("r", "k", at(25)) // 視窗內 2 筆 → fire
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("events = %v; want [fire]", rec.kinds())
	}
	rec.reset()

	e.ObserveEvent("r", "k", at(18)) // 跨 stream 遲到的舊命中
	if len(rec.evs) != 0 {
		t.Fatalf("late entry must not rewind state, got %v", rec.kinds())
	}
	e.Tick(at(26)) // 視窗內仍有 3 筆:維持 Firing,無震盪
	if len(rec.evs) != 0 {
		t.Fatalf("no churn expected after late entry, got %v", rec.kinds())
	}
}

func TestTickDoesNotRewindBeforeLatestObservation(t *testing.T) {
	// Tick 的評估時鐘須與 observe 同樣單調:設備時鐘偏移使觀測時戳領先
	// server 的 tick now 時,以較早的 now 評估會排除領先的真實命中,
	// Firing 的時間視窗條件被誤判解除、下一筆觀測又重觸發,產生假
	// resolve/refire 震盪。
	e, rec := newTestEngine(t, Rule{
		ID:          "r",
		StaleAfter:  -1,
		VanishAfter: -1,
		Levels:      []Level{{Severity: SeverityWarn, Condition: CountInWindow(2, 10*time.Second)}},
	})

	e.ObserveEvent("r", "k", at(100))
	e.ObserveEvent("r", "k", at(105)) // 視窗內 2 筆 → fire;lastObserve=105
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("events = %v; want [fire]", rec.kinds())
	}
	rec.reset()

	// tick now(102)落後於領先時戳的觀測(105):未夾限會以 now=102 評估,
	// 視窗上界排除 105 的命中 → count=1 → 假 resolve。
	e.Tick(at(102))
	if len(rec.evs) != 0 {
		t.Fatalf("tick behind latest observation must not churn, got %v", rec.kinds())
	}
	if st, _, _ := e.State("r", "k"); st != StateFiring {
		t.Fatalf("state = %v; want firing", st)
	}
}

func TestLateOldPointDoesNotBecomeLast(t *testing.T) {
	// 視窗保時間序:遲到舊點不得成為 Last(),Threshold 不得據舊值
	// 誤 resolve;真正的新值才解除。
	e, rec := newTestEngine(t, Rule{
		ID:          "r",
		StaleAfter:  -1,
		VanishAfter: -1,
		Levels:      []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	})
	e.Observe("r", "k", 90, at(10)) // fire
	if !kindsEqual(rec.kinds(), "fire") {
		t.Fatalf("events = %v; want [fire]", rec.kinds())
	}
	rec.reset()

	e.Observe("r", "k", 10, at(0)) // 遲到舊點:非最新值,不得誤 resolve
	if len(rec.evs) != 0 {
		t.Fatalf("late old point must not resolve, got %v", rec.kinds())
	}
	e.Observe("r", "k", 10, at(20)) // 真正的新值
	if !kindsEqual(rec.kinds(), "resolve") {
		t.Fatalf("events = %v; want [resolve]", rec.kinds())
	}
}

func TestRebuildCarriesAvailability(t *testing.T) {
	rule := func(fp string) Rule {
		return Rule{
			ID: "r", Fingerprint: fp,
			StaleAfter: time.Minute, VanishAfter: -1,
			Levels: []Level{{Severity: SeverityError, Condition: ConsecutiveN(2, gt(0))}},
		}
	}

	t.Run("stale survives fingerprint rebuild", func(t *testing.T) {
		// 門檻編輯不得讓進行中的資料中斷從 no_data 消失:斷線期間無
		// 觀測,key 無法由 Observe 重建,刪掉就永遠漏偵測。
		e, rec := newTestEngine(t, rule("v1"))
		e.Observe("r", "k", 0, at(0))
		e.Tick(at(90)) // 逾 stale
		if !kindsEqual(rec.kinds(), "stale") {
			t.Fatalf("events = %v; want [stale]", rec.kinds())
		}
		rec.reset()

		e.SetRules([]Rule{rule("v2")}) // 指紋重建:條件狀態作廢,存在性保留
		snap := e.Snapshot()
		if len(snap) != 1 || snap[0].State != StateStale {
			t.Fatalf("snapshot = %+v; want stale key carried across rebuild", snap)
		}

		e.Observe("r", "k", 1, at(120)) // 資料恢復:靜默 recover,新條件從零累積
		if !kindsEqual(rec.kinds(), "stale_recover") {
			t.Fatalf("events = %v; want [stale_recover] only", rec.kinds())
		}
		rec.reset()
		e.Observe("r", "k", 1, at(130)) // 新條件重新湊滿 2 筆
		if !kindsEqual(rec.kinds(), "fire") {
			t.Fatalf("events = %v; want [fire]", rec.kinds())
		}
	})

	t.Run("firing carried as OK without ghost resolve", func(t *testing.T) {
		e, rec := newTestEngine(t, rule("v1"))
		e.Observe("r", "k", 1, at(0))
		e.Observe("r", "k", 1, at(10)) // fire
		if !kindsEqual(rec.kinds(), "fire") {
			t.Fatalf("events = %v; want [fire]", rec.kinds())
		}
		rec.reset()

		e.SetRules([]Rule{rule("v2")}) // 舊條件的 Firing 不跨語意保留
		if !e.Has("r", "k") {
			t.Fatal("key availability must be carried across rebuild")
		}
		if snap := e.Snapshot(); len(snap) != 0 {
			t.Fatalf("snapshot = %+v; want no firing after semantic rebuild", snap)
		}
		e.Observe("r", "k", 0, at(20)) // 正常值:無 resolve(狀態已是 OK)
		if len(rec.evs) != 0 {
			t.Fatalf("events = %v; want none (silent clear convention)", rec.kinds())
		}
	})
}

func TestEventMetaPropagation(t *testing.T) {
	// 素材於狀態轉移的同一次持鎖期間綁定:Fire 帶轉移當下的素材,
	// 之後覆寫不影響已產生的事件;無素材觀測(Observe)保留既有素材;
	// Tick 促發的 Reminder/Vanish 帶最後一次附帶的素材。
	e, rec := newTestEngine(t, Rule{
		ID:          "r",
		Reminder:    30 * time.Second,
		StaleAfter:  -1,
		VanishAfter: 2 * time.Minute,
		Levels:      []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	})

	e.ObserveMeta("r", "k", 90, at(0), "m1")  // fire(m1)
	e.ObserveMeta("r", "k", 91, at(10), "m2") // 覆寫素材,不生事件
	e.Observe("r", "k", 92, at(20))           // 無素材觀測:保留 m2
	e.Tick(at(45))                            // reminder(m2)
	e.ObserveMeta("r", "k", 10, at(50), "m3") // resolve(m3)
	e.ObserveMeta("r", "k", 95, at(60), "m4") // fire(m4)
	e.Tick(at(200))                           // 逾 vanish → vanish(m4)

	if !kindsEqual(rec.kinds(), "fire", "reminder", "resolve", "fire", "vanish") {
		t.Fatalf("events = %v", rec.kinds())
	}
	want := []any{"m1", "m2", "m3", "m4", "m4"}
	for i, ev := range rec.evs {
		if ev.Meta != want[i] {
			t.Fatalf("evs[%d](%s).Meta = %v; want %v", i, ev.Kind, ev.Meta, want[i])
		}
	}
}

func TestObserveBlocksUntilPriorDeliveryCompletes(t *testing.T) {
	// 同步送達契約:併發呼叫端的 Observe 必須等前一個呼叫端的事件
	// 送達完畢才返回,不得只入列就走(非同步佇列語意會讓呼叫端在
	// 事件送達前繼續執行,衍生 handler 讀到過期狀態的整類 bug)。
	block := make(chan struct{})
	entered := make(chan struct{})
	var mu sync.Mutex
	var order []string
	e := New(func(ev Event) {
		mu.Lock()
		order = append(order, ev.RuleID)
		mu.Unlock()
		if ev.RuleID == "a" {
			close(entered)
			<-block
		}
	})
	e.SetRules([]Rule{
		{ID: "a", Levels: []Level{{Severity: SeverityWarn, Condition: Threshold(gt(80))}}},
		{ID: "b", Levels: []Level{{Severity: SeverityWarn, Condition: Threshold(gt(80))}}},
	})

	go e.Observe("a", "k", 90, at(0))
	<-entered // a 的 handler 執行中(持送達鎖)

	done := make(chan struct{})
	go func() {
		e.Observe("b", "k", 90, at(1))
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Observe must block until prior delivery completes")
	case <-time.After(50 * time.Millisecond):
	}

	close(block)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "a" || order[1] != "b" {
		t.Fatalf("delivery order = %v; want [a b]", order)
	}
}

func TestConcurrentTickObserveEventOrder(t *testing.T) {
	// Tick 轉 Stale 與 Observe 轉 StaleRecover 併發:事件必須依轉移順序
	// 送達——stale/stale_recover 嚴格交替。修正前狀態轉移雖被 mu 序列化,
	// 但解鎖後才回呼 handler,後發生的 StaleRecover 可先於 Stale 送達。
	var mu sync.Mutex
	var kinds []EventKind
	e := New(func(ev Event) {
		mu.Lock()
		kinds = append(kinds, ev.Kind)
		mu.Unlock()
	})
	e.SetRules([]Rule{{
		ID:         "r",
		StaleAfter: time.Minute,
		// 關閉 vanish:靜默刪 key 再重建會產生連續兩個 stale,干擾交替斷言
		VanishAfter: -1,
		Levels:      []Level{{Severity: SeverityWarn, Condition: Threshold(gt(80))}},
	}})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := range 500 {
			e.Observe("r", "k", 1, at(i*120))
		}
	}()
	go func() {
		defer wg.Done()
		for i := range 500 {
			e.Tick(at(i*120 + 60))
		}
	}()
	wg.Wait()

	prevStale := false
	for i, k := range kinds {
		switch k {
		case EventStale:
			if prevStale {
				t.Fatalf("kinds[%d]: consecutive stale without recover", i)
			}
			prevStale = true
		case EventStaleRecover:
			if !prevStale {
				t.Fatalf("kinds[%d]: stale_recover delivered before its stale", i)
			}
			prevStale = false
		default:
			t.Fatalf("kinds[%d]: unexpected %s", i, k)
		}
	}
}

func TestSetRulesWarnsUnsatisfiablePoints(t *testing.T) {
	// 條件所需筆數超過視窗上限即永遠不可能成立,SetRules 需 log 警告
	// 而非靜默夾限(設定層另以 MaxWindowPoints 驗證擋下)。
	var logs []string
	e := New(func(Event) {}, WithLogf(func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}))
	e.SetRules([]Rule{{
		ID:     "r",
		Levels: []Level{{Severity: SeverityWarn, Condition: CountInWindow(MaxWindowPoints+1, time.Minute)}},
	}})
	if len(logs) != 1 || !strings.Contains(logs[0], "can never be satisfied") {
		t.Fatalf("logs = %v; want one unsatisfiable warning", logs)
	}
}

func TestClampDeltaPointsStaysSatisfiable(t *testing.T) {
	// 差分條件的 N 組差分需 N+1 筆觀測:直接用 ClampPoints 夾限會讓
	// 超界設定需要 MaxWindowPoints+1 筆而永不成立(正是夾限要避免的
	// 靜默失效);ClampDeltaPoints 少留一筆,條件仍可滿足。
	if got := ClampDeltaPoints(MaxWindowPoints * 2); got != MaxWindowPoints-1 {
		t.Fatalf("ClampDeltaPoints 超界值 = %d, want %d", got, MaxWindowPoints-1)
	}
	if got := ClampDeltaPoints(3); got != 3 {
		t.Fatalf("ClampDeltaPoints 界內值不應更動, got %d", got)
	}

	var logs []string
	e := New(func(Event) {}, WithLogf(func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}))
	e.SetRules([]Rule{{
		ID: "delta",
		Levels: []Level{{
			Severity:  SeverityWarn,
			Condition: ConsecutiveDeltaN(ClampDeltaPoints(MaxWindowPoints), func(float64) bool { return true }),
		}},
	}})
	if len(logs) != 0 {
		t.Fatalf("夾限後的差分條件應可滿足, logs = %v", logs)
	}

	// 對照:未扣掉差分多吃的那一筆即落入「永不成立」。
	e.SetRules([]Rule{{
		ID: "delta",
		Levels: []Level{{
			Severity:  SeverityWarn,
			Condition: ConsecutiveDeltaN(ClampPoints(MaxWindowPoints), func(float64) bool { return true }),
		}},
	}})
	if len(logs) != 1 || !strings.Contains(logs[0], "can never be satisfied") {
		t.Fatalf("logs = %v; want one unsatisfiable warning", logs)
	}
}

func TestWindowGrowsForSpanConditions(t *testing.T) {
	// 時間視窗條件:視窗滿且最舊觀測仍在跨度內時自動擴容,
	// 高於預估密度的觀測不會被筆數上限截斷(計數保持精確)。
	// Snapshot 時鐘須不早於觀測序列(生產即如此:牆鐘 ≥ 觀測時間),
	// 否則視窗上界會把「時鐘視角的未來點」排除。
	const total = 40 // 遠超初始容量 minRingCap=8
	rec := &recorder{}
	e := New(rec.handle, WithClock(func() time.Time { return at(total * 2) }))
	e.SetRules([]Rule{{
		ID:          "log",
		Levels:      []Level{{Severity: SeverityWarn, Condition: CountInWindow(3, 10*time.Minute)}},
		StaleAfter:  -1,
		VanishAfter: -1,
	}})

	for i := 0; i < total; i++ {
		e.ObserveEvent("log", "k", at(i*2))
	}
	if len(rec.evs) == 0 || rec.evs[0].Kind != EventFire {
		t.Fatalf("events = %v; want fire first", rec.kinds())
	}

	snap := e.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot = %+v; want 1 firing", snap)
	}
	if snap[0].Value != total {
		t.Fatalf("count = %v; want %d (window must grow beyond initial cap)", snap[0].Value, total)
	}
}

func TestWindowGrowthStopsAtMaxRingCap(t *testing.T) {
	rec := &recorder{}
	e := New(rec.handle, WithClock(func() time.Time { return at(maxRingCap + 100) }))
	e.SetRules([]Rule{{
		ID:          "log",
		Levels:      []Level{{Severity: SeverityWarn, Condition: CountInWindow(3, 24*time.Hour)}},
		StaleAfter:  -1,
		VanishAfter: -1,
	}})

	for i := 0; i < maxRingCap+100; i++ {
		e.ObserveEvent("log", "k", at(i))
	}

	snap := e.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot = %+v; want 1 firing", snap)
	}
	if snap[0].Value != maxRingCap {
		t.Fatalf("count = %v; want saturation at maxRingCap %d", snap[0].Value, maxRingCap)
	}
}

func TestKeepWindowOnStale(t *testing.T) {
	// 時間視窗條件在資料中斷後靠自然滑出,不清空視窗。
	e, rec := newTestEngine(t, Rule{
		ID:                "r",
		StaleAfter:        time.Minute,
		KeepWindowOnStale: true,
		Levels:            []Level{{Severity: SeverityError, Condition: ConsecutiveN(3, gt(80))}},
	})

	e.Observe("r", "k", 90, at(0))
	e.Observe("r", "k", 90, at(10))
	e.Tick(at(120)) // stale,但視窗保留
	if !kindsEqual(rec.kinds(), "stale") {
		t.Fatalf("events = %v; want [stale]", rec.kinds())
	}
	rec.reset()

	e.Observe("r", "k", 90, at(180)) // 第 3 筆:視窗未清 → 恢復後立即湊滿觸發
	if !kindsEqual(rec.kinds(), "stale_recover", "fire") {
		t.Fatalf("events = %v; want [stale_recover fire]", rec.kinds())
	}
}

func TestExitGatesDeescalation(t *testing.T) {
	// warn 條件(連續 n 筆 >=1)會被 error 樣本(2)涵蓋:無 Exit 時
	// error Firing 中一筆 warn 樣本即降級;Exit(連續 n 筆 <2)要求
	// 降級需連續 n 筆確實落於 warn 區間。
	e, rec := newTestEngine(t, Rule{
		ID: "r",
		Levels: []Level{
			{
				Severity:  SeverityError,
				Condition: ConsecutiveN(3, func(v float64) bool { return v >= 2 }),
				Exit:      ConsecutiveN(3, func(v float64) bool { return v < 2 }),
			},
			{Severity: SeverityWarn, Condition: ConsecutiveN(3, func(v float64) bool { return v >= 1 })},
		},
		Clear:       ConsecutiveN(3, func(v float64) bool { return v < 1 }),
		StaleAfter:  -1,
		VanishAfter: -1,
	})

	e.Observe("r", "k", 2, at(0))
	e.Observe("r", "k", 2, at(10))
	e.Observe("r", "k", 2, at(20)) // fire error
	e.Observe("r", "k", 1, at(30)) // 單筆 warn:Exit 未成立,維持 error
	e.Observe("r", "k", 2, at(40)) // error/warn 混雜:維持 error
	e.Observe("r", "k", 1, at(50))
	e.Observe("r", "k", 1, at(60))
	e.Observe("r", "k", 1, at(70)) // 連續 3 筆 warn 區間 → deescalate

	if !kindsEqual(rec.kinds(), "fire", "deescalate") {
		t.Fatalf("events = %v; want [fire deescalate]", rec.kinds())
	}
	if rec.evs[0].Severity != SeverityError || rec.evs[1].Severity != SeverityWarn {
		t.Fatalf("severities wrong: %+v", rec.evs)
	}
	rec.reset()

	e.Observe("r", "k", 0, at(80))
	e.Observe("r", "k", 0, at(90))
	e.Observe("r", "k", 0, at(100)) // Clear 連續 3 筆正常 → resolve
	if !kindsEqual(rec.kinds(), "resolve") {
		t.Fatalf("events = %v; want [resolve]", rec.kinds())
	}
}

func TestExitDoesNotGateEscalation(t *testing.T) {
	// Exit 只擋降級:warn Firing 中 error 條件成立仍立即升級。
	e, rec := newTestEngine(t, Rule{
		ID: "r",
		Levels: []Level{
			{
				Severity:  SeverityError,
				Condition: ConsecutiveN(2, func(v float64) bool { return v >= 2 }),
				Exit:      ConsecutiveN(2, func(v float64) bool { return v < 2 }),
			},
			{Severity: SeverityWarn, Condition: ConsecutiveN(2, func(v float64) bool { return v >= 1 })},
		},
		StaleAfter:  -1,
		VanishAfter: -1,
	})

	e.Observe("r", "k", 1, at(0))
	e.Observe("r", "k", 1, at(10)) // fire warn
	e.Observe("r", "k", 2, at(20))
	e.Observe("r", "k", 2, at(30)) // escalate error

	if !kindsEqual(rec.kinds(), "fire", "escalate") {
		t.Fatalf("events = %v; want [fire escalate]", rec.kinds())
	}
}

func TestHas(t *testing.T) {
	e, _ := newTestEngine(t, Rule{
		ID:     "r",
		Levels: []Level{{Severity: SeverityError, Condition: Threshold(gt(80))}},
	})

	if e.Has("r", "k") {
		t.Fatal("empty rule should not have key")
	}
	e.Observe("r", "k", 50, at(0)) // OK 狀態也算存在
	if !e.Has("r", "k") {
		t.Fatal("observed key should exist")
	}
	e.Forget("r", "k")
	if e.Has("r", "k") {
		t.Fatal("forgotten key should not exist")
	}
	if e.Has("unknown", "k") {
		t.Fatal("unknown rule should not have key")
	}
}
