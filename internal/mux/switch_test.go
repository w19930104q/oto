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

package mux_test

import (
	"bytes"
	"errors"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ebitengine/oto/v3/internal/mux"
)

// Fixed audio segments the acceptance tests use. Each is a whole number of
// stereo float32 frames, so every segment ends on a frame boundary.
const (
	segmentFrames  = 256
	segmentSamples = segmentFrames * 2
	segmentBytes   = segmentSamples * 4
)

// segment returns segmentBytes bytes of FormatFloat32LE stereo data with the
// constant sample value v.
func segment(v float32) []byte {
	bits := math.Float32bits(v)
	b := make([]byte, segmentBytes)
	for i := 0; i < segmentSamples; i++ {
		b[4*i] = byte(bits)
		b[4*i+1] = byte(bits >> 8)
		b[4*i+2] = byte(bits >> 16)
		b[4*i+3] = byte(bits >> 24)
	}
	return b
}

// readBlock mixes one block of blockFrames frames from the mux, as a driver does.
func readBlock(m *mux.Mux, blockFrames int) []float32 {
	buf := make([]float32, blockFrames*2)
	m.ReadFloat32s(buf)
	return buf
}

// drainPlayer mixes blocks until the player's source has ended and all buffered
// data is mixed, and returns every mixed sample. It must be called in a
// synctest bubble. The final read is shortened to the remaining buffered data,
// so the returned samples are exactly what the player produced.
func drainPlayer(m *mux.Mux, p *mux.Player, blockFrames int) []float32 {
	var got []float32
	synctest.Wait()
	// A player whose previous source just ended may have no buffered data until
	// the mux's loop leaves its short sleep and reads the new source.
	for p.IsRegistered() {
		for p.BufferedSize() == 0 {
			time.Sleep(2 * time.Millisecond)
			synctest.Wait()
			if !p.IsRegistered() {
				return got
			}
		}
		frames := min(blockFrames, p.BufferedSize()/bytesPerFrame)
		got = append(got, readBlock(m, frames)...)
		synctest.Wait()
	}
	return got
}

// pauseSilenceAndResume verifies the pause/resume contract:
// while paused the mix is silence, and after Play the source resumes from the
// position where it paused without a skipped or repeated sample.
func TestPauseSilencesAndResumeContinues(t *testing.T) {
	synctest.Test(t, testPauseSilencesAndResumeContinues)
}

func testPauseSilencesAndResumeContinues(t *testing.T) {
	const (
		frames      = segmentFrames * 4
		blockFrames = 32
	)

	m := newMux(t)
	// An incrementing ramp lets the test check every sample's position.
	src := &rampFloat32Reader{frames: frames}
	p := newPlayer(t, m, src)
	p.SetBufferSize(frames * bytesPerFrame)
	p.Play()
	synctest.Wait()

	// Play the first segment, then pause.
	first := readBlock(m, blockFrames)
	for len(first) < segmentSamples {
		first = append(first, readBlock(m, blockFrames)...)
	}
	p.Pause()
	if p.IsPlaying() {
		t.Fatal("IsPlaying after Pause: got true; want false")
	}

	// Everything mixed while paused must be silence.
	for _, v := range readBlock(m, blockFrames) {
		if v != 0 {
			t.Fatalf("the mix was audible while paused: got %v; want 0", v)
		}
	}

	// Resume. The player must continue exactly where it paused: the next sample
	// follows the last one mixed before the pause with no gap or repeat.
	p.Play()
	if !p.IsPlaying() {
		t.Fatal("IsPlaying after Play: got false; want true")
	}
	rest := drainPlayer(m, p, blockFrames)

	want := make([]float32, frames*2)
	for i := range want {
		want[i] = rampFloat32Sample(i)
	}
	got := append(first, rest...)
	if len(got) != len(want) {
		t.Fatalf("mixed %d samples; want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d: got %v; want %v (the pause skipped or repeated a sample)", i, got[i], want[i])
		}
	}
}

