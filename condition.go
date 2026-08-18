package alarm

import "time"

// Condition decides whether a key's observation window is in breach.
// Implementations must be stateless — all state lives in the engine — so that
// conditions can be swapped safely on a rule hot reload.
type Condition interface {
	// Breach reports whether the window satisfies the condition.
	Breach(w Window) bool
}

// pointsHinter 條件可宣告判斷所需的最少觀測筆數,引擎據此配置視窗初始容量。
// 未實作者以 defaultMinPoints 計。
type pointsHinter interface {
	minPoints() int
}

// spanHinter 時間視窗型條件可宣告判斷所需的時間跨度。引擎在視窗滿且
// 最舊觀測仍落在跨度內時自動擴容(至 maxRingCap),條件不需預估取樣頻率,
// 時間視窗也不會被筆數上限靜默截斷。
type spanHinter interface {
	minSpan() time.Duration
}

// measurer 條件可提供命中時的量測值(次數/速率),填入 Event.Value;
// 未實作者以最後觀測值計。
type measurer interface {
	measure(w Window) float64
}

const defaultMinPoints = 64

func minPointsOf(c Condition) int {
	if h, ok := c.(pointsHinter); ok {
		return h.minPoints()
	}
	return defaultMinPoints
}

func minSpanOf(c Condition) time.Duration {
	if h, ok := c.(spanHinter); ok {
		return h.minSpan()
	}
	return 0
}

func measureOf(c Condition, w Window) float64 {
	if m, ok := c.(measurer); ok {
		return m.measure(w)
	}
	if p, ok := w.Last(); ok {
		return p.Value
	}
	return 0
}

// Threshold breaches when the most recent observation satisfies judge.
// Threshold comparison stays in caller-owned code, wired in as a closure:
//
//	alarm.Threshold(func(v float64) bool { return v > limit })
func Threshold(judge func(v float64) bool) Condition {
	return threshold{judge: judge}
}

type threshold struct {
	judge func(float64) bool
}

func (c threshold) Breach(w Window) bool {
	p, ok := w.Last()
	return ok && c.judge(p.Value)
}

func (c threshold) minPoints() int { return 1 }

// ConsecutiveN breaches when the most recent n observations all satisfy
// judge. Fewer than n observations never breach.
func ConsecutiveN(n int, judge func(v float64) bool) Condition {
	return consecutiveN{n: n, judge: judge}
}

type consecutiveN struct {
	n     int
	judge func(float64) bool
}

func (c consecutiveN) Breach(w Window) bool {
	pts := w.LastN(c.n)
	if len(pts) < c.n {
		return false
	}
	for _, p := range pts {
		if !c.judge(p.Value) {
			return false
		}
	}
	return true
}

func (c consecutiveN) minPoints() int { return c.n }

// AnyN breaches when any of the most recent n observations satisfies judge.
// Fewer than n observations never breach.
func AnyN(n int, judge func(v float64) bool) Condition {
	return anyN{n: n, judge: judge}
}

type anyN struct {
	n     int
	judge func(float64) bool
}

func (c anyN) Breach(w Window) bool {
	pts := w.LastN(c.n)
	if len(pts) < c.n {
		return false
	}
	for _, p := range pts {
		if c.judge(p.Value) {
			return true
		}
	}
	return false
}

func (c anyN) minPoints() int { return c.n }

// ConsecutiveDeltaN breaches when the most recent n adjacent deltas all
// satisfy judge, which requires n+1 observations. It is for judging a
// monotonic counter by its per-round increment. Deltas are plain
// subtraction and counter resets are not compensated: the negative delta a
// reset produces is left for judge to interpret.
func ConsecutiveDeltaN(n int, judge func(delta float64) bool) Condition {
	return consecutiveDeltaN{n: n, judge: judge}
}

type consecutiveDeltaN struct {
	n     int
	judge func(float64) bool
}

func (c consecutiveDeltaN) Breach(w Window) bool {
	pts := w.LastN(c.n + 1)
	if len(pts) < c.n+1 {
		return false
	}
	for i := 1; i < len(pts); i++ {
		if !c.judge(pts[i].Value - pts[i-1].Value) {
			return false
		}
	}
	return true
}

func (c consecutiveDeltaN) minPoints() int { return c.n + 1 }

func (c consecutiveDeltaN) measure(w Window) float64 {
	pts := w.LastN(2)
	if len(pts) < 2 {
		return 0
	}
	return pts[1].Value - pts[0].Value
}

// CountInWindow breaches when at least n observations fall inside window,
// the usual shape for log-frequency alerting. The ring grows with observation
// density, so the count is not silently truncated by the initial capacity.
func CountInWindow(n int, window time.Duration) Condition {
	return countInWindow{n: n, window: window}
}

type countInWindow struct {
	n      int
	window time.Duration
}

func (c countInWindow) Breach(w Window) bool {
	return w.Count(c.window) >= c.n
}

func (c countInWindow) minPoints() int { return c.n }

func (c countInWindow) minSpan() time.Duration { return c.window }

func (c countInWindow) measure(w Window) float64 {
	return float64(w.Count(c.window))
}

// RateInWindow breaches when a counter's per-second increment over window
// satisfies judge. The rate is computed over the actual elapsed time between
// the first and last observation inside the window; fewer than two
// observations never breach.
func RateInWindow(window time.Duration, judge func(perSec float64) bool) Condition {
	return rateInWindow{window: window, judge: judge}
}

type rateInWindow struct {
	window time.Duration
	judge  func(float64) bool
}

func (c rateInWindow) rate(w Window) (float64, bool) {
	pts := w.Points(c.window)
	if len(pts) < 2 {
		return 0, false
	}
	delta, ok := w.Delta(c.window)
	if !ok {
		return 0, false
	}
	span := pts[len(pts)-1].Time.Sub(pts[0].Time).Seconds()
	if span <= 0 {
		return 0, false
	}
	return delta / span, true
}

func (c rateInWindow) Breach(w Window) bool {
	rate, ok := c.rate(w)
	return ok && c.judge(rate)
}

func (c rateInWindow) minPoints() int { return defaultMinPoints }

func (c rateInWindow) minSpan() time.Duration { return c.window }

func (c rateInWindow) measure(w Window) float64 {
	rate, _ := c.rate(w)
	return rate
}

// All breaches when every sub-condition breaches.
func All(cs ...Condition) Condition { return combined{cs: cs, all: true} }

// Any breaches when at least one sub-condition breaches.
func Any(cs ...Condition) Condition { return combined{cs: cs} }

type combined struct {
	cs  []Condition
	all bool
}

func (c combined) Breach(w Window) bool {
	for _, sub := range c.cs {
		if sub.Breach(w) != c.all {
			return !c.all
		}
	}
	return c.all
}

func (c combined) minPoints() int {
	max := 1
	for _, sub := range c.cs {
		if n := minPointsOf(sub); n > max {
			max = n
		}
	}
	return max
}

func (c combined) minSpan() time.Duration {
	var max time.Duration
	for _, sub := range c.cs {
		if d := minSpanOf(sub); d > max {
			max = d
		}
	}
	return max
}
