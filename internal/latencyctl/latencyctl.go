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

// Package latencyctl implements the low-latency commit policy shared by the
// event-driven audio drivers.
//
// A driver in low-latency mode is signaled by the device once per device
// period. On every signal it asks the Controller how many frames to commit so
// that the amount of queued audio stays within a latency budget instead of
// filling the whole device buffer, which is what keeps the output latency
// bounded. When the render thread is scheduled too late and the device would
// run dry, the Controller raises the budget (degradation) so the stream keeps
// flowing, and lowers it again once the process keeps the deadlines.
package latencyctl

import (
	"math"
	"sync"
	"time"
)

// Controller decides how many frames an event-driven driver commits at each
// device event to keep the queued audio within the latency budget.
//
// The decision methods are meant to be called from the single render thread,
// while Snapshot may be called concurrently from another goroutine.
type Controller struct {
	// sampleRate is the rate of the frames committed to the device, in Hz.
	sampleRate int64

	// periodFrames is the number of frames the device consumes between two
	// ready events.
	periodFrames int64

	// bufferFrames is the device's buffer capacity in frames.
	bufferFrames int64

	// nominalTargetFrames is the queued-water target in a healthy process.
	nominalTargetFrames int64

	// maxTargetFrames is the water target after a full degradation, capped at
	// the device buffer.
	maxTargetFrames int64

	// targetFrames is the water target the next commit aims for. It moves
	// between nominalTargetFrames and maxTargetFrames.
	targetFrames int64

	// peakTargetFrames is the deepest target reached since the start.
	peakTargetFrames int64

	// healthyPeriods counts consecutive events met without an underrun since
	// the last degradation.
	healthyPeriods int64

	// primed reports whether the stream has been filled to its target at least
	// once. The first fills, while the buffer is still empty, are priming
	// rather than missed deadlines.
	primed bool

	degradations int64
	recoveries   int64

	// mu guards the policy state above and the counters. The render thread
	// mutates them on every device event while Snapshot reads them from
	// another goroutine.
	mu  sync.Mutex
	rec *Recorder
}

// recoveryPeriods is how many consecutive on-time events must pass before a
// degraded budget takes one step back toward the nominal budget.
const recoveryPeriods = 50

// NewController creates a Controller for a device with its own fresh recorder.
//
// sampleRate is the device sample rate in Hz, period the device's signaling
// period, bufferFrames its buffer capacity, nominalLatency the latency budget
// to aim for in a healthy process, and maxLatency the budget a fully degraded
// stream may use.
func NewController(sampleRate int, period time.Duration, bufferFrames int, nominalLatency, maxLatency time.Duration) *Controller {
	return newController(sampleRate, period, bufferFrames, nominalLatency, maxLatency, &Recorder{})
}

// NewControllerWithRecorder creates a Controller that records into rec, which
// the caller keeps across device restarts so the statistics cover the whole
// context lifetime. A nil rec allocates a fresh one.
func NewControllerWithRecorder(sampleRate int, period time.Duration, bufferFrames int, nominalLatency, maxLatency time.Duration, rec *Recorder) *Controller {
	if rec == nil {
		rec = &Recorder{}
	}
	return newController(sampleRate, period, bufferFrames, nominalLatency, maxLatency, rec)
}

func newController(sampleRate int, period time.Duration, bufferFrames int, nominalLatency, maxLatency time.Duration, rec *Recorder) *Controller {
	sr := int64(sampleRate)
	periodFrames := durationToFrames(period, sr)
	if periodFrames < 1 {
		periodFrames = 1
	}

	nominal := durationToFrames(nominalLatency, sr)
	if nominal < periodFrames {
		nominal = periodFrames
	}
	maxTarget := durationToFrames(maxLatency, sr)
	if maxTarget < nominal {
		maxTarget = nominal
	}
	if bf := int64(bufferFrames); maxTarget > bf {
		maxTarget = bf
	}
	if nominal > maxTarget {
		nominal = maxTarget
	}

	return &Controller{
		sampleRate:          sr,
		periodFrames:        periodFrames,
		bufferFrames:        int64(bufferFrames),
		nominalTargetFrames: nominal,
		maxTargetFrames:     maxTarget,
		targetFrames:        nominal,
		peakTargetFrames:    nominal,
		rec:                 rec,
	}
}

