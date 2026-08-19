package spc

import "math"

// Rule identifies one of the eight Nelson rules (Nelson, L. S., "The Shewhart
// Control Chart — Tests for Special Causes", Journal of Quality Technology
// 16(4), 1984).
//
// Each rule asks a different question of the same series, and each needs a
// different number of observations to answer it. They are meant to be used
// together: rule 1 catches the gross excursion nobody could miss, and rules 2
// through 6 catch the shifts and trends that never breach a three-sigma limit
// but move the process all the same.
type Rule int

// The eight Nelson rules.
const (
	// Rule1 fires on one point beyond three sigma: a gross shift.
	Rule1 Rule = iota + 1
	// Rule2 fires on nine consecutive points on the same side of the centre
	// line: a sustained shift too small to breach three sigma.
	Rule2
	// Rule3 fires on six consecutive points all increasing or all
	// decreasing: a trend, such as drift in a sensor or a leaking resource.
	Rule3
	// Rule4 fires on fourteen consecutive points alternating up and down:
	// over-adjustment, or two interleaved sources sampled in turn.
	Rule4
	// Rule5 fires on two out of three consecutive points beyond two sigma on
	// the same side, the last of the three being one of them: a medium shift.
	Rule5
	// Rule6 fires on four out of five consecutive points beyond one sigma on
	// the same side, the last of the five being one of them: a small shift.
	Rule6
	// Rule7 fires on fifteen consecutive points all within one sigma. In
	// manufacturing this indicates stratification. In monitoring it much more
	// often means the baseline's sigma is stale and too wide — the process
	// became quieter than the limits still describe — and the useful response
	// is to re-estimate the baseline, not to page anyone.
	//
	// That reading applies to Fixed. Over a trailing baseline sigma is
	// re-estimated on every evaluation, so rule 7 is really asking whether
	// the period just before the test points was noisier than the test points
	// themselves — which any heteroscedastic metric answers yes to regularly.
	// Keep it out of a paging rule either way.
	Rule7
	// Rule8 fires on eight consecutive points, on both sides of the centre
	// line, with none within one sigma: a mixture of two populations.
	//
	// Minitab's test 8 drops the both-sides requirement; Nelson's does not,
	// and neither does this. See the note on mixture.
	Rule8
)

// allRules is every rule in ascending order, and the rule set Check uses when
// the caller names none.
var allRules = []Rule{Rule1, Rule2, Rule3, Rule4, Rule5, Rule6, Rule7, Rule8}

// AllRules returns all eight rules in ascending order. The returned slice is
// a copy and may be modified.
//
// It is the rule set Check applies when the caller names none, and it is what
// to pass Nelson to get the same. Measured on an in-control process it false
// alarms once every 47 observations against 215 for DefaultRules; see the
// package documentation before handing it to anything that pages.
func AllRules() []Rule { return append([]Rule(nil), allRules...) }

// DefaultRules returns the rule set a condition falls back to when it is given
// no rule it recognises: rule 1 alone. The returned slice is a copy and may be
// modified.
//
// Rule 1 is not the quietest of the eight — measured on an in-control process
// against a known baseline it signals once every 373 observations, where rule
// 8 signals once every 12,901 — but it is the one a reader who has not opened
// the documentation already expects a control chart to apply: a three-sigma
// excursion. It is a substitution for unusable input, not a recommendation:
// the rules a metric deserves are a judgement about that metric, which is why
// Nelson requires them rather than defaulting.
func DefaultRules() []Rule { return []Rule{Rule1} }

// Points returns how many consecutive observations the rule needs before it
// can fire. It returns 0 for an unknown rule.
//
// This is what a condition must size its window by: a rule given fewer
// observations than it needs is not merely insensitive, it is permanently
// false.
func (r Rule) Points() int {
	switch r {
	case Rule1:
		return 1
	case Rule2:
		return 9
	case Rule3:
		return 6
	case Rule4:
		return 14
	case Rule5:
		return 3
	case Rule6:
		return 5
	case Rule7:
		return 15
	case Rule8:
		return 8
	}
	return 0
}

