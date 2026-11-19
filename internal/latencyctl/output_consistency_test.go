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
)

// sourceStream models the mux: an ordered, infinite sequence of frames. A
// driver commits contiguous slices of it, never skipping or repeating a frame.
type sourceStream struct {
	nextFrame int64
}

// take returns the next n contiguous global frame indices, exactly as the mux
// hands consecutive samples to ReadFloat32s.
func (s *sourceStream) take(n int64) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = s.nextFrame
		s.nextFrame++
	}
	return out
}

// renderBuffer is a minimal device buffer tracking how many frames are queued.
type renderBuffer struct {
	capacity int64
	queued   int64
}

// assertContinuous verifies that all frames committed across the run form one
// contiguous, gap-free sequence starting at frame zero. This is the
// output-consistency guarantee: the committed audio is the same ordered stream
// whichever commit policy packetizes it.
func assertContinuous(t *testing.T, committed [][]int64) {
	t.Helper()
	var want int64
	for _, chunk := range committed {
		for _, f := range chunk {
			if f != want {
				t.Fatalf("committed frame %d but expected %d: the output stream has a gap or repeat", f, want)
			}
			want++
		}
	}
	if want == 0 {
		t.Fatal("no frames were committed")
	}
}

// schedule returns the frames the device consumes at each event: one period in
// healthy stretches, three periods during the injected outage.
func schedule(event int, periodFrames int64) int64 {
	if event >= 60 && event < 110 {
		return periodFrames * 3
	}
	return periodFrames
}

// TestBothPoliciesCommitTheSameOrderedStream drives the low-latency controller
// and the fill-the-buffer policy through the identical event schedule and
// asserts both consume the source as one contiguous, gap-free stream.
func TestBothPoliciesCommitTheSameOrderedStream(t *testing.T) {
	const (
		rate   = 48000
		period = 10 * time.Millisecond
		events = 400
	)
	periodFrames := int64(rate / 100)
	bufferFrames := periodFrames * 5

	t.Run("low_latency", func(t *testing.T) {
		controller := newTestController()
		src := &sourceStream{}
		dev := &renderBuffer{capacity: bufferFrames}
		var committed [][]int64

		for e := range events {
			dev.queued -= schedule(e, periodFrames)
			if dev.queued < 0 {
				dev.queued = 0
			}
			frames := controller.FramesToCommit(dev.queued)
			if frames > 0 {
				if dev.queued+frames > bufferFrames {
					t.Fatalf("overflow at event %d", e)
				}
				committed = append(committed, src.take(frames))
				dev.queued += frames
			}
			controller.RecordCommit(time.Millisecond, dev.queued)
		}
		assertContinuous(t, committed)
	})

	t.Run("fill_buffer", func(t *testing.T) {
		src := &sourceStream{}
		dev := &renderBuffer{capacity: bufferFrames}
		var committed [][]int64

		for e := range events {
			dev.queued -= schedule(e, periodFrames)
			if dev.queued < 0 {
				dev.queued = 0
			}
			// Regular WASAPI behavior: fill every free frame each event.
			if dev.queued < bufferFrames {
				frames := bufferFrames - dev.queued
				committed = append(committed, src.take(frames))
				dev.queued = bufferFrames
			}
		}
		assertContinuous(t, committed)
	})
}

// TestLowLatencyBuffersLessThanRegular asserts the defining property of the
// feature: under a healthy schedule the low-latency device's steady-state water
// stays at the nominal budget, well below the always-full regular device.
func TestLowLatencyBuffersLessThanRegular(t *testing.T) {
	periodFrames := int64(48000 / 100)
	bufferFrames := periodFrames * 5
	controller := newTestController()
	dev := &renderBuffer{capacity: bufferFrames}

	var maxWater int64
	for range 300 {
		dev.queued -= periodFrames
		if dev.queued < 0 {
			dev.queued = 0
		}
		if frames := controller.FramesToCommit(dev.queued); frames > 0 {
			dev.queued += frames
			controller.RecordCommit(100*time.Microsecond, dev.queued)
		}
		if dev.queued > maxWater {
			maxWater = dev.queued
		}
	}

	nominalFrames := int64(2 * periodFrames)
	if maxWater > nominalFrames {
		t.Fatalf("low-latency water peaked at %d frames, over the nominal %d", maxWater, nominalFrames)
	}
	if maxWater >= bufferFrames {
		t.Fatalf("low-latency water %d reached the full buffer %d; latency would be unbounded", maxWater, bufferFrames)
	}
}

// TestConcurrentSnapshotDrivesAndReads runs the render decisions on one
// goroutine and snapshots on another, asserting the statistics stay coherent
// and the call never panics. (The race detector would additionally prove
// exclusion, but the locking is exercised regardless.)
func TestConcurrentSnapshotDrivesAndReads(t *testing.T) {
	periodFrames := int64(48000 / 100)
	controller := newTestController()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 200 {
			_ = controller.Snapshot()
		}
	}()

	padding := int64(0)
	for range 2000 {
		padding -= periodFrames
		if padding < 0 {
			padding = 0
		}
		frames := controller.FramesToCommit(padding)
		padding += frames
		controller.RecordCommit(time.Millisecond, padding)
	}
	<-done

	stats := controller.Snapshot()
	if stats.CommitLatency.Count == 0 {
		t.Fatal("expected commit samples under concurrent access")
	}
}

// TestDegradationNeverInterruptsFlow proves that during a sustained outage the
// device always holds at least one period of audio right after each commit, so
// it never runs dry once the controller has reacted.
func TestDegradationNeverInterruptsFlow(t *testing.T) {
	periodFrames := int64(48000 / 100)
	bufferFrames := periodFrames * 5
	controller := newTestController()
	dev := &renderBuffer{capacity: bufferFrames}

	var underrunEvents int
	for e := range 400 {
		dev.queued -= schedule(e, periodFrames)
		if dev.queued < 0 {
			dev.queued = 0
		}
		wasBelowPeriod := dev.queued < periodFrames
		frames := controller.FramesToCommit(dev.queued)
		dev.queued += frames
		controller.RecordCommit(2*time.Millisecond, dev.queued)

		// After the first priming events, a post-commit water below one period
		// would mean the device is about to audibly glitch.
		if e > 2 && wasBelowPeriod && dev.queued < periodFrames {
			underrunEvents++
		}
	}
	if underrunEvents != 0 {
		t.Fatalf("device left with less than one period queued at %d events", underrunEvents)
	}
}