// switchAtPlay verifies the atomic buffer switch while playing: every buffered
// sample of the old segment is played first, the new segment follows with no
// gap, and no sample is skipped or repeated.
func TestSwitchSourceWhilePlayingJoinsSegments(t *testing.T) {
	synctest.Test(t, testSwitchSourceWhilePlayingJoinsSegments)
}

func testSwitchSourceWhilePlayingJoinsSegments(t *testing.T) {
	const blockFrames = 32

	m := newMux(t)
	oldSeg, newSeg := segment(0.25), segment(0.75)
	p := newPlayer(t, m, bytes.NewReader(oldSeg))
	p.SetBufferSize(segmentBytes)
	p.Play()
	synctest.Wait()

	// Consume part of the segment in fixed blocks, then switch before it ends.
	consumedFrames := blockFrames * 3
	head := readBlock(m, blockFrames)
	for len(head) < consumedFrames*2 {
		head = append(head, readBlock(m, blockFrames)...)
	}

	p.SwitchSource(bytes.NewReader(newSeg))

	// The remaining samples of the old segment come first, then the whole new
	// segment. The two join with no silence in between and no lost sample.
	rest := drainPlayer(m, p, blockFrames)
	got := append(head, rest...)
	want := append(decodeSamples(oldSeg), decodeSamples(newSeg)...)
	if len(got) != len(want) {
		t.Fatalf("mixed %d samples in total; want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d after the switch: got %v; want %v", i, got[i], want[i])
		}
	}
}

// switchAtBlockBoundary verifies that a switch issued while the player holds
// buffered data does not change the data already buffered: the old segment's
// tail is mixed whole before the new segment starts, so the switch takes effect
// only at a block boundary.
func TestSwitchSourceTakesEffectAtABlockBoundary(t *testing.T) {
	synctest.Test(t, testSwitchSourceTakesEffectAtABlockBoundary)
}

func testSwitchSourceTakesEffectAtABlockBoundary(t *testing.T) {
	const blockFrames = 64

	m := newMux(t)
	oldSeg, newSeg := segment(0.25), segment(-0.75)
	p := newPlayer(t, m, bytes.NewReader(oldSeg))
	p.SetBufferSize(segmentBytes)
	p.Play()
	synctest.Wait()

	// The first block is entirely old-segment data.
	first := readBlock(m, blockFrames)
	oldSamples := decodeSamples(oldSeg)
	for i, v := range first {
		if v != oldSamples[i] {
			t.Fatalf("first block sample %d: got %v; want %v", i, v, oldSamples[i])
		}
	}

	p.SwitchSource(bytes.NewReader(newSeg))
	synctest.Wait()

	// Drain the rest. Every remaining old-segment sample must precede every
	// new-segment sample: the switch never overwrites data in a mixed block.
	rest := drainPlayer(m, p, blockFrames)
	joined := append(append([]float32{}, first...), rest...)
	want := append(oldSamples, decodeSamples(newSeg)...)
	if len(joined) != len(want) {
		t.Fatalf("mixed %d samples; want %d", len(joined), len(want))
	}
	for i := range want {
		if joined[i] != want[i] {
			t.Fatalf("sample %d: got %v; want %v (the switch did not happen at a block boundary)", i, joined[i], want[i])
		}
	}

	// The boundary between the values must sit exactly after the old segment.
	if joined[segmentSamples-1] != 0.25 || joined[segmentSamples] != -0.75 {
		t.Fatalf("the segments did not join at the old segment's end: got %v then %v", joined[segmentSamples-1], joined[segmentSamples])
	}
}

// switchWhilePaused keeps the player paused and silent, and the new source is
// played from its start after Play without losing the old segment's tail.
func TestSwitchSourceWhilePaused(t *testing.T) {
	synctest.Test(t, testSwitchSourceWhilePaused)
}