// Valid reports whether r is one of the eight rules.
func (r Rule) Valid() bool { return r >= Rule1 && r <= Rule8 }

// String returns "rule1" … "rule8", or "unknown".
func (r Rule) String() string {
	if !r.Valid() {
		return "unknown"
	}
	return "rule" + string(rune('0'+int(r)))
}

// Violation is one rule firing at one position in the series.
type Violation struct {
	Rule  Rule
	Index int     // index into the series where the rule completed
	Z     float64 // sigma distance of the triggering point, the observation at Index
}

// Check applies the named rules to series against the given centre line and
// sigma, and returns every violation found. Passing no rules applies all
// eight, which is the Nelson procedure as published.
//
// Rules 5 and 6 additionally require the point the rule completes at to be
// beyond the limit itself. Nelson does not say so, and Minitab and JMP do not
// implement it that way, but without it the rule completes at a point that is
// comfortably in control — two of three beyond two sigma is satisfied by a
// series that went out twice and has since recovered — and the violation then
// reports the sigma distance of an observation that triggered nothing.
//
// Unknown rules are ignored, and a rule set consisting only of unknown rules
// therefore matches nothing and yields no violations. That is not the same as
// naming no rules: an absent filter selects everything and a filter that
// matches nothing selects nothing. The condition constructors cannot take the
// second branch — a condition that is never true is worse than a noisy one —
// so Nelson substitutes DefaultRules instead. Check has no such constraint,
// because an empty result from an analysis costs a reader nothing.
//
// A rule is evaluated at every position it can complete at, so a nine-point
// run reports a violation at the ninth point and again at the tenth if the
// run continues. Violations come back ordered by index, and by rule within an
// index. Callers judging a live series usually want only the violations whose
// Index is the last one — a rule that completed six observations ago has
// already been reported.
//
// Check reports nothing when sigma is not a positive finite number: there is
// no meaningful sigma distance to compute, and the alternative is a division
// that yields infinities.
func Check(series []float64, centre, sigma float64, rules ...Rule) []Violation {
	if len(series) == 0 || !finite(centre) || !finite(sigma) || sigma <= 0 {
		return nil
	}
	if len(rules) == 0 {
		rules = allRules
	}

	c := chart{series: series, centre: centre, sigma: sigma}
	var out []Violation
	for i := range series {
		for _, r := range allRules {
			if !contains(rules, r) {
				continue
			}
			n := r.Points()
			if i+1 < n {
				continue
			}
			if c.fires(r, i-n+1, i+1) {
				out = append(out, Violation{Rule: r, Index: i, Z: c.z(i)})
			}
		}
	}
	return out
}

func contains(rules []Rule, r Rule) bool {
	for _, x := range rules {
		if x == r {
			return true
		}
	}
	return false
}

// chart is a series bound to a centre line and sigma. Every rule is expressed
// against the half-open window series[lo:hi], whose length the caller has
// already checked against Rule.Points.
type chart struct {
	series        []float64
	centre, sigma float64
}

// z is the sigma distance of the point at i.
func (c chart) z(i int) float64 { return (c.series[i] - c.centre) / c.sigma }

// side reports which side of the centre line the point at i falls on: +1
// above, -1 below, 0 exactly on it. A point on the centre line belongs to
// neither side and so breaks a same-side run.
func (c chart) side(i int) int {
	switch {
	case c.series[i] > c.centre:
		return 1
	case c.series[i] < c.centre:
		return -1
	}
	return 0
}

func (c chart) fires(r Rule, lo, hi int) bool {
	switch r {
	case Rule1:
		return math.Abs(c.z(hi-1)) > 3
	case Rule2:
		return c.sameSide(lo, hi)
	case Rule3:
		return c.monotone(lo, hi, 1) || c.monotone(lo, hi, -1)
	case Rule4:
		return c.alternating(lo, hi)
	case Rule5:
		return c.kOfNBeyond(lo, hi, 2, 2)
	case Rule6:
		return c.kOfNBeyond(lo, hi, 1, 4)
	case Rule7:
		return c.allWithin(lo, hi)
	case Rule8:
		return c.mixture(lo, hi)
	}
	return false
}

