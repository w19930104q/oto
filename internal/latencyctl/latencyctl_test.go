// Copyright 2026 The Oto Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package latencyctl_test

import (
	"testing"
	"time"

	"github.com/ebitengine/oto/v3/internal/latencyctl"
)

const (
	testRate       = 48000
	testPeriod     = 10 * time.Millisecond
	testPeriodFr   = int64(testRate / 100) // 480 frames per device period
	testBufferFr   = testPeriodFr * 5
	testNominal    = 2 * testPeriod
	testMaxLatency = 5 * testPeriod
)

func newTestController() *latencyctl.Controller {
	return latencyctl.NewController(testRate, testPeriod, int(testBufferFr), testNominal, testMaxLatency)
}

// step simulates one device event: the device consumes consumedFrames, never
// below an empty buffer, then the controller commits. It returns the frames
// committed and the resulting queued water.
func step(t *testing.T, c *latencyctl.Controller, padding, consumedFrames int64, delay time.Duration) (committed, queued int64) {
	t.Helper()

	padding -= consumedFrames
	if padding < 0 {
		padding = 0
	}
	committed = c.FramesToCommit(padding)
	if padding+committed > testBufferFr {
		t.Fatalf("commit of %d on top of padding %d overflows buffer %d", committed, padding, testBufferFr)
	}
	queued = padding + committed
	c.RecordCommit(delay, queued)
	return committed, queued
}

// TestNominalBudgetKeepsOutputWithinBudget drives a perfectly scheduled
// virtual device and asserts the queued audio never exceeds the latency
// budget once the stream has primed.
func TestNominalBudgetKeepsOutputWithinBudget(t *testing.T) {
	c := newTestController()

	// Virtual device consumes one period per event; the render thread is
	// always on time.
	padding := int64(0)
	for range 200 {
		var frames int64
		frames, padding = step(t, c, padding, testPeriodFr, 100*time.Microsecond)
		if frames < 0 || frames > testBufferFr {
			t.Fatalf("FramesToCommit returned %d, want within [0, %d]", frames, testBufferFr)
		}

		// After the priming events the water must stay at the nominal target.
		if padding > testPeriodFr*2 {
			t.Fatalf("queued %d frames exceeds nominal budget %d", padding, testPeriodFr*2)
		}
	}

	stats := c.Snapshot()
	if stats.Underruns != 0 || stats.Degradations != 0 {
		t.Fatalf("healthy stream must not degrade: underruns=%d degradations=%d", stats.Underruns, stats.Degradations)
	}
	if stats.OutputLatency.P95 > testNominal {
		t.Fatalf("output latency p95 %s exceeds budget %s", stats.OutputLatency.P95, testNominal)
	}
	if stats.OutputLatency.Max > testNominal+testPeriod {
		t.Fatalf("output latency max %s exceeds budget %s by more than one period", stats.OutputLatency.Max, testNominal)
	}
	if stats.TargetLatency != testNominal {
		t.Fatalf("target latency %s, want nominal %s", stats.TargetLatency, testNominal)
	}
}

// TestLateEventDegradesAndKeepsFlowing makes the render thread miss events
// repeatedly. The controller must raise the budget and every commit must fit
// the free space, so the stream keeps playing without over-commitment.
func TestLateEventDegradesAndKeepsFlowing(t *testing.T) {
	c := newTestController()

	padding := int64(0)
	// The render thread is on time for the first 50 events, then the device
	// drains three periods per event for a while, then recovers.
	for event := range 300 {
		consumed := testPeriodFr
		if event >= 50 && event < 120 {
			consumed = testPeriodFr * 3
		}
		_, padding = step(t, c, padding, consumed, 2*time.Millisecond)
	}

	stats := c.Snapshot()
	if stats.Degradations == 0 {
		t.Fatal("late events must trigger degradation")
	}
	if stats.Underruns == 0 {
		t.Fatal("late events must be counted as underruns")
	}
	// The outage must have raised the target above the nominal budget, even
	// if the healthy tail already walked it back down.
	if stats.PeakTargetLatency <= testNominal {
		t.Fatalf("peak target %s must exceed nominal %s after underruns", stats.PeakTargetLatency, testNominal)
	}
	if stats.PeakTargetLatency > testMaxLatency {
		t.Fatalf("degraded target %s exceeds max %s", stats.PeakTargetLatency, testMaxLatency)
	}
	// The deepest water reached during the outage must stay within the maximum
	// latency budget; OutputLatency samples capture it.
	if stats.OutputLatency.Max > testMaxLatency {
		t.Fatalf("output latency max %s exceeds max budget %s", stats.OutputLatency.Max, testMaxLatency)
	}
}

// TestRecoveryReturnsToNominal verifies a degraded stream walks back to the
// nominal budget after enough healthy events.
func TestRecoveryReturnsToNominal(t *testing.T) {
	c := newTestController()

	padding := int64(0)
	// Prime, then starve, then run healthy long enough to recover fully.
	phases := []struct {
		events   int
		consumed int64
	}{
		{50, testPeriodFr},
		{40, testPeriodFr * 3},
		{2000, testPeriodFr},
	}
	for _, ph := range phases {
		for range ph.events {
			_, padding = step(t, c, padding, ph.consumed, time.Millisecond)
		}
	}

	stats := c.Snapshot()
	if stats.Recoveries == 0 {
		t.Fatal("healthy stretch after degradation must recover")
	}
	if stats.TargetLatency != testNominal {
		t.Fatalf("target %s did not return to nominal %s", stats.TargetLatency, testNominal)
	}
}

