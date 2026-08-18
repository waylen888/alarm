package alarm

import "time"

// Condition decides whether a key's observation window is in breach.
// Implementations must be stateless — all state lives in the engine — so that
// conditions can be swapped safely on a rule hot reload.
type Condition interface {
	// Breach reports whether the window satisfies the condition.
	Breach(w Window) bool
}

// PointsHinter is an optional interface a Condition may implement to declare
// the minimum number of observations its judgement needs. The engine uses it
// to size a key's window when the rule is installed. A Condition that does not
// implement it is assumed to need DefaultMinPoints observations.
//
// Declare this whenever the judgement inspects a fixed number of samples: a
// window sized too small can never satisfy the condition, and the engine has
// no other way to know.
type PointsHinter interface {
	MinPoints() int
}

// SpanHinter is an optional interface a Condition may implement to declare the
// time span its judgement covers. When the window is full and its oldest
// observation still falls inside the declared span, the engine grows the
// window (up to MaxWindowPoints) rather than dropping that observation. A
// Condition that does not implement it declares a span of zero.
//
// Declare this for any time-based judgement. It is what frees the condition
// from having to guess the sampling frequency, and what stops a time window
// from being silently truncated by the point cap.
type SpanHinter interface {
	MinSpan() time.Duration
}

// Measurer is an optional interface a Condition may implement to supply the
// value reported in Event.Value — a count, a rate, or whatever quantity the
// judgement is actually about. A Condition that does not implement it reports
// the most recent observed value.
//
// Declare this when the last raw observation would misrepresent the breach,
// as it does for a condition that judges a counter's increment rather than
// the counter itself.
type Measurer interface {
	Measure(w Window) float64
}

// DefaultMinPoints is the window size assumed for a Condition that does not
// implement PointsHinter. A condition whose judgement inspects more
// observations than this will never breach unless it declares that through
// PointsHinter. MaxWindowPoints is the corresponding upper bound.
const DefaultMinPoints = 64

func minPointsOf(c Condition) int {
	if h, ok := c.(PointsHinter); ok {
		return h.MinPoints()
	}
	return DefaultMinPoints
}

func minSpanOf(c Condition) time.Duration {
	if h, ok := c.(SpanHinter); ok {
		return h.MinSpan()
	}
	return 0
}

func measureOf(c Condition, w Window) float64 {
	if m, ok := c.(Measurer); ok {
		return m.Measure(w)
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

func (c threshold) MinPoints() int { return 1 }

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

func (c consecutiveN) MinPoints() int { return c.n }

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

func (c anyN) MinPoints() int { return c.n }

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

func (c consecutiveDeltaN) MinPoints() int { return c.n + 1 }

func (c consecutiveDeltaN) Measure(w Window) float64 {
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

func (c countInWindow) MinPoints() int { return c.n }

func (c countInWindow) MinSpan() time.Duration { return c.window }

func (c countInWindow) Measure(w Window) float64 {
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

func (c rateInWindow) MinPoints() int { return DefaultMinPoints }

func (c rateInWindow) MinSpan() time.Duration { return c.window }

func (c rateInWindow) Measure(w Window) float64 {
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

func (c combined) MinPoints() int {
	max := 1
	for _, sub := range c.cs {
		if n := minPointsOf(sub); n > max {
			max = n
		}
	}
	return max
}

func (c combined) MinSpan() time.Duration {
	var max time.Duration
	for _, sub := range c.cs {
		if d := minSpanOf(sub); d > max {
			max = d
		}
	}
	return max
}
