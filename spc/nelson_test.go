package spc

import (
	"math"
	"reflect"
	"testing"
)

// Every series below is expressed in sigma units against a centre of 0 and a
// sigma of 1, so a value is its own z. Each rule is checked in isolation so
// that a golden series only has to satisfy the rule under test.
const (
	zero = 0.0
	one  = 1.0
)

func check(series []float64, r Rule) []Violation {
	return Check(series, zero, one, r)
}

// indices returns just the positions the rule completed at, which is the part
// a near-miss test is about.
func indices(vs []Violation) []int {
	out := []int{}
	for _, v := range vs {
		out = append(out, v.Index)
	}
	return out
}

func wantIndices(t *testing.T, vs []Violation, want ...int) {
	t.Helper()
	if len(want) == 0 {
		want = []int{}
	}
	if got := indices(vs); !reflect.DeepEqual(got, want) {
		t.Errorf("violation indices = %v, want %v", got, want)
	}
}

// alternate returns n points alternating between +amp and -amp, starting
// above the centre line.
func alternate(n int, amp float64) []float64 {
	xs := make([]float64, n)
	for i := range xs {
		if i%2 == 0 {
			xs[i] = amp
		} else {
			xs[i] = -amp
		}
	}
	return xs
}

func repeat(n int, v float64) []float64 {
	xs := make([]float64, n)
	for i := range xs {
		xs[i] = v
	}
	return xs
}

// Rule 1: one point beyond three sigma.
func TestRule1(t *testing.T) {
	wantIndices(t, check([]float64{0, 0.5, 3.5}, Rule1), 2)
	wantIndices(t, check([]float64{0, 0.5, -3.5}, Rule1), 2) // either side
	wantIndices(t, check([]float64{0, 0.5, 3.0}, Rule1))     // exactly 3σ is inside
	wantIndices(t, check([]float64{0, 0.5, 2.99}, Rule1))
}

func TestRule1ReportsZ(t *testing.T) {
	vs := Check([]float64{10, 10, 17}, 10, 2)
	if len(vs) == 0 {
		t.Fatal("no violation")
	}
	if vs[0].Rule != Rule1 {
		t.Fatalf("first violation is %v, want Rule1", vs[0].Rule)
	}
	close(t, vs[0].Z, 3.5, "Z")
}

// Rule 2: nine consecutive points on the same side of the centre.
func TestRule2(t *testing.T) {
	wantIndices(t, check(repeat(9, 0.5), Rule2), 8)
	wantIndices(t, check(repeat(9, -0.5), Rule2), 8)
	wantIndices(t, check(repeat(10, 0.5), Rule2), 8, 9) // a continuing run reports again
	wantIndices(t, check(repeat(8, 0.5), Rule2))        // eight is not nine

	broken := repeat(9, 0.5)
	broken[4] = -0.5
	wantIndices(t, check(broken, Rule2))

	onLine := repeat(9, 0.5)
	onLine[4] = 0 // a point exactly on the centre is on neither side
	wantIndices(t, check(onLine, Rule2))

	onLineBelow := repeat(9, -0.5) // and it breaks a run below the line too
	onLineBelow[4] = 0
	wantIndices(t, check(onLineBelow, Rule2))
}

// Rule 3: six consecutive points all increasing or all decreasing.
func TestRule3(t *testing.T) {
	wantIndices(t, check([]float64{0, 0.1, 0.2, 0.3, 0.4, 0.5}, Rule3), 5)
	wantIndices(t, check([]float64{0, -0.1, -0.2, -0.3, -0.4, -0.5}, Rule3), 5)
	wantIndices(t, check([]float64{0, 0.1, 0.2, 0.3, 0.4, 0.35}, Rule3)) // five rises, then a fall
	wantIndices(t, check([]float64{0, 0.1, 0.2, 0.3, 0.4}, Rule3))       // five points

	// A plateau is neither increasing nor decreasing and breaks the run. Both
	// directions: for a rising run the direction test rejects a zero step for
	// free, so the explicit guard is load-bearing only for a falling one.
	wantIndices(t, check([]float64{0, 0.1, 0.2, 0.2, 0.3, 0.4, 0.5}, Rule3))
	wantIndices(t, check([]float64{0, -0.1, -0.2, -0.2, -0.3, -0.4, -0.5}, Rule3))
}

