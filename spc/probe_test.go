package spc_test

import "github.com/waylen888/alarm"

// probeCondition observes another condition's verdict, or captures a window,
// without ever breaching itself. Its MinPoints is explicit because that is
// what sizes the engine's ring: a probe wrapping a real condition must
// declare that condition's own requirement, and one merely capturing a window
// wants everything the engine will hold.
//
// The two uses share a type deliberately. A capture helper that hard-coded
// MaxWindowPoints would size the ring at 4096 for a measurement that needs 65,
// and the numbers would be wrong with nothing failing.
type probeCondition struct {
	points int
	judge  func(alarm.Window) bool
}

func (c probeCondition) Breach(w alarm.Window) bool { return c.judge(w) }

func (c probeCondition) MinPoints() int { return c.points }

// conditionFunc is a probe that asks the engine for as much history as it
// will hold, for tests that want to inspect a window rather than judge it.
func conditionFunc(f func(alarm.Window) bool) probeCondition {
	return probeCondition{points: alarm.MaxWindowPoints, judge: f}
}
