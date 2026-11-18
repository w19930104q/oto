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

package oto_test

import (
	"bytes"
	"math"
	"testing"
	"time"
)

const (
	acceptFrames  = 512
	acceptBytes   = acceptFrames * 2 * 4 // stereo float32
	acceptTimeout = 5 * time.Second
)

// acceptSegment builds acceptFrames frames of stereo float32 with value v.
func acceptSegment(v float32) []byte {
	bits := math.Float32bits(v)
	b := make([]byte, acceptBytes)
	for i := 0; i < acceptFrames*2; i++ {
		b[4*i] = byte(bits)
		b[4*i+1] = byte(bits >> 8)
		b[4*i+2] = byte(bits >> 16)
		b[4*i+3] = byte(bits >> 24)
	}
	return b
}

// waitUntil polls until cond holds or the deadline passes.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(acceptTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestPlayerPauseAndResume exercises pause/resume through the public API with a
// fixed segment: a paused player stays paused, and playback reaches the end
// after Play without an error.
func TestPlayerPauseAndResume(t *testing.T) {
	p := theContext.NewPlayer(bytes.NewReader(acceptSegment(0.3)))
	p.SetBufferSize(acceptBytes)
	p.Play()
	waitUntil(t, "the buffer to fill", func() bool { return p.BufferedSize() == acceptBytes })

	p.Pause()
	if p.IsPlaying() {
		t.Fatal("IsPlaying after Pause: got true; want false")
	}
	time.Sleep(50 * time.Millisecond)
	if p.IsPlaying() {
		t.Fatal("a paused player resumed on its own")
	}

	p.Play()
	if !p.IsPlaying() {
		t.Fatal("IsPlaying after Play: got false; want true")
	}
	waitUntil(t, "the segment to finish", func() bool { return !p.IsPlaying() })
	if err := p.Err(); err != nil {
		t.Fatalf("Err after pause and resume: %v", err)
	}
}

// TestPlayerSwitchSource joins two fixed segments through the public API. The
// first segment's buffered data is played to its end before the second segment
// starts, and the player ends after the second segment with no error.
func TestPlayerSwitchSource(t *testing.T) {
	first, second := acceptSegment(0.25), acceptSegment(-0.75)
	p := theContext.NewPlayer(bytes.NewReader(first))
	// A buffer much larger than the segment keeps the player playing past the
	// first segment's end while the second one fills, so the switch joins them.
	p.SetBufferSize(acceptBytes * 4)
	p.Play()
	waitUntil(t, "the first segment to buffer", func() bool { return p.BufferedSize() == acceptBytes })

	// Switch while the first segment is buffered and playing.
	p.SwitchSource(bytes.NewReader(second))
	if err := p.Err(); err != nil {
		t.Fatalf("Err after SwitchSource: %v", err)
	}
	if !p.IsPlaying() {
		t.Fatal("SwitchSource paused a playing player")
	}

	// Both segments are consumed: the player ends only after the second one,
	// which is longer than the remaining first-segment data alone.
	waitUntil(t, "both segments to finish", func() bool { return !p.IsPlaying() })
	if err := p.Err(); err != nil {
		t.Fatalf("Err after playing both segments: %v", err)
	}
	// What remains is only data already sent to the hardware, still playing out.
	if got := p.BufferedSize(); got != 0 {
		t.Errorf("BufferedSize after the end: got %d; want 0", got)
	}
}

// TestPlayerSwitchSourceWhilePaused keeps a paused player paused across a
// switch, and plays the joined segments after Play.
func TestPlayerSwitchSourceWhilePaused(t *testing.T) {
	first, second := acceptSegment(0.25), acceptSegment(0.75)
	p := theContext.NewPlayer(bytes.NewReader(first))
	p.SetBufferSize(acceptBytes)
	p.Play()
	waitUntil(t, "the first segment to buffer", func() bool { return p.BufferedSize() == acceptBytes })

	p.PauseAndStopReading()
	p.SwitchSource(bytes.NewReader(second))
	if p.IsPlaying() {
		t.Fatal("SwitchSource started a paused player")
	}
	time.Sleep(50 * time.Millisecond)
	if p.IsPlaying() {
		t.Fatal("a paused player resumed on its own")
	}

	p.Play()
	waitUntil(t, "the joined segments to finish", func() bool { return !p.IsPlaying() })
	if err := p.Err(); err != nil {
		t.Fatalf("Err after switching while paused: %v", err)
	}
}

// TestPlayerSwitchSourceDoesNotRequireSeeker uses plain io.Readers, which do
// not implement io.Seeker, for both segments.
func TestPlayerSwitchSourceDoesNotRequireSeeker(t *testing.T) {
	first, second := acceptSegment(0.1), acceptSegment(0.9)
	p := theContext.NewPlayer(bytes.NewBuffer(first))
	p.SetBufferSize(acceptBytes)
	p.Play()
	waitUntil(t, "the first segment to buffer", func() bool { return p.BufferedSize() == acceptBytes })

	p.SwitchSource(bytes.NewBuffer(second))
	waitUntil(t, "both segments to finish", func() bool { return !p.IsPlaying() })
	if err := p.Err(); err != nil {
		t.Fatalf("Err after switching non-seekable sources: %v", err)
	}
}
