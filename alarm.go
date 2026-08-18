// Package alarm implements an embeddable alerting state machine.
//
// The engine owns the "condition met -> fire -> resolve" lifecycle for any
// number of rules, each tracking any number of keys: it evaluates conditions
// over a per-key observation window, applies duration gates (For, ClearFor),
// handles multi-level escalation and de-escalation, throttles reminders, and
// detects data staleness and key disappearance. Its only output is a stream
// of Event values delivered synchronously and in transition order to a
// caller-supplied Handler.
//
// The engine deliberately does not decide semantics. Message text, severity
// mapping to notification payloads, alert history persistence and delivery
// channels are the caller's business. Threshold comparison itself stays in
// caller-owned closures, wired in through Threshold and friends, so the
// caller keeps a single source of truth for what "breached" means.
//
// Dependency rule: this package imports only the standard library. That is a
// hard guarantee, not an aspiration — it is what lets the engine be embedded
// in any process without dragging in a dependency tree.
package alarm

import "time"

// Severity is the level of an alert. Higher values are more severe.
type Severity int

// Severity levels, ordered from least to most severe.
const (
	SeverityInfo Severity = iota
	SeverityWarn
	SeverityError
)

// String returns the lower-case name of the severity, or "unknown".
func (s Severity) String() string {
	switch s {
	case SeverityInfo:
		return "info"
	case SeverityWarn:
		return "warn"
	case SeverityError:
		return "error"
	}
	return "unknown"
}

// State is the alert state of a single key.
type State int

// Alert states of a key.
const (
	StateOK      State = iota
	StatePending       // condition holds but For has not elapsed yet; silent
	StateFiring
	StateStale // no observation for longer than StaleAfter
)

// String returns the lower-case name of the state, or "unknown".
func (s State) String() string {
	switch s {
	case StateOK:
		return "ok"
	case StatePending:
		return "pending"
	case StateFiring:
		return "firing"
	case StateStale:
		return "stale"
	}
	return "unknown"
}

// EventKind is the kind of event emitted by the engine.
type EventKind int

// Event kinds emitted by the engine.
const (
	EventFire       EventKind = iota
	EventEscalate             // severity raised while Firing
	EventDeescalate           // severity lowered while Firing
	EventReminder             // periodic re-notification while Firing
	EventResolve
	EventStale
	EventStaleRecover
	EventVanish // key state cleared; emitted only for keys that were Firing (including Firing before Stale), and implies resolved
)

// String returns the snake_case name of the event kind, or "unknown".
func (k EventKind) String() string {
	switch k {
	case EventFire:
		return "fire"
	case EventEscalate:
		return "escalate"
	case EventDeescalate:
		return "deescalate"
	case EventReminder:
		return "reminder"
	case EventResolve:
		return "resolve"
	case EventStale:
		return "stale"
	case EventStaleRecover:
		return "stale_recover"
	case EventVanish:
		return "vanish"
	}
	return "unknown"
}

// Event is the engine's only output.
type Event struct {
	RuleID   string
	Key      string
	Kind     EventKind
	Severity Severity
	State    State     // state after the transition
	Value    float64   // measured value behind the event (count, rate or last observation, depending on the condition)
	Since    time.Time // start time of the state this event reports (for Fire, the time the condition first held)
	At       time.Time

	// Meta is whatever the most recent ObserveMeta call attached to this key.
	// The engine never reads or writes it and hands it back verbatim. It is
	// bound at the moment of the transition, which makes each event
	// self-contained: the caller does not need (and should not keep) a
	// key-to-payload side table next to the engine. When observations arrive
	// from several goroutines, delivery of Tick-driven events
	// (Reminder/Stale/Vanish) can still interleave with other goroutines'
	// writes to shared state, so the state "as of the transition" is only
	// available here — do not read shared mutable state inside a handler.
	// Callers that never use ObserveMeta always see nil.
	//
	// Payload lifetime equals key lifetime, which can be indefinite when
	// VanishAfter is disabled: store only the minimal immutable data the
	// handler needs, and do not capture large object graphs (an entire
	// session or connection), or engine state will pin them from collection.
	Meta any
}

// Handler turns an Event into whatever the caller wants to do with it.
// The delivery contract is:
//
//  1. Synchronous and ordered — the engine calls the handler outside the
//     state lock but inside the delivery lock, so events are delivered
//     serially in transition order, and by the time Observe/Tick returns its
//     events have all been delivered. Callers may assume any projection the
//     handler maintains (a mirror, a dashboard) is up to date before they
//     continue.
//  2. A handler may only call read-only APIs (State/Has/Snapshot). The
//     event-producing methods (Observe*/Touch/Tick/Run) and the rule-mutating
//     ones (SetRules/Forget/ForgetRule) are serialised by the same lock as
//     delivery, so calling them from a handler self-deadlocks.
//  3. A handler blocks every observation path of the same engine. Make
//     long-running work (persistence, notification delivery) asynchronous
//     behind your own order-preserving queue.
//  4. Anything the handler needs "as of the transition" must be attached with
//     ObserveMeta (see Event.Meta). Delivery of Tick-driven events can still
//     interleave with other goroutines' writes, so reading shared mutable
//     state from a handler remains unsafe.
type Handler func(Event)