func testSwitchSourceWhilePaused(t *testing.T) {
	const blockFrames = 32

	m := newMux(t)
	oldSeg, newSeg := segment(0.25), segment(0.75)
	p := newPlayer(t, m, bytes.NewReader(oldSeg))
	p.SetBufferSize(segmentBytes)
	p.Play()
	synctest.Wait()

	head := readBlock(m, blockFrames)
	head = append(head, readBlock(m, blockFrames)...)
	// Pause explicitly and stop reading, so the source cannot reach its end on
	// its own before the switch: this is a user-held pause, not an end-of-source
	// auto-pause.
	p.PauseAndStopReading()

	// Silence while paused, including after the switch.
	for _, v := range readBlock(m, blockFrames) {
		if v != 0 {
			t.Fatalf("the mix was audible while paused: got %v; want 0", v)
		}
	}

	p.SwitchSource(bytes.NewReader(newSeg))
	if p.IsPlaying() {
		t.Fatal("the switch started a paused player")
	}
	synctest.Wait()
	for _, v := range readBlock(m, blockFrames) {
		if v != 0 {
			t.Fatalf("the mix was audible while paused after the switch: got %v; want 0", v)
		}
	}

	p.Play()
	rest := drainPlayer(m, p, blockFrames)
	got := append(head, rest...)
	want := append(decodeSamples(oldSeg), decodeSamples(newSeg)...)
	if len(got) != len(want) {
		t.Fatalf("mixed %d samples in total; want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d: got %v; want %v", i, got[i], want[i])
		}
	}
}

// switchStopsOldSourceReads guarantees the old source is never read after the
// switch, including a read that was in flight when SwitchSource was called.
func TestSwitchSourceStopsReadingTheOldSource(t *testing.T) {
	synctest.Test(t, testSwitchSourceStopsReadingTheOldSource)
}

func testSwitchSourceStopsReadingTheOldSource(t *testing.T) {
	const blockFrames = 32

	m := newMux(t)
	old := &gatedReader{
		gate:  make(chan struct{}),
		began: make(chan struct{}),
	}
	new := &countingReader{data: segment(0.5)}
	p := newPlayer(t, m, old)
	p.Play()
	<-old.began

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.SwitchSource(new)
	}()
	select {
	case <-done:
		t.Fatal("SwitchSource returned while a read from the old source was in flight")
	case <-time.After(100 * time.Millisecond):
	}

	close(old.gate)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SwitchSource did not return after the ongoing read finished")
	}

	reads := old.reads.Load()
	// Mixing makes room in the buffer; only the new source may fill it.
	readBlock(m, blockFrames)
	synctest.Wait()
	if got := old.reads.Load(); got != reads {
		t.Errorf("the old source was read after SwitchSource: got %d reads; want %d", got, reads)
	}
	if got := new.reads.Load(); got == 0 {
		t.Error("the new source was not read after SwitchSource")
	}
}

// switchWithAQueuedSourceError replaces a source whose read failed: the pending
// error is dropped with the source, and the new source plays normally.
func TestSwitchSourceDropsAPendingSourceError(t *testing.T) {
	synctest.Test(t, testSwitchSourceDropsAPendingSourceError)
}

func testSwitchSourceDropsAPendingSourceError(t *testing.T) {
	const blockFrames = 32

	m := newMux(t)
	srcErr := errors.New("old source failed")
	oldSeg, newSeg := segment(0.25), segment(0.75)
	p := newPlayer(t, m, &errorReader{
		data: oldSeg,
		err:  srcErr,
	})
	p.SetBufferSize(segmentBytes)
	p.Play()
	synctest.Wait()

	// Switch before the buffered data is mixed, while the source error is pending.
	p.SwitchSource(bytes.NewReader(newSeg))
	if err := p.Err(); err != nil {
		t.Fatalf("Err after switching away from a failed source: got %v; want nil", err)
	}
	synctest.Wait()

	got := drainPlayer(m, p, blockFrames)
	want := append(decodeSamples(oldSeg), decodeSamples(newSeg)...)
	if len(got) != len(want) {
		t.Fatalf("mixed %d samples; want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d: got %v; want %v", i, got[i], want[i])
		}
	}
	if err := p.Err(); err != nil {
		t.Errorf("Err after playing the new source: got %v; want nil", err)
	}
}