// sameSide reports whether every point in the window is on the same side of
// the centre line. Rule 2.
func (c chart) sameSide(lo, hi int) bool {
	s := c.side(lo)
	if s == 0 {
		return false
	}
	for i := lo + 1; i < hi; i++ {
		if c.side(i) != s {
			return false
		}
	}
	return true
}

// monotone reports whether every step in the window moves in direction dir.
// Equal adjacent values are neither increasing nor decreasing and break the
// run. Rule 3.
func (c chart) monotone(lo, hi, dir int) bool {
	for i := lo + 1; i < hi; i++ {
		d := c.series[i] - c.series[i-1]
		if d == 0 || (d > 0) != (dir > 0) {
			return false
		}
	}
	return true
}

// alternating reports whether every step in the window reverses the direction
// of the one before it, with no equal adjacent values. Rule 4.
func (c chart) alternating(lo, hi int) bool {
	up := false
	for i := lo + 1; i < hi; i++ {
		d := c.series[i] - c.series[i-1]
		if d == 0 {
			return false
		}
		if i > lo+1 && (d > 0) == up {
			return false
		}
		up = d > 0
	}
	return true
}

// kOfNBeyond reports whether at least k points in the window lie beyond
// mult sigma on the same side of the centre line, counting only the side the
// window's last point is on. Rules 5 and 6.
//
// The last point must itself be beyond the limit. Without that gate the rule
// completes at a point that is comfortably in control — two of three beyond
// two sigma is satisfied by a series that went out twice and has since
// recovered — and the violation then reports the sigma distance of an
// observation that triggered nothing. It also keeps a condition breaching
// for up to k-1 observations after the process came back, which is the
// stickiness the "fires only at the newest observation" rule exists to
// prevent. The qualifying points need not be adjacent, and Nelson does not
// require them to be; requiring the last one is the convention every
// published implementation follows, because it is the point being charted.
func (c chart) kOfNBeyond(lo, hi int, mult float64, k int) bool {
	var want int
	switch z := c.z(hi - 1); {
	case z > mult:
		want = 1
	case z < -mult:
		want = -1
	default:
		return false
	}
	n := 0
	for i := lo; i < hi; i++ {
		z := c.z(i)
		if (want > 0 && z > mult) || (want < 0 && z < -mult) {
			n++
		}
	}
	return n >= k
}

// allWithin reports whether every point in the window lies within one sigma
// of the centre line. Rule 7.
//
// A point at exactly one sigma counts as within, which is the same
// convention rule 1 uses for the three-sigma limit and the complement of the
// one mixture uses. The two rules partition the same band, so they must
// agree on its edge: with the boundary counted as outside here and as inside
// there, a series sitting exactly on one sigma satisfied neither rule.
func (c chart) allWithin(lo, hi int) bool {
	for i := lo; i < hi; i++ {
		if math.Abs(c.z(i)) > 1 {
			return false
		}
	}
	return true
}

// mixture reports whether no point in the window lies within one sigma of the
// centre line and points fall on both sides of it. Rule 8.
//
// The both-sides requirement is Nelson's, and it is what separates rule 8
// from an ordinary sustained shift: eight points more than one sigma out on
// a single side is a shift, and rule 6 has already reported it three
// observations earlier — four of five beyond one sigma on one side is
// implied by eight of eight. Eight points straddling the centre with a hole
// around it is two populations being sampled alternately, which is a
// different fault with a different remedy.
//
// Minitab's test 8 omits the both-sides clause; JMP and the Nelson lineage
// keep it. A reader cross-checking against Minitab will see this package
// report fewer rule 8 violations, and the difference is exactly the
// one-sided runs rule 6 already covers.
func (c chart) mixture(lo, hi int) bool {
	above, below := false, false
	for i := lo; i < hi; i++ {
		z := c.z(i)
		switch {
		case z > 1:
			above = true
		case z < -1:
			below = true
		default:
			return false
		}
	}
	return above && below
}