// TestCommitDelayIsBoundedAndRecorded checks the commit-delay statistics the
// acceptance uses are populated and reflect the measured delays.
func TestCommitDelayIsBoundedAndRecorded(t *testing.T) {
	c := newTestController()

	padding := int64(0)
	delays := []time.Duration{
		50 * time.Microsecond, 80 * time.Microsecond, 120 * time.Microsecond,
		2 * time.Millisecond, 3 * time.Millisecond,
	}
	for event := range 100 {
		frames := c.FramesToCommit(padding)
		delay := delays[event%len(delays)]
		queued := padding + frames
		c.RecordCommit(delay, queued)
		padding = queued - testPeriodFr
		if padding < 0 {
			padding = 0
		}
	}

	stats := c.Snapshot()
	if stats.CommitLatency.Count == 0 {
		t.Fatal("commit latency was not recorded")
	}
	if stats.CommitLatency.Min < 50*time.Microsecond {
		t.Fatalf("min commit latency %s below injected minimum", stats.CommitLatency.Min)
	}
	if stats.CommitLatency.Max < 3*time.Millisecond {
		t.Fatalf("max commit latency %s below injected maximum", stats.CommitLatency.Max)
	}
	if stats.CommitLatency.P95 < stats.CommitLatency.Mean {
		t.Fatalf("p95 %s below mean %s", stats.CommitLatency.P95, stats.CommitLatency.Mean)
	}
}

// TestNeverOverflowOnExtremeLateEvents floods the controller with the worst
// case scheduling pattern and asserts every commit fits the free space.
func TestNeverOverflowOnExtremeLateEvents(t *testing.T) {
	c := newTestController()

	padding := int64(0)
	for range 500 {
		// The device drained completely between every event.
		padding = 0
		frames := c.FramesToCommit(padding)
		if frames > testBufferFr {
			t.Fatalf("commit %d exceeds buffer %d", frames, testBufferFr)
		}
		queued := padding + frames
		c.RecordCommit(5*time.Millisecond, queued)
		padding = queued
	}

	stats := c.Snapshot()
	// Fully degraded target must clamp at the buffer, not grow past it.
	if stats.TargetLatency > testMaxLatency {
		t.Fatalf("target %s exceeds max latency %s", stats.TargetLatency, testMaxLatency)
	}
}

// TestStatsEmptyBeforeAnyCommit verifies zero-value stats are well formed.
func TestStatsEmptyBeforeAnyCommit(t *testing.T) {
	c := newTestController()
	stats := c.Snapshot()
	if !stats.LowLatency {
		t.Fatal("controller must report low-latency mode")
	}
	if stats.CommitLatency.Count != 0 || stats.OutputLatency.Count != 0 {
		t.Fatal("fresh controller must have no samples")
	}
	if stats.NominalLatency != testNominal || stats.MaxLatency != testMaxLatency {
		t.Fatalf("budgets not initialized: nominal=%s max=%s", stats.NominalLatency, stats.MaxLatency)
	}
}

// TestStatsSurviveDeviceRecreation models a device restart: the driver keeps
// one recorder, creates a second controller for the new device and adopts the
// first one's counters. The accumulated samples and degradation totals must
// survive.
func TestStatsSurviveDeviceRecreation(t *testing.T) {
	rec := &latencyctl.Recorder{}
	first := latencyctl.NewControllerWithRecorder(testRate, testPeriod, int(testBufferFr), testNominal, testMaxLatency, rec)

	// Prime, then force degradations on the first device.
	padding := int64(0)
	for event := range 120 {
		consumed := testPeriodFr
		if event >= 50 {
			consumed = testPeriodFr * 3
		}
		padding -= consumed
		if padding < 0 {
			padding = 0
		}
		frames := first.FramesToCommit(padding)
		queued := padding + frames
		first.RecordCommit(2*time.Millisecond, queued)
		padding = queued
	}
	before := first.Snapshot()
	if before.Degradations == 0 || before.CommitLatency.Count == 0 {
		t.Fatalf("test setup produced no history: %+v", before)
	}

	// Recreate the device with a fresh controller sharing the recorder.
	second := latencyctl.NewControllerWithRecorder(testRate, testPeriod, int(testBufferFr), testNominal, testMaxLatency, rec)
	second.Adopt(first)

	after := second.Snapshot()
	if after.Degradations != before.Degradations {
		t.Fatalf("degradations reset across restart: %d vs %d", after.Degradations, before.Degradations)
	}
	if after.PeakTargetLatency != before.PeakTargetLatency {
		t.Fatalf("peak target reset across restart: %s vs %s", after.PeakTargetLatency, before.PeakTargetLatency)
	}
	if after.CommitLatency.Count != before.CommitLatency.Count {
		t.Fatalf("latency samples reset across restart: %d vs %d", after.CommitLatency.Count, before.CommitLatency.Count)
	}

	// New events keep accumulating into the same history.
	for range 50 {
		frames := second.FramesToCommit(padding)
		queued := padding + frames
		second.RecordCommit(time.Millisecond, queued)
		padding = queued - testPeriodFr
		if padding < 0 {
			padding = 0
		}
	}
	if got := second.Snapshot().CommitLatency.Count; got <= before.CommitLatency.Count {
		t.Fatalf("new samples not accumulated after restart: %d <= %d", got, before.CommitLatency.Count)
	}
}