// Rule 4: fourteen consecutive points alternating up and down.
func TestRule4(t *testing.T) {
	wantIndices(t, check(alternate(14, 0.5), Rule4), 13)
	wantIndices(t, check(alternate(15, 0.5), Rule4), 13, 14)
	wantIndices(t, check(alternate(13, 0.5), Rule4)) // thirteen is not fourteen

	broken := alternate(14, 0.5)
	broken[7] = 0.6 // two rises in a row
	wantIndices(t, check(broken, Rule4))

	// An equal pair is no direction at all. The fixture has to be chosen with
	// care: a zero step surrounded by a rise and a fall breaks the run for a
	// second reason, so dropping the guard would still refuse it and the test
	// would pass without testing anything. Here the zero step sits where a
	// fall belongs — steps are +,-,+,0,+,-,+,-,+,-,+,-,+ — so treating it as a
	// fall, which is what happens when the guard is gone, leaves a perfectly
	// alternating series.
	flat := []float64{0, 1, 0, 1, 1, 2, 1, 2, 1, 2, 1, 2, 1, 2}
	wantIndices(t, check(flat, Rule4))
}

// Rule 5: two of three consecutive points beyond two sigma, same side.
func TestRule5(t *testing.T) {
	wantIndices(t, check([]float64{0, 2.5, 2.5}, Rule5), 2)
	wantIndices(t, check([]float64{2.5, 0, 2.5}, Rule5), 2) // need not be adjacent
	wantIndices(t, check([]float64{0, -2.5, -2.5}, Rule5), 2)
	wantIndices(t, check([]float64{0, 2.5, -2.5}, Rule5)) // opposite sides do not combine
	wantIndices(t, check([]float64{0, 2.5, 1.9}, Rule5))  // only one beyond 2σ
	wantIndices(t, check([]float64{0, 2.0, 2.0}, Rule5))  // exactly 2σ is inside

	// The point the rule completes at must itself be beyond the limit.
	// Otherwise the rule fires on a process that has already recovered, and
	// reports the sigma distance of an observation that triggered nothing.
	wantIndices(t, check([]float64{2.5, 2.5, 0.1}, Rule5))
	wantIndices(t, check([]float64{2.5, 2.5, -2.5}, Rule5)) // beyond, but the other side
}

// Rule 6: four of five consecutive points beyond one sigma, same side.
func TestRule6(t *testing.T) {
	wantIndices(t, check([]float64{1.5, 1.5, 0.5, 1.5, 1.5}, Rule6), 4)
	wantIndices(t, check([]float64{-1.5, -1.5, -0.5, -1.5, -1.5}, Rule6), 4)
	wantIndices(t, check([]float64{1.5, 1.5, 0.5, 0.5, 1.5}, Rule6)) // only three
	wantIndices(t, check([]float64{1.5, 1.5, -1.5, 1.5, -1.5}, Rule6))
	wantIndices(t, check([]float64{1.5, 1.5, 1.5, 1.5}, Rule6)) // four points

	// As for rule 5: four of the five are beyond one sigma, but the point the
	// rule would complete at is in control.
	wantIndices(t, check([]float64{1.5, 1.5, 1.5, 1.5, 0.1}, Rule6))
	wantIndices(t, check([]float64{1.5, 1.5, 1.5, 1.5, -1.5}, Rule6))
}

// Rule 7: fifteen consecutive points all within one sigma.
func TestRule7(t *testing.T) {
	wantIndices(t, check(alternate(15, 0.2), Rule7), 14)
	wantIndices(t, check(alternate(14, 0.2), Rule7)) // fourteen is not fifteen

	broken := alternate(15, 0.2)
	broken[9] = 1.2
	wantIndices(t, check(broken, Rule7))

	// A point at exactly one sigma counts as within it, matching rule 1's
	// treatment of the three-sigma limit and complementing rule 8.
	edge := alternate(15, 0.2)
	edge[9] = 1.0
	wantIndices(t, check(edge, Rule7), 14)
}

// Rule 8: eight consecutive points, on both sides, none within one sigma.
func TestRule8(t *testing.T) {
	wantIndices(t, check(alternate(8, 1.5), Rule8), 7)
	wantIndices(t, check(alternate(7, 1.5), Rule8)) // seven is not eight

	inside := alternate(8, 1.5)
	inside[3] = 0.5 // one point back inside the one-sigma band
	wantIndices(t, check(inside, Rule8))

	edge := alternate(8, 1.5)
	edge[3] = 1.0 // exactly one sigma counts as within
	wantIndices(t, check(edge, Rule8))

	// Nelson's rule 8 requires both sides. Eight points a long way out on one
	// side is a sustained shift, which rule 6 already reports three
	// observations earlier; calling it a mixture would be wrong.
	wantIndices(t, check(repeat(8, 1.5), Rule8))
}

