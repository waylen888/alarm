# alarm

An embeddable alerting state machine for Go: it owns the "condition met → fire → resolve"
lifecycle so your code only has to answer two questions — what counts as an observation,
and what to do when an event comes out.

[繁體中文說明](README.zh-TW.md)

```go
import "github.com/waylen888/alarm"
```

Zero dependencies. The package imports only the standard library, and always will.

## Contents

- [What it is](#what-it-is)
- [Quick start](#quick-start)
- [Core concepts](#core-concepts)
- [State machine](#state-machine)
- [API](#api)
- [Built-in conditions](#built-in-conditions)
- [Statistical process control: the spc subpackage](#statistical-process-control-the-spc-subpackage)
- [Escalate and Exit](#escalate-and-exit)
- [Hot reload and Fingerprint](#hot-reload-and-fingerprint)
- [Data availability: Stale, Vanish, MaxKeys](#data-availability-stale-vanish-maxkeys)
- [Event.Meta: self-contained events](#eventmeta-self-contained-events)
- [Concurrency and lifecycle](#concurrency-and-lifecycle)
- [Window capacity and limits](#window-capacity-and-limits)
- [Production usage](#production-usage)
- [Limitations](#limitations)
- [Design invariants](#design-invariants)
- [License](#license)

---

## What it is

A library, not a server. There is no scrape loop, no query language, no notification
delivery, no storage. You feed it observations; it decides when a key transitions between
OK, Pending, Firing, Stale and gone, and calls your handler with an `Event`.

The comparison point is Prometheus Alertmanager or Grafana alerting. Those are excellent at
what they do: they run as infrastructure, evaluate rules against a time-series database, and
route notifications for a whole fleet. This package is for the other case — when you need
alert state machine semantics *inside your own process*, over data you already have in
memory, without standing up or depending on external infrastructure. An agent that watches
its own host, a network prober, a log tailer, a service that alerts on its own internal
counters. If your data already lives in Prometheus, use Alertmanager.

What the engine handles:

| The engine does | The engine does not |
| --- | --- |
| Condition evaluation, state transitions, de-duplication | Message text, notification payloads |
| Duration gates (`For`), recovery damping (`ClearFor`) | Alert history persistence, delivery channels |
| Multi-level escalation and de-escalation | How a threshold compares (a closure you supply) |
| Data-gap (`Stale`) and disappearance (`Vanish`) detection | Sampling, scraping, parsing |
| Per rule+key re-notification throttling (`Reminder`) | Cross-rule notification silencing |
| Cardinality caps (`MaxKeys`) | Recipient resolution, notification batching |

Deliberate non-goals:

- **No global singleton.** Create one `Engine` per subsystem, each with its own handler.
  Whether a given subsystem uses a package-level instance is that subsystem's business —
  a package-level engine is a reasonable way to keep alert state alive across reconnects.
- **One key carries one scalar series.** Encode multi-dimensional metrics into a single
  number in the caller (packet loss and latency folded into a 0/1/2 severity), or split them
  into separate rules sharing the same key space.
- **No notification-layer throttling.** A silence window that spans several rules, or that
  suppresses even the first alert, is not equivalent to per-rule `ClearFor` + `Reminder`.
  It belongs in the caller.

---

## Quick start

```go
// 1. Create the engine. The handler decides what an event becomes.
engine := alarm.New(func(ev alarm.Event) {
    switch ev.Kind {
    case alarm.EventFire:
        sendAlert(ev.Key, ev.Value)
    case alarm.EventResolve:
        sendResolved(ev.Key)
    }
})

// 2. Install rules. Safe to call again whenever configuration changes.
engine.SetRules([]alarm.Rule{{
    ID: "cpu-high",
    Levels: []alarm.Level{{
        Severity:  alarm.SeverityError,
        Condition: alarm.ConsecutiveN(3, func(v float64) bool { return v > 90 }),
    }},
    StaleAfter:  -1, // no data-gap detection: state freezes when collection stops
    VanishAfter: -1,
}})

// 3. Feed observations. The key separates entities tracked by the same rule.
engine.Observe("cpu-high", agentID, cpuPercent, time.Now())
```

If a rule needs time to advance on its own — window decay, `Stale`/`Vanish` detection,
`Reminder` re-notification — give the engine a clock source:

```go
go engine.Run(ctx, 10*time.Second) // or call engine.Tick(now) from your existing loop
```

See [`example_test.go`](example_test.go) for complete runnable examples.

---

## Core concepts

### Rule

One rule tracks any number of **keys** (agents, series, links). Each key runs the state
machine independently.

| Field | Type | Meaning |
| --- | --- | --- |
| `ID` | `string` | Rule identity. `SetRules` matches on this. |
| `Levels` | `[]Level` | At least one. Evaluated highest severity first; the highest one that holds wins. |
| `For` | `time.Duration` | How long the condition must hold before Firing. `<=0` fires immediately. |
| `Fingerprint` | `string` | Semantic fingerprint of the conditions. A change rebuilds the rule. See [Hot reload](#hot-reload-and-fingerprint). |
| `Reminder` | `time.Duration` | Re-notification interval while Firing. `<=0` disables. |
| `ClearFor` | `time.Duration` | How long the condition must stay clear before Resolve (flap damping). `<=0` resolves immediately. |
| `Clear` | `Condition` | Count-based recovery condition. When set, `ClearFor` is ignored. |
| `StaleAfter` | `time.Duration` | Idle time before a key goes Stale. `0` = engine default (off); `<0` disables. |
| `VanishAfter` | `time.Duration` | Idle time before a key's state is dropped. `0` = engine default (1h); `<0` disables. |
| `MaxKeys` | `int` | Cardinality cap for this rule. `0` = engine default (1000). |
| `KeepWindowOnStale` | `bool` | Keep the observation window when the key goes Stale. Default is to clear it. |

### Level

```go
Levels: []alarm.Level{
    {Severity: alarm.SeverityError, Condition: errCond, Escalate: escCond, Exit: exitCond},
    {Severity: alarm.SeverityWarn,  Condition: warnCond},
}
```

Three severities exist: `SeverityInfo`, `SeverityWarn`, `SeverityError`. A single-level alert
just gives one `Level`. `Escalate` and `Exit` are described in
[Escalate and Exit](#escalate-and-exit) — read that section before using multiple levels.

### Condition and Window

A `Condition` is a **stateless** predicate whose only input is that key's observation
window. All state lives in the engine, which is what makes conditions safe to swap on a hot
reload.

```go
type Condition interface{ Breach(w Window) bool }

type Window interface {
    Last() (Point, bool)                       // most recent observation
    LastN(n int) []Point                       // most recent n, oldest first
    Points(since time.Duration) []Point        // observations within since
    Count(since time.Duration) int             // how many within since
    Delta(since time.Duration) (float64, bool) // counter delta; a decrease is treated as a reset
}
```

`since` is always measured backwards from the moment of evaluation, so time-window
conditions decay naturally and are released by `Tick`.

### Event

The engine's only output.

```go
type Event struct {
    RuleID   string
    Key      string
    Kind     EventKind
    Severity Severity
    State    State     // state after the transition
    Value    float64   // measured value (count, rate, or last observation, per condition)
    Since    time.Time // start time of the state being reported
    At       time.Time
    Meta     any       // whatever ObserveMeta attached, handed back verbatim
}
```

---

## State machine

```
              condition holds        held for For
      OK ─────────────────────► Pending ─────────────► Firing
      ▲                            │                     │
      │  condition clears          │                     ├─► Reminder (every Reminder)
      ├─  before For elapses ──────┘                     ├─► Escalate / Deescalate
      │       (silent)                                   │
      └──────────────── Resolve ─────────────────────────┘
                (ClearFor elapsed, or Clear holds)

  any state ──── idle > StaleAfter ────► Stale ──── data returns ────► pre-gap state restored
                                           │                            (StaleRecover)
  any state ──── idle > VanishAfter ───────┴────────────────────────► state dropped
                                                          (Vanish emitted only if it was Firing)
```

| State | Meaning |
| --- | --- |
| `StateOK` | Healthy, or never breached |
| `StatePending` | Condition holds but `For` has not elapsed. Silent — no event |
| `StateFiring` | Alerting |
| `StateStale` | Idle longer than `StaleAfter`; the data source went quiet |

| EventKind | When |
| --- | --- |
| `EventFire` | First breach, after `For` is satisfied. `Since` is the time the condition first held |
| `EventEscalate` / `EventDeescalate` | Severity raised / lowered while Firing |
| `EventReminder` | Every `Reminder` while Firing |
| `EventResolve` | Condition cleared (after `ClearFor`, or once `Clear` holds) |
| `EventStale` | Idle longer than `StaleAfter`. `Since` is the last observation time |
| `EventStaleRecover` | Data returned. `Since` is the start of the restored state |
| `EventVanish` | State dropped after `VanishAfter`. **Emitted only for keys that were Firing**, and implies resolved |

A key that was never Firing disappears silently. That is the rule throughout: no alert, no
event.

---

## API

Method signatures and their full semantics live in the godoc, which is the single source of
truth: **[pkg.go.dev/github.com/waylen888/alarm](https://pkg.go.dev/github.com/waylen888/alarm)**.

---

## Built-in conditions

| Condition | Semantics | Points needed |
| --- | --- | --- |
| `Threshold(judge)` | The most recent observation satisfies `judge` | 1 |
| `ConsecutiveN(n, judge)` | The last n observations all satisfy `judge`; fewer than n never breaches | n |
| `AnyN(n, judge)` | Any of the last n satisfies `judge`; fewer than n never breaches | n |
| `ConsecutiveDeltaN(n, judge)` | The last n adjacent deltas all satisfy `judge` (counter increments) | **n+1** |
| `CountInWindow(n, window)` | At least n observations inside `window` (log-frequency alerting) | n, and declares a time span |
| `RateInWindow(window, judge)` | A counter's per-second increment over `window` satisfies `judge`, computed over the actual elapsed time between the first and last point in the window | ≥2 in the window; capacity starts at `DefaultMinPoints` (64) and grows with the span |
| `All(cs...)` / `Any(cs...)` | Conjunction / disjunction | max over sub-conditions |

Threshold semantics always arrive as a closure, so the caller keeps one source of truth for
what "breached" means:

```go
alarm.Threshold(func(v float64) bool { return v > limit })
```

`ConsecutiveDeltaN` subtracts directly and does **not** compensate for counter resets; the
negative delta a reset produces is left for `judge` to interpret. (`Window.Delta`, used by
`RateInWindow`, does treat a decrease as a reset.)

### Custom conditions

`Condition` is the extension point. Implement it in your own package and the engine treats
it exactly like a built-in:

```go
type Condition interface{ Breach(w Window) bool }
```

Three further interfaces are optional. Implement the ones that apply, and the engine sizes
the window and reports the value accordingly:

| Interface | Method | Tells the engine | Default if absent |
| --- | --- | --- | --- |
| `PointsHinter` | `MinPoints() int` | How many observations the judgement needs | `DefaultMinPoints` (64) |
| `SpanHinter` | `MinSpan() time.Duration` | What time span it covers; the window grows to keep it | no span |
| `Measurer` | `Measure(w Window) float64` | What to report in `Event.Value` | the last observed value |

Declaring them matters. A condition that inspects 200 samples but declares nothing gets a
window of `DefaultMinPoints` (64) and can never breach; a time-based condition that declares
no span gets silently truncated by the point cap once the window fills.

```go
// Breaches when the mean of the last n observations exceeds limit.
type meanOver struct {
    n     int
    limit float64
}

func (c meanOver) Breach(w alarm.Window) bool { return c.mean(w) > c.limit }

func (c meanOver) MinPoints() int { return c.n }

func (c meanOver) Measure(w alarm.Window) float64 { return c.mean(w) } // report the mean, not the last sample

func (c meanOver) mean(w alarm.Window) float64 {
    pts := w.LastN(c.n)
    if len(pts) < c.n {
        return 0
    }
    var sum float64
    for _, p := range pts {
        sum += p.Value
    }
    return sum / float64(len(pts))
}
```

Condition implementations **must be stateless**. All state lives in the engine, which is
what allows conditions to be hot-swapped on reload.

---

## Statistical process control: the spc subpackage

```go
import "github.com/waylen888/alarm/spc"
```

A threshold is the wrong tool for a metric with a strong daily cycle. In a trading system
a ten-fold spike at 09:00 is the market opening and the same value at 11:00 is an
incident; a fixed limit is wrong at every hour except the one it was tuned for.
`spc` supplies control-chart conditions that ask a different question — is this sample
consistent with this metric's own recent behaviour?

Be clear about what that buys. A trailing baseline detects *changes*, never *levels*, so
it needs no per-hour tuning and works on a metric whose normal range nobody wrote down.
But the market opening is a change too, and these conditions report it. What goes away is
having to know the level in advance; what does not is having to silence the changes you
expect.

Two conditions, both ordinary `alarm.Condition` values:

```go
spc.Nelson(spc.TrailingRange(50), 50, []spc.Rule{spc.Rule1}) // one point beyond three sigma
spc.EWMA(spc.Trailing(50), 50, 0.2, 3)                       // a small sustained shift
```

The eight [Nelson rules](https://en.wikipedia.org/wiki/Nelson_rules) (Nelson, 1984) cover
gross excursions, sustained shifts, trends, over-adjustment and mixtures. EWMA covers the
small sustained shift that rule 1 never sees and rule 2 sees nine samples late. The centre
line and dispersion come from a `Baseline`: `Fixed` for a known normal range, `Trailing`
for mean and standard deviation over the preceding observations, `TrailingRobust` for
median and MAD when the reference period may contain spikes, `TrailingRange` for mean and
mean moving range when it drifts.

The last two answer opposite problems and neither answers both. A standard deviation and a
MAD are global measures of dispersion, so drift through the reference period inflates them
and hides the shift that follows; a moving range is local and cannot see the level, but one
outlier contributes two large ranges to it. On a reference period whose level climbs while
its scatter does not, a shift four units past where it ended reads 2.2σ to `Trailing`, 1.8σ
to `TrailingRobust` and 19.2σ to `TrailingRange`; with one large outlier in a steady
reference instead, the same three read 0.6σ, 4.1σ and 1.3σ. Pick by which the metric has.

Worth knowing before reaching for it:

- **A breach against a trailing baseline lasts about `ref` observations.** A sustained
  shift enters its own reference period as it scrolls, and once it has taken the period
  over, the chart reads in control with the metric still shifted — so the alert resolves
  in the middle of the incident, carrying a near-zero sigma distance as its evidence. Set
  `ClearFor` to at least as long as you expect an incident to last, and do not template a
  recovery message off `Event.Value`. `Fixed` has no such behaviour.
- **The rules are a required argument.** There is no default, because no rule set is quiet
  enough to be safe as one and which rules a metric deserves is a judgement about that
  metric. `spc.AllRules()` gives the published procedure and false-alarms once every 47
  observations on an in-control process, against 215 for rule 1 alone; rule 7 is a
  baseline-maintenance signal that does not belong in a paging rule at all. No rule set
  makes this safe for a pager by itself — see the measured table in the package
  documentation. Give each rule its own `alarm.Level`: `Event` carries a single float, so
  bundling several rules into one condition loses which of them fired, while levels share
  one window and let `Severity` say so.
- **The observations under test are never part of their own baseline.** The `Baseline`
  interface only ever receives the reference observations, so the exclusion is structural
  on any single evaluation.
- **These are point counts, not durations.** Nothing reads the timestamps, so `ref` means
  "the preceding 50 observations". `For` must be shorter than `ref` times the sampling
  interval or the rule can never fire; leave `KeepWindowOnStale` false.
- **`MinPoints` counts both halves.** Rule 7 over a `Trailing(50)` baseline reads 65
  observations and declares 65. The subpackage handles this; a hand-rolled condition that
  forgets it is silently never true.
- **The metric has to be noisy.** A baseline that cannot estimate a dispersion leaves the
  condition silently never true — which a constant queue depth does to `Trailing`, and a
  mostly-zero integer counter does to `TrailingRobust`. Use a threshold for those.

The statistics layer — `Mean`, `StdDev`, `Median`, `MAD`, the baselines, `Check`,
`EWMAStat` — operates on `[]float64` and does not import `alarm`. It is usable on its own.
Zero dependencies, same as the parent.

CUSUM and seasonal baselines are deliberately not here; see the
[package documentation](https://pkg.go.dev/github.com/waylen888/alarm/spc) for why, and
for the measured false-alarm rates.

---

## Escalate and Exit

This is the part worth reading carefully. Multi-level alerting has a failure mode that is
easy to miss and unpleasant to debug, and the two guard conditions exist to close it.

### The problem

Levels are evaluated highest severity first and the highest one that holds wins. That is the
right rule for the *initial* classification. It is the wrong rule for stepping an alert
*down*, because in almost every real ruleset **a lower level's condition is implied by the
samples that satisfied the higher level**.

Concretely. A link monitor grades each probe into `0` (healthy), `1` (degraded), `2` (down),
and has two levels:

```go
warn:  ConsecutiveN(2, func(v float64) bool { return v >= 1 })
error: ConsecutiveN(2, func(v float64) bool { return v >= 2 })
```

Every sample that satisfies `error` (`v >= 2`) also satisfies `warn` (`v >= 1`). Now watch a
link that is down and starts recovering:

| Sample | Window (last 2) | `error` holds? | `warn` holds? | Highest level |
| --- | --- | --- | --- | --- |
| `2` | `[2]` | no (needs 2 points) | no | — |
| `2` | `[2, 2]` | **yes** | yes | error → **Fire at error** |
| `1` | `[2, 1]` | no | **yes** | warn → **Deescalate to warn** |

That last row is the bug. A single degraded sample stepped the alert down from error to
warn — not because the link recovered, but because the *previous, still-down* sample is
itself `>= 1` and helped satisfy the warn condition. One flapping probe and the alert
oscillates between error and warn, notifying on every step.

The naive fixes are all worse. Making the levels mutually exclusive (`warn` = `v >= 1 && v < 2`)
breaks the initial classification, because now a genuinely down link that has only produced
one `2` so far matches neither level. Raising `ConsecutiveN` on `warn` desensitises the warn
alert itself. The asymmetry is real: **the condition for entering a level and the condition
for leaving it are different conditions**, and a ruleset that conflates them will misbehave.

### Exit

`Exit` is the de-escalation guard, declared on the level being *left*. While Firing at that
level, a lower level becoming true is no longer sufficient to step down — `Exit` must hold
as well:

```go
{
    Severity:  alarm.SeverityError,
    Condition: alarm.ConsecutiveN(2, func(v float64) bool { return v >= 2 }),
    Exit:      alarm.ConsecutiveN(2, func(v float64) bool { return v < 2 }),
},
{
    Severity:  alarm.SeverityWarn,
    Condition: alarm.ConsecutiveN(2, func(v float64) bool { return v >= 1 }),
},
```

Replaying the same sequence:

| Sample | Window (last 2) | Highest level | `error`'s `Exit` holds? | Result |
| --- | --- | --- | --- | --- |
| `2` | `[2]` | — | — | — |
| `2` | `[2, 2]` | error | — | **Fire at error** |
| `1` | `[2, 1]` | warn | **no** (`2` is not `< 2`) | stays error, no event |
| `1` | `[1, 1]` | warn | **yes** | **Deescalate to warn** |

The alert now steps down on evidence that the higher level has actually ended, not on an
artefact of overlapping conditions. This is the case in `ExampleLevel_exitGuard`.

### Escalate

`Escalate` is the symmetric guard, declared on the level being *entered*. While Firing at a
lower level, this level's `Condition` becoming true is no longer sufficient to step up —
`Escalate` must hold as well.

It exists because "classify on first fire" and "escalate an existing alert" also want
different conditions. A typical link rule fires after n consecutive bad samples and grades
the alert by the *current* sample, so a single `2` among degraded samples is enough to open
an error-level alert. But escalating an alert that is already warn should demand more:
n consecutive samples at error level, where any lower sample resets the streak. `Condition`
expresses the first, `Escalate` expresses the second:

```go
{
    Severity:  alarm.SeverityError,
    Condition: alarm.Threshold(func(v float64) bool { return v >= 2 }),
    Escalate:  alarm.ConsecutiveN(3, func(v float64) bool { return v >= 2 }),
}
```

`Escalate` affects only the escalation path while Firing. The initial classification
(`OK → Firing`) and `Pending` ignore it — that is what makes the split useful rather than
merely stricter.

Both fields are optional; `nil` keeps the default behaviour, where the highest holding level
wins outright.

---

## Hot reload and Fingerprint

`SetRules` matches on **ID**. A rule that is no longer present has its state cleared
silently, following the convention that deleting or disabling a rule does not emit a
resolved event. A rule that keeps its ID keeps its per-key state, except in two cases where
the whole rule is rebuilt:

1. The conditions now need a **larger window** — an old window holding too few points could
   never satisfy the new `ConsecutiveN`.
2. **`Fingerprint` changed.**

A rebuild invalidates only *condition-semantic* state (window, Pending, Firing). Each key's
**data availability** — its last observation time and Stale tracking — is carried into the
new runtime. Otherwise editing a threshold would silently flip an in-progress data gap from
"no data" to "normal", and since a disconnected key produces no observations it could never
be recreated by `Observe`, so the gap would go undetected forever.

Growing the required **time span** deliberately does *not* rebuild. Observations older than
the span were never retained, so a rebuild cannot recover them; it would only additionally
discard Firing state, making an alert disappear silently. Reusing the existing window merely
underestimates for a while and heals itself as later observations grow it — the same
situation as a rule whose window has not filled after a cold start.

### When you need a Fingerprint

The engine cannot see inside a `Condition` closure, nor how observed values are encoded.
These cases **require** the semantic inputs to be encoded into the fingerprint:

- **The encoding of observed values depends on configuration.** If samples are graded into
  `0/1/2` against a threshold *before* they are observed, changing that threshold invalidates
  the old encodings in the window — two `warn` samples from the old configuration plus one
  from the new must not be allowed to satisfy a `ConsecutiveN` together.
- **Which values get observed at all depends on configuration.** A syslog query determines
  what counts as a hit; change the query and the history means something else.
- **`For > 0` and the decision threshold is mutable.** The pending start time is accrued
  state — "the condition has held since T" — and is *not* recomputed from the window. Keeping
  it across a condition swap lets the new condition borrow the old one's elapsed time: change
  the threshold from `>10` to `>100`, and a key already pending for four minutes only needs
  one more minute to satisfy a five-minute `For`. Every setting that can change the verdict
  (operator, threshold, …) must be encoded.

You **do not** need a fingerprint when observations are raw measurements, all judgement lives
inside the closure, and `For <= 0`. `SetRules` swaps in the new condition and the raw history
in the window is simply re-judged by it.

Changing `For` itself does **not** belong in the fingerprint. "The condition has held for X"
is independent of the value of `For`; comparing the existing start time against the new value
is the correct answer (shortening `For` fires immediately, lengthening it keeps waiting).
Rebuilding would only discard an alert in progress.

A fingerprint must be a **deterministic serialisation** of the configuration — sort your maps.

```go
// A frequency rule: count, window and query are all semantic inputs.
func conditionFingerprint(cfg ObserverConfig) string {
    q, _ := json.Marshal(cfg.Query)
    return fmt.Sprintf("%d|%s|%s", cfg.Times, cfg.Window, q)
}

// A metric rule whose observations are raw values: only the judge's inputs matter.
func (r MetricAlarm) ConditionFingerprint() string {
    return string(r.Op) + "|" + strconv.FormatFloat(r.Value, 'g', -1, 64)
}
```

---

## Data availability: Stale, Vanish, MaxKeys

"Condition semantics" and "data availability" are orthogonal dimensions, and the engine
keeps them apart.

| Setting | Meaning |
| --- | --- |
| `StaleAfter: -1` | No data-gap detection. State freezes when collection stops — right when *not* collecting is normal or intended |
| `StaleAfter: d` | Idle for d → Stale, emitting `EventStale`; data returning emits `EventStaleRecover` |
| `VanishAfter: -1` | No disappearance cleanup. **State accumulates**; only for bounded key sets |
| `VanishAfter: d` | Idle for d → state dropped; `EventVanish` only if the key was Firing |
| `MaxKeys: n` | Cardinality cap. Beyond it new keys are rejected, logged once |
| `KeepWindowOnStale` | Keep the window when going Stale |

The window is **cleared** on entering Stale by default: a data gap is a break in continuity,
and count-based conditions (`ConsecutiveN` and friends) must not count across it. Time-window
conditions (`CountInWindow`) decay on their own, so set `KeepWindowOnStale: true` where a gap
should not erase the statistic.

`For` and `ClearFor` durations are continuity too, and likewise do not accrue across a gap.
On entering Stale, an unsatisfied `Pending` drops back to OK and restarts on recovery;
`Firing` is preserved, but a clearing-in-progress timer is reset.

Callers that track entities by whether they appeared this round can use `Forget` to drop a
disappeared key immediately, instead of waiting out the `VanishAfter` grace period.

---

## Event.Meta: self-contained events

Material the handler needs to compose a message — the original log line, the probe packet,
the configuration as of the observation — should be attached with `ObserveMeta`. The engine
hands it back verbatim on every event for that key.

**Do not keep a `key → payload` side table next to the engine.** Its lifetime will not line
up with the engine's keys: it leaks when `MaxKeys` rejects a key, and goes uncleaned when a
key vanishes.

The payload is bound while `ObserveMeta` holds the lock, so queued events are
self-contained: calling `Forget` or overwriting the payload right after `ObserveMeta` returns
does not change events already queued.

Payload lifetime equals key lifetime, which can be indefinite when `VanishAfter` is
disabled. Store only the **minimal immutable data the handler needs**; do not capture large
object graphs (an entire session or connection) or engine state will pin them from
collection.

`Observe` and `Touch` leave an existing payload untouched. Only `ObserveMeta` and
`ObserveEventMeta` update it.

---

## Concurrency and lifecycle

**This is the easiest part of the API to get wrong. Read it fully.**

1. **Delivery is synchronous and ordered.** The engine calls the handler outside the state
   lock but inside the delivery lock, so events are delivered serially in transition order,
   and by the time `Observe`/`Tick` **returns, its events have all been delivered**. Callers
   may assume any projection the handler maintains — a dashboard mirror, say — is up to date
   before they continue.

2. **A handler may only call read-only APIs** (`State`, `Has`, `Snapshot`). The
   event-producing methods (`Observe*`, `Touch`, `Tick`, `Run`) and the rule-mutating ones
   (`SetRules`, `Forget`, `ForgetRule`) are serialised by the same lock as delivery, so
   calling them from a handler **self-deadlocks**.

3. **A handler blocks every observation path of the same engine.** Long-running work —
   persistence, sending a notification — must be made asynchronous behind **your own
   order-preserving queue**. A single worker draining a channel in order is the usual shape:
   it keeps delivery non-blocking without giving up ordering. Handing each event to its own
   goroutine does not work, because it silently discards the ordering guarantee the engine
   went to some trouble to provide.

4. **Anything needed "as of the transition" must arrive via `ObserveMeta`** — never read
   shared mutable state from a handler. Even with the delivery lock held, `Tick`-driven
   events (`Reminder`, `Stale`, `Vanish`) can still be delivered interleaved with *other*
   goroutines' writes to that shared state, so what the handler reads may already have moved
   on. `Event.Meta` is the only view of the world as of the moment the transition happened.

`Observe`, `ObserveEvent` and `Touch` are safe from any goroutine. `Tick` is safe
concurrently with them.

---

## Window capacity and limits

- Initial capacity comes from the condition's declared `minPoints`, floored at 8 and capped
  at `MaxWindowPoints` (4096).
- A condition declaring a `minSpan` gets **automatic doubling**: when the window is full and
  the oldest point is still inside the span, capacity doubles (up to the cap). Conditions
  therefore do not have to guess the sampling frequency, and a time window is not silently
  truncated by a point count.
- A condition needing more points than `MaxWindowPoints` **can never be satisfied**.
  `SetRules` logs a warning.

Where the count is user-configurable, clamp it when building the rule, so an out-of-range
setting approximates rather than failing silently:

```go
alarm.ClampPoints(n)      // consecutive/any-N conditions
alarm.ClampDeltaPoints(n) // delta conditions (N deltas need N+1 points, so the cap is one lower)
```

New user input should still be validated against `MaxWindowPoints` at your API layer.

---

## Production usage

The engine has driven six genuinely different alerting subsystems in production, between
them eight distinct rule shapes. The value of this table is the mapping from workload shape
to the engine features it needs.

| Workload | Condition | Engine features exercised |
| --- | --- | --- |
| A syslog frequency monitor | `CountInWindow(times, window)` | Time window; `Reminder` re-notifies once per window; count/window/query in the fingerprint |
| A network link monitor with multi-level severity | `ConsecutiveN` + `Threshold` | Multi-level; `Escalate`/`Exit` guards; count-based `Clear` recovery; thresholds in the fingerprint |
| A connection-count monitor | `ConsecutiveN(threshold, judge)` | No observation when the value is unavailable, so state freezes rather than alerting |
| A general metric-rule monitor | `Threshold(judge)` + `For` | Many series per rule; `VanishAfter` grace period; `MaxKeys`; `Touch` for a counter's first round |
| A per-host system-metric monitor | `ConsecutiveN` / `ConsecutiveDeltaN` | Gauge and counter semantics side by side; `ClampPoints`/`ClampDeltaPoints` |
| A per-process liveness monitor (~100k keys) | `Threshold(v >= 1)` | Package-level engine so state survives reconnects; `MaxKeys` in the six figures; `Forget` for immediate cleanup |
| An ICMP / agent reachability monitor | `Threshold(v >= 1)` + `For` (as a timeout) | Event-driven input; `ObserveMeta` carrying the probe payload; `Run` ticking itself |
| A threshold-judgement routine | `AnyN(threshold, judge)` | Long-lived rules with observations that skip rounds; a skipped round must not clear the window |

---

## Limitations

- **One key carries one scalar series.** Multi-dimensional metrics must be encoded into a
  single number by the caller, or split across rules.
- **No cross-rule silencing.** `Reminder` and `ClearFor` throttle one rule+key. A silence
  window spanning rules, or one that suppresses even the first alert, belongs in the caller.
- **No notification delivery.** The engine emits events; routing, formatting, batching and
  recipient resolution are yours.
- **No persistence.** State is in memory and does not survive a restart. `Snapshot` exists so
  you can reconcile an external projection after one, but the engine itself starts cold —
  a key that was Firing before a restart will fire again once its condition is re-satisfied.
- **No built-in statistical conditions.** Conditions are threshold- and count-shaped.
  Anything more sophisticated is a `Condition` you write yourself.

---

## Design invariants

If you change this package, these properties must still hold:

1. **Delivered means done.** When `Observe`/`Tick` returns, every event it produced has
   reached the handler.
2. **Transition order equals delivery order.** Guaranteed by holding the delivery lock across
   both transition and delivery. The lock order is fixed: delivery lock, then state lock;
   the handler runs inside the former and outside the latter.
3. **The evaluation clock is monotonic.** A late observation inserts *data*; it never rewinds
   *time*. Evaluating at a historical timestamp would let the window's upper bound exclude
   newer real hits, producing false resolve/re-fire oscillation.
4. **The window stays in time order.** Observations are inserted by timestamp at the single
   write point, so out-of-order input cannot assemble a false window; the count is bounded.
5. **Conditions are stateless.** All state lives in the engine, which is what makes hot
   swapping safe.
6. **Never alerted means silent.** A key that has never been Firing emits nothing, whether it
   disappears or its rule is deleted.
7. **The window's upper time bound is asymmetric.** `Points` and `Count` exclude observations
   later than the moment of evaluation; `Last` and `LastN` do not, and return the newest
   points held regardless. Nothing is currently evaluated in the past — invariant 3 is what
   guarantees that — so the difference is unobservable through the engine. It is not
   unobservable through `Condition`, which is a public extension point: a condition must not
   assume `Last`/`LastN` are bounded the way `Points`/`Count` are. Either bound both ends
   here, or leave invariant 3 intact.
8. **Window capacity grows but never shrinks.** Reinstalling a rule whose conditions need
   fewer points leaves the existing window at its larger capacity. This is intended: a window
   that is too large only costs memory, whereas one that is too small silently prevents a
   condition from ever breaching.
9. **Standard library only.** No dependencies, ever.

---

## License

MIT. See [LICENSE](LICENSE).