// concurrentSwitch hammers SwitchSource together with the other player controls
// and the mixer. No call may deadlock, and the player stays usable throughout.
func TestSwitchSourceConcurrentWithControls(t *testing.T) {
	synctest.Test(t, testSwitchSourceConcurrentWithControls)
}

func testSwitchSourceConcurrentWithControls(t *testing.T) {
	const blockFrames = 16

	m := newMux(t)
	p := newPlayer(t, m, bytes.NewReader(segment(0.25)))
	p.SetBufferSize(segmentBytes)
	p.Play()
	synctest.Wait()

	var wg sync.WaitGroup
	wg.Go(func() {
		for range 200 {
			p.SwitchSource(bytes.NewReader(segment(0.5)))
		}
	})
	wg.Go(func() {
		for range 200 {
			p.Pause()
			p.Play()
		}
	})
	wg.Go(func() {
		for range 200 {
			p.SetVolume(0.5)
			_ = p.BufferedSize()
			_ = p.IsPlaying()
		}
	})
	wg.Go(func() {
		for range 200 {
			readBlock(m, blockFrames)
		}
	})
	wg.Wait()

	if err := p.Err(); err != nil {
		t.Fatalf("Err after concurrent controls: %v", err)
	}
	// The player stays usable: a fresh source plays after the storm of controls.
	p.PauseAndStopReading()
	p.SwitchSource(bytes.NewReader(segment(0.5)))
	p.Play()
	// The mux's loop can be in its short sleep after the old source ended; move
	// the synthetic clock past it so the fresh source is read deterministically.
	time.Sleep(2 * time.Millisecond)
	synctest.Wait()
	waitForBufferedSize(t, p, blockFrames*bytesPerFrame)
	buf := readBlock(m, blockFrames)
	for _, v := range buf {
		if v != 0 {
			return
		}
	}
	t.Error("the player produced only silence after the concurrent controls")
}

// switchAfterEnd revives a player whose source has ended: replacing the source
// starts it playing again, since the auto-pause at the end of a source is not an
// explicit Pause.
func TestSwitchSourceAfterSourceEnded(t *testing.T) {
	synctest.Test(t, testSwitchSourceAfterSourceEnded)
}

func testSwitchSourceAfterSourceEnded(t *testing.T) {
	const blockFrames = 32

	m := newMux(t)
	p := newPlayer(t, m, bytes.NewReader(segment(0.25)))
	p.SetBufferSize(segmentBytes)
	p.Play()
	synctest.Wait()
	if got := drainPlayer(m, p, blockFrames); len(got) != segmentSamples {
		t.Fatalf("mixed %d samples of the first segment; want %d", len(got), segmentSamples)
	}
	synctest.Wait()
	if p.IsPlaying() {
		t.Fatal("the player did not stop at the end of its source")
	}

	p.SwitchSource(bytes.NewReader(segment(0.75)))
	if !p.IsPlaying() {
		t.Fatal("SwitchSource did not revive the player after its source ended")
	}
	got := drainPlayer(m, p, blockFrames)
	want := decodeSamples(segment(0.75))
	if len(got) != len(want) {
		t.Fatalf("mixed %d samples of the new segment; want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("new segment sample %d: got %v; want %v", i, got[i], want[i])
		}
	}
}

// switchOnAPlayerPausedExplicitly keeps it paused: an explicit Pause is not
// undone by replacing the source. On Play, the retained tail of the old segment
// is still played before the new segment.
func TestSwitchSourceKeepsAnExplicitPause(t *testing.T) {
	synctest.Test(t, testSwitchSourceKeepsAnExplicitPause)
}

