// Package spc provides statistical process control conditions for the alarm
// engine: control-chart rules that detect shifts, trends and drift in a
// metric rather than a static threshold being crossed.
//
// The motivating case is a metric with a strong daily cycle. In a trading
// system a ten-fold spike at 09:00 is the market opening, and the same value
// at 11:00 is an incident. A fixed threshold is wrong at every hour except
// the one it was tuned for. A control chart asks a different question: is
// this sample consistent with this metric's own recent behaviour?
//
// Be clear about what that buys and what it does not. A trailing baseline
// detects *changes*, never *levels*, so it needs no per-hour tuning and it
// works on a metric whose normal range nobody wrote down. But the market
// opening is a change too. These conditions will report it, and they cannot
// tell it apart from an incident of the same shape; separating "this happens
// every day at 09:00" from "this has never happened before" requires a
// seasonal baseline, which is out of scope below. What this package removes
// is the need to know the level in advance. What it does not remove is the
// need to silence changes you expect.
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
// # How long a breach lasts
//
// This is the most important operational property here, and it is a direct
// consequence of the trailing baselines: a step change enters the reference
// period roughly `test` observations after it starts, and has taken the
// reference period over entirely after `ref + test`. At that point the
// incident is the new normal, the chart reads in control, and the condition
// goes false — with the metric still at its shifted level.
//
// So a breach against Trailing or TrailingRobust lasts on the order of `ref`
// observations, whatever the incident does. In wall-clock terms that is `ref`
// times the sampling interval. The engine will then emit a Resolve, and its
// Event.Value will be a sigma distance near zero, measured against a centre
// line that has followed the incident. It is a false all-clear, and it will
// look authoritative.
//
// Two consequences for anyone wiring this to a pager:
//
//   - Set Rule.ClearFor to at least as long as you expect an incident to
//     last. It is what holds the alert up after the condition goes quiet, and
//     for these conditions it is a requirement rather than a refinement.
//   - Do not template a recovery notification off Event.Value. For a step
//     change it describes the baseline, not the process.
//
// Fixed does not have this property, because its centre line cannot move. If
// what you need is "tell me while the metric is bad" rather than "tell me
// when the metric changes", Fixed — or an ordinary alarm.Threshold — is the
// right tool and a trailing baseline is the wrong one.
//
// # Sampling, gaps and time
//
// These conditions never read Point.Time. Every judgement is a point count,
// so `ref` is "the preceding 50 observations" and not "the preceding eight
// minutes". The mapping between the two is the caller's, and it is silent: if
// a scrape degrades from 10s to 5m, Trailing(50) quietly stops describing
// eight minutes and starts describing four hours, which for a metric with a
// daily cycle is a different process. Set Rule.StaleAfter to a small multiple
// of the expected sampling interval so a degraded feed becomes Stale rather
// than reinterpreting the reference period.
//
// After any gap the window must refill before the condition can be true
// again, which takes MinPoints observations — 65 of them for rule 7 over
// Trailing(50). The engine's StaleRecover event fires on the first returning
// observation, well before that. There is no event for "back, but not yet
// able to judge".
//
// Leave Rule.KeepWindowOnStale at its default of false. Because the
// conditions ignore timestamps, keeping the window across a gap builds a
// chart whose reference period is the process before the gap and whose test
// points are the process after it — which reports every deploy as a shift.
//
// # For and Fingerprint
//
// Rule.For must be shorter than `ref` times the sampling interval. That
// product is the entire span for which one of these conditions can be
// continuously true, so a longer For yields a rule that never fires at all,
// and nothing warns about it. This is worth care because For is the obvious
// knob to reach for against the false-alarm rate below.
//
// Rule.Fingerprint is unnecessary while For is zero or less: observations are
// raw measurements, all judgement lives in the condition, and a hot reload
// simply re-judges the existing window. Once For is positive it is required,
// for the reason the parent package documents — the pending start time is
// accrued state, so a condition swap would otherwise inherit elapsed time
// from the condition it replaced. Encode the baseline, the rule set, lambda
// and L.
//
// # How often these fire when nothing is wrong
//
// Every rule has a false-alarm rate, and running several multiplies it.
// Champ and Woodall (Technometrics 29(4), 1987) give the exact result for the
// four standard Western Electric rules run together: an in-control ARL of
// 91.75, against 370.4 for a three-sigma limit alone. Nelson's eight include
// those four and four more.
//
// Measured over 400,000 in-control observations of a normal process, judged
// the way these conditions judge — a rule breaches when it completes at the
// newest observation, evaluated on every observation:
//
//	rule 1, Fixed baseline           one false breach every 373 observations
//	rule 1, Trailing(50)             one every 222
//	rule 2, Trailing(50)             one every 170
//	rule 7, Trailing(50)             one every 236
//	rules 1 and 2, Trailing(50)      one every  96
//	all eight, Trailing(50)          one every  31
//
// At a ten-second sampling interval, all eight rules is a false breach every
// five minutes, per key. Naming no rules enables all eight, so the shortest
// call is also the noisiest; name the rules you want. Rule 7 in particular is
// a baseline-maintenance signal — its own documentation says the useful
// response is to re-estimate, not to page — and it does not belong in a
// paging rule alongside rule 1.
//
// Two further effects visible in that table. A trailing baseline is itself
// estimated, and the estimation error adds variance the limits do not account
// for, which is why rule 1 over Trailing(50) false-alarms two-thirds more
// often than the textbook 370.4 the same rule achieves against a known
// baseline; Quesenberry (Journal of Quality Technology 25(4), 1993) puts the
// reference size needed for individuals limits to behave like known-parameter
// limits at the order of 300 observations. And the rules assume independent
// observations. Monitoring data sampled at second-to-minute granularity is
// usually autocorrelated, which inflates the run-based rules (2, 3, 6, 7) and
// EWMA considerably beyond the numbers above. Sampling at an interval longer
// than the metric's autocorrelation time is the practical mitigation.
//
// # Metrics these do not work on
//
// A baseline that cannot produce a dispersion estimate reports false, and the
// condition is then never true. Nothing announces this: the engine warns at
// SetRules about a condition whose MinPoints cannot fit in a window, but a
// baseline that declines to estimate on every evaluation looks exactly like a
// process that is in control. Two common metric shapes do this:
//
//   - A metric that is usually constant. StdDev of a constant reference
//     period is zero, so Trailing is dead on a queue depth pinned at zero, a
//     healthy error counter, or a saturated gauge.
//   - A low-cardinality integer metric. MAD is zero as soon as more than half
//     the reference observations share a value, so TrailingRobust is dead on
//     a mostly-zero error count — which is one of the most commonly alerted
//     metric shapes there is.
//
// These conditions are for metrics that are always noisy. For a metric that
// is usually flat and occasionally not, an alarm.Threshold is the right tool,
// and it is a better one. The String method on both conditions reports the
// effective configuration, which is the quickest way to check what a
// constructor actually built.
//
// # Arguments
//
// Constructors never return an error and never panic. An argument outside its
// valid range is clamped to the nearest value inside it, and one whose range
// is open at the offending end is replaced by a documented default. The
// alternative is a condition that is never true, which is the failure mode
// this package is organised to avoid.
//
// Because a constructor returns an alarm.Condition, the value it produced is
// otherwise opaque. Both conditions implement String and report their
// effective configuration, including anything that was clamped; log the rule
// you built, or print it in a test, and a mistyped lambda is visible.
//
// # Reporting which rule fired
//
// alarm.Event carries a single float64, so a condition that judges several
// rules at once cannot say which of them fired. Give each rule its own
// alarm.Level rather than bundling them into one condition:
//
//	Levels: []alarm.Level{
//		{Severity: alarm.SeverityError, Condition: spc.Nelson(b, 50, spc.Rule1)},
//		{Severity: alarm.SeverityWarn,  Condition: spc.Nelson(b, 50, spc.Rule2)},
//	}
//
// The engine sizes a rule's window from the largest MinPoints among all its
// levels, so the levels share one window rather than one each, and Severity
// then identifies what fired — which is also the granularity triage wants,
// since a gross excursion and a slow trend do not deserve the same response.
// The limitation is that only the highest level that holds is reported, so
// order the severities the way you would order the pages.
//
// The centre line and sigma a breach was judged against are not recoverable
// from the event at all. A sigma distance is scale-free by construction, and
// three sigma means something different when sigma is two milliseconds and
// when it is four hundred. Attach whatever context the handler needs with
// alarm.ObserveMeta.
package spc
