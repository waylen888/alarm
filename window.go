package alarm

import "time"

// Point is a single observation.
type Point struct {
	Time  time.Time
	Value float64
}

// Window is one key's observation window and the only input a Condition
// gets. Every since is measured backwards from the moment of evaluation, so
// event-style conditions such as CountInWindow decay as time advances and are
// released by Engine.Tick.
type Window interface {
	// Last returns the most recent observation. Unlike Points and Count it is
	// not bounded by the moment of evaluation: it returns the newest
	// observation held, even one timestamped after that moment.
	Last() (Point, bool)
	// LastN returns the most recent n observations (all of them if there are
	// fewer), oldest first. Like Last, and unlike Points and Count, it is not
	// bounded by the moment of evaluation.
	LastN(n int) []Point
	// Points returns the observations within since of the moment of
	// evaluation, oldest first.
	Points(since time.Duration) []Point
	// Count returns how many observations fall within since of the moment of
	// evaluation.
	Count(since time.Duration) int
	// Delta is the counter difference over since: the sum of the increments,
	// treating a decrease as a counter reset back to zero. It returns
	// (0, false) when there are fewer than two observations.
	Delta(since time.Duration) (float64, bool)
}

// ring 固定容量環形緩衝,舊資料自動覆寫。非併發安全,由 Engine 持鎖操作。
type ring struct {
	pts  []Point
	head int // 下一個寫入位置
	n    int
}

func newRing(capacity int) *ring {
	return &ring{pts: make([]Point, capacity)}
}

// reset 清空所有觀測(容量不變)。
func (r *ring) reset() {
	r.head = 0
	r.n = 0
}

func (r *ring) full() bool { return r.n == len(r.pts) }

func (r *ring) capacity() int { return len(r.pts) }

// oldest 最舊一筆觀測。
func (r *ring) oldest() (Point, bool) {
	if r.n == 0 {
		return Point{}, false
	}
	start := r.head - r.n
	if start < 0 {
		start += len(r.pts)
	}
	return r.pts[start], true
}

// grow 擴容並保序搬遷既有觀測;容量只增不減。
func (r *ring) grow(capacity int) {
	if capacity <= len(r.pts) {
		return
	}
	pts := make([]Point, capacity)
	i := 0
	r.each(func(p Point) {
		pts[i] = p
		i++
	})
	r.pts = pts
	r.head = i % capacity
	r.n = i
}

// push 保序插入。視窗承諾「依時間由舊到新」,此不變量在唯一寫入點
// 強制:亂序餵入(如 syslog 跨 stream 合併)的遲到舊點插回正確位置,
// 否則 Last/LastN 把舊點當最新(Threshold 誤判)、Delta/Rate 算出
// 反向時距。快路徑(時間不回退)維持純追加,攤銷零成本;回退時
// 線性重建(僅亂序餵入發生,頻率低);同時戳保持到達序。
func (r *ring) push(p Point) {
	if r.n == 0 || !r.last().Time.After(p.Time) {
		r.pts[r.head] = p
		r.head = (r.head + 1) % len(r.pts)
		if r.n < len(r.pts) {
			r.n++
		}
		return
	}
	if r.full() {
		if oldest, _ := r.oldest(); oldest.Time.After(p.Time) {
			return // 比滿環中最舊的還舊:等同已被擠出,直接丟棄
		}
	}
	all := make([]Point, 0, r.n+1)
	inserted := false
	r.each(func(q Point) {
		if !inserted && q.Time.After(p.Time) {
			all = append(all, p)
			inserted = true
		}
		all = append(all, q)
	})
	if len(all) > len(r.pts) {
		all = all[1:] // 滿環擠出最舊
	}
	copy(r.pts, all)
	r.n = len(all)
	r.head = r.n % len(r.pts)
}

// last 最新一筆觀測;呼叫端保證 n > 0。
func (r *ring) last() Point {
	idx := r.head - 1
	if idx < 0 {
		idx += len(r.pts)
	}
	return r.pts[idx]
}

// each 依時間由舊到新走訪。
func (r *ring) each(fn func(Point)) {
	start := r.head - r.n
	if start < 0 {
		start += len(r.pts)
	}
	for i := 0; i < r.n; i++ {
		fn(r.pts[(start+i)%len(r.pts)])
	}
}

// view 以 now 為評估基準產生 Window 視圖。
func (r *ring) view(now time.Time) Window {
	return windowView{r: r, now: now}
}

type windowView struct {
	r   *ring
	now time.Time
}

func (w windowView) Last() (Point, bool) {
	if w.r.n == 0 {
		return Point{}, false
	}
	idx := w.r.head - 1
	if idx < 0 {
		idx += len(w.r.pts)
	}
	return w.r.pts[idx], true
}

func (w windowView) LastN(n int) []Point {
	if n <= 0 {
		return nil
	}
	var all []Point
	w.r.each(func(p Point) { all = append(all, p) })
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

func (w windowView) Points(since time.Duration) []Point {
	if since <= 0 {
		return nil
	}
	cutoff := w.now.Add(-since)
	var pts []Point
	w.r.each(func(p Point) {
		// 上界排除「評估當下」之後的點:餵入源亂序時(如多 stream
		// 聚合到同一 key),已入環的未來點不得計入較早評估點的視窗。
		if p.Time.After(cutoff) && !p.Time.After(w.now) {
			pts = append(pts, p)
		}
	})
	return pts
}

func (w windowView) Count(since time.Duration) int {
	if since <= 0 {
		return 0
	}
	cutoff := w.now.Add(-since)
	n := 0
	w.r.each(func(p Point) {
		if p.Time.After(cutoff) && !p.Time.After(w.now) { // 上界說明見 Points
			n++
		}
	})
	return n
}

func (w windowView) Delta(since time.Duration) (float64, bool) {
	pts := w.Points(since)
	if len(pts) < 2 {
		return 0, false
	}
	var delta float64
	prev := pts[0].Value
	for _, p := range pts[1:] {
		if p.Value >= prev {
			delta += p.Value - prev
		} else {
			// counter reset:視為從 0 重計,本筆值即為 reset 後的增量
			delta += p.Value
		}
		prev = p.Value
	}
	return delta, true
}