func testSwitchSourceKeepsAnExplicitPause(t *testing.T) {
	const blockFrames = 32

	m := newMux(t)
	oldSeg, newSeg := segment(0.25), segment(0.75)
	p := newPlayer(t, m, bytes.NewReader(oldSeg))
	p.SetBufferSize(segmentBytes)
	p.Play()
	synctest.Wait()
	// Consume part of the segment, then pause explicitly with the tail retained.
	head := readBlock(m, blockFrames)
	head = append(head, readBlock(m, blockFrames)...)
	p.PauseAndStopReading()

	p.SwitchSource(bytes.NewReader(newSeg))
	if p.IsPlaying() {
		t.Fatal("SwitchSource started a player that was paused explicitly")
	}
	for _, v := range readBlock(m, blockFrames) {
		if v != 0 {
			t.Fatalf("the mix was audible while paused: got %v; want 0", v)
		}
	}

	p.Play()
	rest := drainPlayer(m, p, blockFrames)
	got := append(head, rest...)
	want := append(decodeSamples(oldSeg), decodeSamples(newSeg)...)
	if len(got) != len(want) {
		t.Fatalf("mixed %d samples; want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d: got %v; want %v", i, got[i], want[i])
		}
	}
}

// singleSegmentBehaviorIsUnchanged plays one fixed segment without switching
// and checks the whole segment comes through sample by sample, then ends.
func TestSingleSegmentPlaybackIsUnchanged(t *testing.T) {
	synctest.Test(t, testSingleSegmentPlaybackIsUnchanged)
}

func testSingleSegmentPlaybackIsUnchanged(t *testing.T) {
	const blockFrames = 48

	m := newMux(t)
	seg := segment(0.4)
	p := newPlayer(t, m, bytes.NewReader(seg))
	p.SetBufferSize(segmentBytes)
	p.Play()
	synctest.Wait()

	got := drainPlayer(m, p, blockFrames)
	want := decodeSamples(seg)
	if len(got) != len(want) {
		t.Fatalf("mixed %d samples; want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sample %d: got %v; want %v", i, got[i], want[i])
		}
	}
}

// decodeSamples decodes all whole float32 samples of FormatFloat32LE data.
func decodeSamples(b []byte) []float32 {
	n := len(b) / 4
	out := make([]float32, n)
	for i := range out {
		bits := uint32(b[4*i]) | uint32(b[4*i+1])<<8 | uint32(b[4*i+2])<<16 | uint32(b[4*i+3])<<24
		out[i] = math.Float32frombits(bits)
	}
	return out
}

// rampFloat32Reader produces deterministic float32 samples ramping over frames.
type rampFloat32Reader struct {
	frames int
	pos    int
}

// rampFloat32Sample is the value of sample i of a rampFloat32Reader.
func rampFloat32Sample(i int) float32 {
	return float32(i%1024) / 1024
}

func (r *rampFloat32Reader) Read(buf []byte) (int, error) {
	n := 0
	for n+4 <= len(buf) && r.pos < r.frames*2 {
		v := rampFloat32Sample(r.pos)
		bits := math.Float32bits(v)
		buf[n] = byte(bits)
		buf[n+1] = byte(bits >> 8)
		buf[n+2] = byte(bits >> 16)
		buf[n+3] = byte(bits >> 24)
		r.pos++
		n += 4
	}
	if r.pos >= r.frames*2 {
		return n, io.EOF
	}
	return n, nil
}

// countingReader counts reads of fixed data and then reports EOF.
type countingReader struct {
	data  []byte
	reads atomic.Int64
	m     sync.Mutex
	pos   int
}

func (r *countingReader) Read(buf []byte) (int, error) {
	r.reads.Add(1)
	r.m.Lock()
	defer r.m.Unlock()
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(buf, r.data[r.pos:])
	r.pos += n
	if r.pos >= len(r.data) {
		return n, io.EOF
	}
	return n, nil
}