// Adopt carries the cumulative counters of prev, a controller for a previous
// device acquisition, into c so that degradation and recovery totals survive a
// device restart. Both controllers must share the same recorder (created with
// NewControllerWithRecorder), so the latency samples already persist; only the
// counters are folded in here.
func (c *Controller) Adopt(prev *Controller) {
	if prev == nil || prev == c {
		return
	}
	prev.mu.Lock()
	degradations, recoveries := prev.degradations, prev.recoveries
	peak := prev.peakTargetFrames
	prev.mu.Unlock()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.degradations = degradations
	c.recoveries = recoveries
	c.peakTargetFrames = peak
	if c.peakTargetFrames < c.nominalTargetFrames {
		c.peakTargetFrames = c.nominalTargetFrames
	}
}

// FramesToCommit returns how many frames should be committed at a device
// event, given paddingFrames, the number of frames already queued in the
// device buffer.
//
// A zero result means the queued audio is still within the latency budget and
// nothing must be committed this event. An underrun, padding at or below one
// device period, raises the budget; a buffer that is already full also yields
// zero.
func (c *Controller) FramesToCommit(paddingFrames int64) int64 {
	if paddingFrames < 0 {
		paddingFrames = 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// The device was not serviced in time: it has less than one period queued,
	// so it is about to run dry or already has. Before the first successful
	// fill this is just priming, so only a primed stream degrades.
	if paddingFrames < c.periodFrames && c.primed {
		c.onUnderrunLocked(paddingFrames)
	} else if c.targetFrames > c.nominalTargetFrames {
		c.healthyPeriods++
		// Step the budget back down slowly once the process keeps meeting the
		// deadlines, so a single quiet stretch does not oscillate.
		if c.healthyPeriods >= recoveryPeriods {
			c.healthyPeriods = 0
			next := c.targetFrames - c.periodFrames
			if next < c.nominalTargetFrames {
				next = c.nominalTargetFrames
			}
			if next < c.targetFrames {
				c.targetFrames = next
				if c.targetFrames == c.nominalTargetFrames {
					c.recoveries++
				}
			}
		}
	}

	if paddingFrames >= c.bufferFrames {
		return 0
	}

	frames := c.targetFrames - paddingFrames
	if frames <= 0 {
		return 0
	}
	if available := c.bufferFrames - paddingFrames; frames > available {
		frames = available
	}
	if frames > 0 {
		// A fill up to at least one period means the stream is primed.
		if paddingFrames+frames >= c.periodFrames {
			c.primed = true
		}
	}
	return frames
}

// onUnderrunLocked raises the target water by one device period, up to the
// device buffer, so the next commits queue more safety margin. The caller holds
// c.mu.
func (c *Controller) onUnderrunLocked(paddingFrames int64) {
	c.healthyPeriods = 0

	next := c.targetFrames + c.periodFrames
	if next > c.bufferFrames {
		next = c.bufferFrames
	}
	if next > c.targetFrames {
		c.targetFrames = next
		c.degradations++
		if c.targetFrames > c.peakTargetFrames {
			c.peakTargetFrames = c.targetFrames
		}
	}

	// A nearly empty buffer means the audible latency collapsed toward zero.
	c.rec.RecordUnderrun(c.framesToDuration(paddingFrames))
}

// RecordCommit records a finished commit: delay is how long the commit took
// after the device event, and queuedFrames is the amount queued in the device
// buffer afterwards.
func (c *Controller) RecordCommit(delay time.Duration, queuedFrames int64) {
	c.rec.RecordCommit(delay, c.framesToDuration(queuedFrames))
}

// Snapshot returns the statistics collected so far.
func (c *Controller) Snapshot() Stats {
	c.mu.Lock()
	target := c.framesToDuration(c.targetFrames)
	peak := c.framesToDuration(c.peakTargetFrames)
	nominal := c.framesToDuration(c.nominalTargetFrames)
	max := c.framesToDuration(c.maxTargetFrames)
	degradations, recoveries := c.degradations, c.recoveries
	c.mu.Unlock()

	return c.rec.Snapshot(true, target, peak, nominal, max, degradations, recoveries)
}

func (c *Controller) framesToDuration(frames int64) time.Duration {
	if frames <= 0 || c.sampleRate <= 0 {
		return 0
	}
	return time.Duration(math.Round(float64(frames) * float64(time.Second) / float64(c.sampleRate)))
}

func durationToFrames(d time.Duration, sampleRate int64) int64 {
	if d <= 0 || sampleRate <= 0 {
		return 0
	}
	return int64(math.Round(float64(d) * float64(sampleRate) / float64(time.Second)))
}