// Level binds a severity to a condition. A Rule evaluates its levels from
// highest to lowest severity and takes the highest one that holds.
type Level struct {
	Severity  Severity
	Condition Condition

	// Exit is the optional de-escalation guard. While Firing at this level, a
	// lower level becoming true is not by itself enough to step down; Exit
	// must hold as well before a Deescalate event is emitted. This is for the
	// common case where the lower level's condition is implied by the samples
	// that satisfied the higher one (warn = n consecutive samples with v >= 1,
	// while error samples are also >= 1; with no Exit, a single warn sample
	// would drop the key from error). nil keeps the default behaviour.
	Exit Condition

	// Escalate is the optional escalation guard, symmetric to Exit. While
	// Firing at a lower level, this level's Condition becoming true is not by
	// itself enough to step up; Escalate must hold as well before an Escalate
	// event is emitted. This is for cases where "classify on first fire" and
	// "escalate an existing alert" have different semantics (first fire = n
	// consecutive bad samples, graded by the current sample; escalation = n
	// consecutive samples at the higher level, where a lower-level sample
	// resets the streak). nil keeps the default behaviour. It only affects
	// the escalation path while Firing; initial classification (OK -> Firing)
	// and Pending are unaffected.
	Escalate Condition
}

// Rule is one alerting rule. A single rule tracks any number of keys (for
// example one per series), each running the state machine independently.
type Rule struct {
	ID     string
	Levels []Level       // at least one; a single-level alert has exactly one
	For    time.Duration // how long the condition must hold before Firing (<=0 fires immediately)

	// Fingerprint is an optional semantic fingerprint of the rule's
	// conditions. Hot reload matches rules by ID and keeps their state, but
	// the engine cannot see inside a Condition closure, nor how observed
	// values are encoded. When "which values get observed" or "what a value
	// means" depends on mutable configuration (samples encoded against a
	// threshold before observation, a query filter deciding what counts as a
	// hit), a configuration change invalidates the observations already in
	// the window. Callers should encode those semantic inputs into the
	// fingerprint: when SetRules sees the same ID with a different
	// fingerprint it rebuilds the whole rule, so old observations cannot
	// count towards the new condition and an old Firing state is cleared
	// silently, the same semantics as a rule disappearing. The fingerprint
	// must be a deterministic serialisation of the configuration (sort maps).
	//
	// A rule whose observations are raw measurements, with all judgement
	// inside the Condition closure, needs no fingerprint when For <= 0 (empty
	// = keep state forever): SetRules swaps in the new condition and the raw
	// history in the window is simply re-judged by it.
	//
	// When For > 0, however, the decision threshold itself must go into the
	// fingerprint. The pending start time is accrued state — "the condition
	// has held since T" — and is not recomputed from the window, so keeping
	// it across a condition swap lets the new condition borrow the old one's
	// elapsed time (change the threshold from >10 to >100 and a key already
	// pending for four minutes only needs one more to satisfy a five-minute
	// For). Every setting that can change the Condition's verdict (operator,
	// threshold, ...) must be encoded.
	//
	// Changing For itself does not belong in the fingerprint: "the condition
	// has held for X" is independent of the value of For, and comparing the
	// existing start time against the new value is the correct answer
	// (shortening For fires immediately, lengthening it keeps waiting).
	// Rebuilding would only discard an alert in progress.
	Fingerprint string

	Reminder time.Duration // re-notification interval while Firing (<=0 disables)
	ClearFor time.Duration // how long the condition must stay clear before Resolve (flap damping, <=0 resolves immediately)

	// Clear is an optional count-based recovery condition. When set, a Firing
	// key resolves only when no Level holds and Clear holds (for example
	// ConsecutiveN(n, healthy) = n consecutive healthy samples), and ClearFor
	// is ignored. nil keeps the time-based ClearFor behaviour.
	Clear       Condition
	StaleAfter  time.Duration // idle time before a key goes Stale (0 = engine default; <0 disables)
	VanishAfter time.Duration // idle time before a key's state is dropped (0 = engine default; <0 disables, note that state then accumulates)
	MaxKeys     int           // cardinality cap per rule (0 = engine default)

	// KeepWindowOnStale keeps the observation window when a key goes Stale
	// (it is cleared by default). Count-based consecutive conditions
	// (ConsecutiveN and friends) treat a data gap as a break in continuity
	// and want the default; time-window conditions (CountInWindow and
	// friends) decay on their own, so set this to true when a gap should not
	// erase the statistic.
	KeepWindowOnStale bool
}
