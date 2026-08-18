package alarm

import (
	"testing"
	"time"
)

var bt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return bt.Add(time.Duration(sec) * time.Second) }

func TestRingWraparound(t *testing.T) {
	r := newRing(3)
	for i := 1; i <= 5; i++ {
		r.push(Point{Time: at(i), Value: float64(i)})
	}
	w := r.view(at(5))

	last, ok := w.Last()
	if !ok || last.Value != 5 {
		t.Fatalf("Last = %v, %v; want 5, true", last.Value, ok)
	}
	pts := w.LastN(10)
	if len(pts) != 3 || pts[0].Value != 3 || pts[2].Value != 5 {
		t.Fatalf("LastN(10) = %v; want [3 4 5]", pts)
	}
}

func TestWindowTimeCutoff(t *testing.T) {
	r := newRing(10)
	for i := 0; i <= 60; i += 10 {
		r.push(Point{Time: at(i), Value: float64(i)})
	}

	// 評估基準 60s,回推 30s → 只含 (30,60] 的 40/50/60 三筆
	w := r.view(at(60))
	if n := w.Count(30 * time.Second); n != 3 {
		t.Fatalf("Count(30s) = %d; want 3", n)
	}
	pts := w.Points(30 * time.Second)
	if len(pts) != 3 || pts[0].Value != 40 || pts[2].Value != 60 {
		t.Fatalf("Points(30s) = %v; want [40 50 60]", pts)
	}

	// 同一批資料,評估基準往後推 → 視窗自然衰減
	w = r.view(at(120))
	if n := w.Count(30 * time.Second); n != 0 {
		t.Fatalf("Count(30s)@120 = %d; want 0", n)
	}
}

func TestWindowDelta(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
		want   float64
		wantOK bool
	}{
		{"increasing", []float64{10, 15, 30}, 20, true},
		{"counter reset", []float64{100, 110, 5, 8}, 18, true}, // 10 + reset(5) + 3
		{"single point", []float64{10}, 0, false},
		{"empty", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRing(10)
			for i, v := range tt.values {
				r.push(Point{Time: at(i), Value: v})
			}
			got, ok := r.view(at(len(tt.values))).Delta(time.Minute)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("Delta = %v, %v; want %v, %v", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestWindowLastN(t *testing.T) {
	r := newRing(5)
	for i := 1; i <= 3; i++ {
		r.push(Point{Time: at(i), Value: float64(i)})
	}
	w := r.view(at(3))
	if pts := w.LastN(2); len(pts) != 2 || pts[0].Value != 2 || pts[1].Value != 3 {
		t.Fatalf("LastN(2) = %v; want [2 3]", pts)
	}
	if pts := w.LastN(0); pts != nil {
		t.Fatalf("LastN(0) = %v; want nil", pts)
	}
}
