package alarm

import (
	"testing"
	"time"
)

func windowOf(now time.Time, pts ...Point) Window {
	r := newRing(len(pts) + 1)
	for _, p := range pts {
		r.push(p)
	}
	return r.view(now)
}

func gt(threshold float64) func(float64) bool {
	return func(v float64) bool { return v > threshold }
}

func TestThreshold(t *testing.T) {
	c := Threshold(gt(80))
	if c.Breach(windowOf(at(1), Point{at(1), 90})) != true {
		t.Fatal("90 > 80 should breach")
	}
	if c.Breach(windowOf(at(2), Point{at(1), 90}, Point{at(2), 70})) != false {
		t.Fatal("last value 70 should not breach")
	}
	if c.Breach(windowOf(at(1))) != false {
		t.Fatal("empty window should not breach")
	}
}

func TestConsecutiveN(t *testing.T) {
	c := ConsecutiveN(3, gt(80))
	tests := []struct {
		name   string
		values []float64
		want   bool
	}{
		{"all breach", []float64{90, 91, 92}, true},
		{"not enough points", []float64{90, 91}, false},
		{"one below", []float64{90, 70, 92}, false},
		{"older below ignored", []float64{70, 90, 91, 92}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pts := make([]Point, len(tt.values))
			for i, v := range tt.values {
				pts[i] = Point{at(i), v}
			}
			if got := c.Breach(windowOf(at(len(pts)), pts...)); got != tt.want {
				t.Fatalf("Breach = %v; want %v", got, tt.want)
			}
		})
	}
}

func TestAnyN(t *testing.T) {
	c := AnyN(3, gt(80))
	tests := []struct {
		name   string
		values []float64
		want   bool
	}{
		{"one breach in window", []float64{50, 90, 50}, true},
		{"no breach", []float64{50, 60, 70}, false},
		{"not enough points", []float64{90, 90}, false},
		{"breach aged out of last n", []float64{90, 50, 50, 50}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pts := make([]Point, len(tt.values))
			for i, v := range tt.values {
				pts[i] = Point{at(i), v}
			}
			if got := c.Breach(windowOf(at(len(pts)), pts...)); got != tt.want {
				t.Fatalf("Breach = %v; want %v", got, tt.want)
			}
		})
	}
}

func TestConsecutiveDeltaN(t *testing.T) {
	c := ConsecutiveDeltaN(2, gt(100))
	// 差分 200, 300 → 皆 >100
	w := windowOf(at(3), Point{at(1), 1000}, Point{at(2), 1200}, Point{at(3), 1500})
	if !c.Breach(w) {
		t.Fatal("兩組差分皆達標應成立")
	}
	if got := measureOf(c, w); got != 300 {
		t.Fatalf("measure = %v; want 最新差分 300", got)
	}
	// 差分 200, 50 → 第二組未達標
	w = windowOf(at(3), Point{at(1), 1000}, Point{at(2), 1200}, Point{at(3), 1250})
	if c.Breach(w) {
		t.Fatal("任一差分未達標不應成立")
	}
	// 筆數不足(需 n+1=3 筆)
	w = windowOf(at(2), Point{at(1), 1000}, Point{at(2), 1200})
	if c.Breach(w) {
		t.Fatal("觀測不足不應成立")
	}
}

func TestCountInWindow(t *testing.T) {
	c := CountInWindow(3, time.Minute)
	w := windowOf(at(60), Point{at(10), 1}, Point{at(30), 1}, Point{at(50), 1})
	if !c.Breach(w) {
		t.Fatal("3 events in window should breach")
	}
	if got := measureOf(c, w); got != 3 {
		t.Fatalf("measure = %v; want 3", got)
	}
	// 事件老化滑出視窗後不再成立
	w = windowOf(at(90), Point{at(10), 1}, Point{at(30), 1}, Point{at(50), 1})
	if c.Breach(w) {
		t.Fatal("only 1 event left in window, should not breach")
	}
}

func TestRateInWindow(t *testing.T) {
	c := RateInWindow(time.Minute, gt(1.5))
	// counter 0→60 across 30s → 2/s
	w := windowOf(at(30), Point{at(0), 0}, Point{at(30), 60})
	if !c.Breach(w) {
		t.Fatal("2/s > 1.5/s should breach")
	}
	if got := measureOf(c, w); got != 2 {
		t.Fatalf("measure = %v; want 2", got)
	}
	// 0→30 across 30s → 1/s
	w = windowOf(at(30), Point{at(0), 0}, Point{at(30), 30})
	if c.Breach(w) {
		t.Fatal("1/s should not breach")
	}
	if c.Breach(windowOf(at(1), Point{at(1), 10})) {
		t.Fatal("single point should not breach")
	}
}

func TestCombinators(t *testing.T) {
	w := windowOf(at(1), Point{at(1), 90})
	yes, no := Threshold(gt(80)), Threshold(gt(100))
	if !All(yes, yes).Breach(w) || All(yes, no).Breach(w) {
		t.Fatal("All combinator wrong")
	}
	if !Any(no, yes).Breach(w) || Any(no, no).Breach(w) {
		t.Fatal("Any combinator wrong")
	}
}

func TestMinPointsSizing(t *testing.T) {
	rr := newRuleRuntime(Rule{ID: "r", Levels: []Level{
		{Severity: SeverityWarn, Condition: ConsecutiveN(20, gt(0))},
		{Severity: SeverityError, Condition: Threshold(gt(0))},
	}})
	if rr.ringCap != 20 {
		t.Fatalf("ringCap = %d; want 20 (max of level hints)", rr.ringCap)
	}
}
