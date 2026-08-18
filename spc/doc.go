// Package spc provides statistical process control conditions for the alarm
// engine: control-chart rules that detect shifts, trends and drift in a
// metric rather than a static threshold being crossed.
//
// The motivating case is a metric with a strong daily cycle. In a trading
// system a ten-fold spike at 09:00 is the market opening, and the same value
// at 11:00 is an incident. A fixed threshold is wrong at every hour except
// the one it was tuned for. A control chart asks a better question: is this
// sample consistent with this metric's own recent behaviour?
//
// Two conditions are exported, both satisfying alarm.Condition:
//
//	spc.Nelson(baseline, ref, rules...)   // the eight Nelson rules
//	spc.EWMA(baseline, ref, lambda, L)    // exponentially weighted moving average
//
// # Layering
//
// Everything except the conditions themselves operates on []float64 and knows
// nothing about windows, engines or events. Mean, StdDev, Median, MAD, the
// Baseline implementations, Check and the EWMA functions are usable, and
// testable, on their own. Only conditions.go imports alarm. That boundary is
// deliberate: it keeps the statistics available to anyone doing manufacturing
// QA, data quality or batch analysis without the alerting engine attached.
//
// Like the parent package, this one imports only the standard library.
//
// # Window truncation: what these conditions remember
//
// alarm.Condition requires implementations to be stateless, so that rules can
// be swapped on a hot reload without carrying stale state. An EWMA, though,
// is recursive, and in principle remembers the whole process history.
//
// These conditions resolve that by recomputing the statistic from the window
// on every evaluation. The window is bounded at alarm.MaxWindowPoints, so a
// full recomputation is O(n) over a small n on a path that already walks the
// window, and the condition holds nothing between calls.
//
// The consequence is not hidden: the statistic's memory is exactly the
// window's length. An EWMA recomputed over the last N observations is not the
// same number as an EWMA carried forward since the process started. For an
// exponentially decaying statistic the difference is bounded by the weight of
// the oldest retained observation, (1-λ)^N, and EWMAMinPoints sizes the
// window so that weight stays under EWMAResidualWeight — one percent. Below
// that point count the statistic is measurably not the one it claims to be,
// which is why MinPoints is derived from lambda rather than chosen.
//
// A statistic that does not decay has no such bound, which is why CUSUM is
// not in this package: recomputed over a truncated window it would silently
// mean something different from the CUSUM its users expect.
//
// # Baselines
//
// Every control chart needs a centre line and a dispersion estimate, and
// where they come from decides everything else. Three are supplied:
//
//   - Fixed, for a metric whose normal range is genuinely known.
//   - Trailing, mean and sample standard deviation over the observations
//     preceding the ones under test.
//   - TrailingRobust, median and MAD·MADScale over the same, for a reference
//     period that may contain spikes — which monitoring data usually does.
//
// The observations under test are never part of their own baseline. A
// sustained shift allowed into its own centre line drags the centre after it
// and inflates the dispersion, and the chart goes blind to precisely what it
// exists to detect. The Baseline interface receives only the reference
// observations, so that exclusion is structural rather than a convention
// somebody has to remember.
//
// A seasonal baseline — compare against the same minute last week — is out of
// scope and cannot be built on alarm.Window, which holds at most
// alarm.MaxWindowPoints observations spanning minutes to hours, not days. The
// interface accommodates one: implement Baseline over a time-series store and
// pass it to either constructor. Nothing here needs to change.
//
// # Sizing the window
//
// Both conditions implement alarm.PointsHinter, and their MinPoints is the
// observations the rules need plus the reference observations the baseline
// needs. A Nelson condition using rule 7 over a Trailing(50) baseline reads
// 65 observations and declares 65. A window sized for only one of the two
// halves yields a condition that is silently and permanently false, which is
// the worst failure mode an alerting library has.
//
// Neither condition implements alarm.SpanHinter. These judgements are
// point-count based and have no meaningful time span to declare; implementing
// the interface anyway would make the engine size windows against a number
// that means nothing.
//
// Both implement alarm.Measurer. Nelson reports the sigma distance of the
// most recent observation and EWMA reports the EWMA statistic, because the
// last raw observation — the engine's default — says almost nothing about a
// rule that fired on a nine-point run.
//
// # Rules 7 and 8
//
// Rule 7, fifteen consecutive points all within one sigma, is usually omitted
// from monitoring implementations and is often the most informative one
// present. In manufacturing it indicates stratification. In monitoring it
// much more often means the baseline is stale: the process became quieter
// than the limits still describe, and the right response is to re-estimate
// the baseline rather than to page anyone.
//
// Rule 8 follows Nelson's definition, which requires the eight points to fall
// on both sides of the centre line. Without that requirement it would report
// an ordinary sustained shift — already covered by rules 2 and 6 — as a
// mixture of two populations.
//
// The rules are Nelson, L. S., "The Shewhart Control Chart — Tests for
// Special Causes", Journal of Quality Technology 16(4), 1984. Nothing here is
// novel; they are a published standard, which is exactly why they are worth
// implementing exactly.
//
// # Arguments
//
// Constructors never return an error and never panic. An argument outside its
// valid range is clamped to the nearest value inside it, and one whose range
// is open at the offending end is replaced by a documented default. The
// engine offers no logger through which a condition could report that it had
// been built unusable, so the alternative to clamping is a condition that is
// silently never true.
package spc