// Rules 7 and 8 partition the same one-sigma band, so a series sitting
// exactly on the boundary must satisfy one of them. It used to satisfy
// neither.
func TestRules7And8AgreeOnTheOneSigmaBoundary(t *testing.T) {
	onTheLine := repeat(15, 1.0)
	if len(check(onTheLine, Rule7)) == 0 {
		t.Error("fifteen points at exactly one sigma should be within one sigma")
	}
	if len(check(alternate(8, 1.0), Rule8)) != 0 {
		t.Error("eight points at exactly one sigma are within one sigma, so rule 8 must not fire")
	}
}

func TestCheckDefaultsToAllRules(t *testing.T) {
	series := repeat(9, 0.5)
	if got := len(Check(series, zero, one)); got == 0 {
		t.Fatal("no rules named should mean all eight, but nothing fired")
	}
	// Naming none must apply all eight, not merely some. A series long enough
	// for every rule to complete, chosen so each one does.
	long := append(alternate(8, 1.5), 4.0) // rule 8 straddles the centre, then rule 1
	long = append(long, alternate(14, 0.5)...)
	long = append(long, []float64{0, 0.1, 0.2, 0.3, 0.4, 0.5}...)
	long = append(long, repeat(15, 0.2)...)
	long = append(long, repeat(9, 0.5)...)
	long = append(long, 2.5, 2.5, 1.5, 1.5, 1.5, 1.5)
	got := map[Rule]bool{}
	for _, v := range Check(long, zero, one) {
		got[v.Rule] = true
	}
	for _, r := range AllRules() {
		if !got[r] {
			t.Errorf("%v never fired, so naming no rules did not apply all eight", r)
		}
	}
}

func TestCheckIgnoresUnknownRules(t *testing.T) {
	if vs := Check(repeat(9, 0.5), zero, one, Rule(0), Rule(99)); len(vs) != 0 {
		t.Errorf("unknown rules should be ignored, got %v", vs)
	}
	vs := Check(repeat(9, 0.5), zero, one, Rule(99), Rule2)
	wantIndices(t, vs, 8)
}

func TestCheckRejectsAnUnusableSigma(t *testing.T) {
	for _, sigma := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if vs := Check(repeat(9, 0.5), zero, sigma); vs != nil {
			t.Errorf("sigma %v should yield no violations, got %v", sigma, vs)
		}
	}
	if vs := Check(repeat(9, 0.5), math.NaN(), one); vs != nil {
		t.Errorf("a NaN centre should yield no violations, got %v", vs)
	}
	if vs := Check(nil, zero, one); vs != nil {
		t.Errorf("an empty series should yield no violations, got %v", vs)
	}
}

// Violations arrive ordered by index, and by rule within an index. Conditions
// depend on this to pick out the ones that completed at the newest point.
func TestCheckOrdersViolations(t *testing.T) {
	series := append(repeat(8, 1.5), 4.0)
	vs := Check(series, zero, one)
	if len(vs) < 2 {
		t.Fatalf("expected several violations, got %v", vs)
	}
	for i := 1; i < len(vs); i++ {
		if vs[i].Index < vs[i-1].Index {
			t.Fatalf("violations out of index order: %v", vs)
		}
		if vs[i].Index == vs[i-1].Index && vs[i].Rule <= vs[i-1].Rule {
			t.Fatalf("violations out of rule order within an index: %v", vs)
		}
	}
}

func TestRuleMetadata(t *testing.T) {
	want := map[Rule]int{Rule1: 1, Rule2: 9, Rule3: 6, Rule4: 14, Rule5: 3, Rule6: 5, Rule7: 15, Rule8: 8}
	for r, points := range want {
		if got := r.Points(); got != points {
			t.Errorf("%v.Points() = %d, want %d", r, got, points)
		}
		if !r.Valid() {
			t.Errorf("%v should be valid", r)
		}
	}
	if got := len(AllRules()); got != 8 {
		t.Errorf("AllRules() has %d entries, want 8", got)
	}
	if Rule(0).Valid() || Rule(9).Valid() {
		t.Error("out-of-range rules should not be valid")
	}
	if got := Rule(0).Points(); got != 0 {
		t.Errorf("an unknown rule needs no points, got %d", got)
	}
	if got := Rule3.String(); got != "rule3" {
		t.Errorf("Rule3.String() = %q", got)
	}
	if got := Rule(42).String(); got != "unknown" {
		t.Errorf("Rule(42).String() = %q", got)
	}
}

func TestAllRulesIsACopy(t *testing.T) {
	rs := AllRules()
	rs[0] = Rule8
	if AllRules()[0] != Rule1 {
		t.Error("AllRules returned an aliased slice")
	}
}
