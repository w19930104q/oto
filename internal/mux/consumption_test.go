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
	"errors"
	"io"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ebitengine/oto/v3/internal/mux"
)

// chunkedReader is a seekable source that serves its PCM in fixed-size chunks,
// no matter how large a read the mux asks for. It records the read and seek
// history so tests can pin the consumption order, and it flags reads after
// Close, which the player must never perform.
type chunkedReader struct {
	mu     sync.Mutex
	data   []byte
	pos    int64
	chunk  int
	reads  int
	seeks  int
	closed bool

	readAfterClose atomic.Bool
}

func newChunkedReader(data []byte, chunkSize int) *chunkedReader {
	return &chunkedReader{
		data:  data,
		chunk: chunkSize,
	}
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.reads++
	if r.closed {
		r.readAfterClose.Store(true)
		return 0, errors.New("chunkedReader: Read after Close")
	}
	n := min(len(p), r.chunk, len(r.data)-int(r.pos))
	if n <= 0 {
		return 0, io.EOF
	}
	copy(p, r.data[r.pos:int(r.pos)+n])
	r.pos += int64(n)
	return n, nil
}

func (r *chunkedReader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.seeks++
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.pos
	case io.SeekEnd:
		offset += int64(len(r.data))
	}
	if offset < 0 || offset > int64(len(r.data)) {
		return 0, errors.New("chunkedReader: Seek out of range")
	}
	r.pos = offset
	return offset, nil
}

func (r *chunkedReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true
	return nil
}

func (r *chunkedReader) stats() (reads, seeks int, pos int64) {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.reads, r.seeks, r.pos
}

// segmentPCM returns frames stereo frames in which every float32 sample
// encodes id as id/127, so a mixed frame can be attributed back to its
// segment. id 0 would be silence, so segment ids start at 1.
func segmentPCM(id byte, frames int) []byte {
	bits := math.Float32bits(float32(id) / 127)
	b := make([]byte, frames*bytesPerFrame)
	for i := 0; i < len(b); i += 4 {
		b[i] = byte(bits)
		b[i+1] = byte(bits >> 8)
		b[i+2] = byte(bits >> 16)
		b[i+3] = byte(bits >> 24)
	}
	return b
}

// mixedSegments mixes frames frames from m and returns the segment each mixed
// frame came from, with 0 for silence. The tests encode a segment id in a
// float32 sample's low byte, i.e. sample value id/127, so it is recovered by
// rounding the mixed float32 back to the id.
func mixedSegments(m *mux.Mux, frames int) []int {
	buf := make([]float32, frames*2)
	m.ReadFloat32s(buf)

	segments := make([]int, frames)
	for i := range frames {
		segments[i] = int(float32(buf[i*2])*127 + 0.5)
	}
	return segments
}

// TestConsumptionContractPinsOrderAcrossResets drives a headless Mux and a
// chunked, seekable source through a fixed interleaving of SetBufferSize,
// Seek, and Reset, and pins the consumption contract:
//
//   - within each generation between resets, every segment of PCM is mixed at
//     most once and in source order (no byte is consumed twice or rolled back
//     into the mix);
//   - Seek and Reset make the player re-read from the new source position
//     rather than replay buffered or already sent bytes;
//   - UnplayedSize never counts more than the source has delivered and never
//     less than BufferedSize;
//   - after Close the source is not read anymore;
//   - stopping the Mux leaves no goroutine behind.
func TestConsumptionContractPinsOrderAcrossResets(t *testing.T) {
	synctest.Test(t, testConsumptionContractPinsOrderAcrossResets)
}

func testConsumptionContractPinsOrderAcrossResets(t *testing.T) {
	// Four segments, two 8-byte frames each, delivered one frame per Read.
	var pcm []byte
	for id := byte(1); id <= 4; id++ {
		pcm = append(pcm, segmentPCM(id, 2)...)
	}

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	// Stop the loop even if the test fails inside the bubble, or the bubble
	// cannot tear down with a goroutine still waiting on the condition.
	t.Cleanup(func() {
		m.Stop()
		time.Sleep(time.Second)
	})
	src := newChunkedReader(pcm, bytesPerFrame)
	p := newPlayer(t, m, src)

	p.SetBufferSize(2 * bytesPerFrame)
	p.Play()
	synctest.Wait()
	// The mux's filling loop is the only goroutine this player adds.
	playingGoroutines := runtime.NumGoroutine()
	t.Logf("active goroutines while playing: %d", playingGoroutines)
	if got, want := p.BufferedSize(), 2*bytesPerFrame; got != want {
		t.Fatalf("BufferedSize after filling: got %d; want %d", got, want)
	}

	// Generation 1: a two-frame buffer, then shrink it while data is buffered.
	var epochs [][]int
	epochs = append(epochs, mixedSegments(m, 2)) // segment 1's two frames.

	// Shrinking the buffer changes no bytes by itself: the loop only moves on
	// the next Wait, so the read count is unchanged immediately afterward.
	readsBefore, _, _ := src.stats()
	p.SetBufferSize(bytesPerFrame)
	if got, _, _ := src.stats(); got != readsBefore {
		t.Fatalf("SetBufferSize shrank the buffer and triggered a source read: got %d reads; want %d", got, readsBefore)
	}
	synctest.Wait()                                       // refill one frame, segment 2.
	epochs[0] = append(epochs[0], mixedSegments(m, 1)...) // segment 2's frame.
	synctest.Wait()                                       // the loop refills one frame, segment 3.

	// Seek while playing: the buffered segment 3 and everything sent so far
	// belong to the old position and must never be mixed again.
	if _, err := p.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if got, want := p.UnplayedSize(), p.BufferedSize(); got != want {
		t.Fatalf("UnplayedSize after Seek counted bytes sent before it: got %d; want BufferedSize %d", got, want)
	}
	synctest.Wait()
	t.Logf("active goroutines after Seek: %d", runtime.NumGoroutine())

	// Growing the buffer makes the loop read further ahead, but still once.
	p.SetBufferSize(3 * bytesPerFrame)
	synctest.Wait()
	// After the seek the buffer held segment 1's first frame; growing it adds
	// segment 1's second frame and segment 2's first, so the mix is [1 1 2].
	epochs = append(epochs, mixedSegments(m, 3))

	// Reset pauses, stops reading, and discards buffered and sent bytes.
	p.Reset()
	if got := p.BufferedSize(); got != 0 {
		t.Fatalf("BufferedSize after Reset: got %d; want 0", got)
	}
	if got := p.UnplayedSize(); got != 0 {
		t.Fatalf("UnplayedSize after Reset: got %d; want 0", got)
	}
	synctest.Wait()
	p.Play()
	synctest.Wait()
	t.Logf("active goroutines after Reset and Play: %d", runtime.NumGoroutine())
	// Freshly read from the source's current position: [2 3 3], none of it
	// replayed from the buffer or the spans sent before Reset.
	epochs = append(epochs, mixedSegments(m, 3))

	// Every generation sees each segment at most once, in source order.
	// Reset discards the buffer but does not rewind the source, so the last
	// generation continues at the source's current position, segment 2.
	wantEpochs := [][]int{
		{1, 1, 2},
		{1, 1, 2},
		{2, 3, 3},
	}
	for i, got := range epochs {
		if len(got) != len(wantEpochs[i]) {
			t.Fatalf("generation %d mixed %d frames; want %d (%v)", i, len(got), len(wantEpochs[i]), got)
		}
		seen := map[int]bool{}
		prev := 0
		for j, id := range got {
			if id != wantEpochs[i][j] {
				t.Fatalf("generation %d frame %d: got segment %d; want segment %d; full sequence %v", i, j, id, wantEpochs[i][j], got)
			}
			if id != 0 {
				if seen[id] && id != prev {
					t.Fatalf("generation %d mixed segment %d twice after leaving it: %v", i, id, got)
				}
				seen[id] = true
			}
			prev = id
		}
	}

	// The accounting never goes negative, never undercounts the buffer, and
	// never counts bytes the source has not delivered.
	_, _, delivered := src.stats()
	deliveredBytes := int(delivered)
	if got := p.UnplayedSize(); got < 0 || got < p.BufferedSize() || got > deliveredBytes {
		t.Fatalf("UnplayedSize %d outside [BufferedSize %d, delivered %d]", got, p.BufferedSize(), deliveredBytes)
	}

	// After Close, the player must not read the source anymore.
	readsAtClose, _, _ := src.stats()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	// Give the mux loop chances to misbehave: in the bubble the sleeps take no
	// wall-clock time.
	synctest.Wait()
	time.Sleep(time.Second)
	if reads, _, _ := src.stats(); reads != readsAtClose {
		t.Fatalf("the source was read after Close: got %d reads; want %d", reads, readsAtClose)
	}
	if src.readAfterClose.Load() {
		t.Fatal("the player read the source after it was closed")
	}
	t.Logf("active goroutines after player Close: %d", runtime.NumGoroutine())

	m.Stop()
	time.Sleep(time.Second)
	stopped := runtime.NumGoroutine()
	if got := stopped; got >= playingGoroutines {
		t.Fatalf("the mux's filling loop outlived Mux Stop: %d goroutines remain; want fewer than %d while playing", got, playingGoroutines)
	}
	// A further wait must not bring goroutines back: no leaked loop or reader.
	time.Sleep(time.Second)
	if got := runtime.NumGoroutine(); got != stopped {
		t.Fatalf("active goroutines changed after Mux Stop: got %d; want %d", got, stopped)
	}
	t.Logf("active goroutines after Mux Stop: %d (was %d while playing)", stopped, playingGoroutines)
}

// TestUnplayedSizeContractAcrossReportsAndResets pins the byte accounting
// against a driver's played-frame reports while SetBufferSize, Seek, and
// Reset interleave: buffered and sent-but-unplayed bytes are disjoint sets,
// so UnplayedSize neither double counts nor rolls back, and every reset makes
// the pre-reset bytes disappear from the count.
func TestUnplayedSizeContractAcrossReportsAndResets(t *testing.T) {
	synctest.Test(t, testUnplayedSizeContractAcrossReportsAndResets)
}

func testUnplayedSizeContractAcrossReportsAndResets(t *testing.T) {
	// Six segments leave enough source behind for the re-reads after Seek.
	var pcm []byte
	for id := byte(1); id <= 6; id++ {
		pcm = append(pcm, segmentPCM(id, 2)...)
	}

	m := newMux(t)
	src := newChunkedReader(pcm, bytesPerFrame)
	p := newPlayer(t, m, src)

	p.SetBufferSize(4 * bytesPerFrame)
	p.Play()
	synctest.Wait()

	// Without a driver report, UnplayedSize is exactly the buffered bytes.
	if got, want := p.UnplayedSize(), 4*bytesPerFrame; got != want {
		t.Fatalf("UnplayedSize before any report: got %d; want %d", got, want)
	}

	// Mix two frames (one segment): they leave the buffer and become "sent".
	readFrames(m, 2)
	synctest.Wait() // the loop refills the two consumed frames.
	if got, want := p.BufferedSize(), 4*bytesPerFrame; got != want {
		t.Fatalf("BufferedSize after refilling: got %d; want %d", got, want)
	}

	// The driver reports that frame 4 of the mix is played four frames from
	// now, so two of the eight sent samples are already played.
	m.SetPlayed(4, time.Now().Add(frameTime(4)))
	if got, want := p.UnplayedSize(), 4*bytesPerFrame+bytesPerFrame; got != want {
		t.Fatalf("UnplayedSize at the report: got %d; want %d", got, want)
	}
	// Two frames on, six of the eight sent samples are played.
	time.Sleep(frameTime(2))
	if got, want := p.UnplayedSize(), 4*bytesPerFrame; got != want {
		t.Fatalf("UnplayedSize as the report ages: got %d; want %d", got, want)
	}
	// Once the report is gone, what was sent is kept but only the buffer counts.
	m.ClearPlayed()
	if got, want := p.UnplayedSize(), p.BufferedSize(); got != want {
		t.Fatalf("UnplayedSize after ClearPlayed: got %d; want BufferedSize %d", got, want)
	}

	// Seek discards the buffer and every span sent before it.
	deliveredBeforeSeek, seeksBefore, _ := src.stats()
	if _, err := p.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	synctest.Wait()
	if got, want := p.UnplayedSize(), p.BufferedSize(); got != want {
		t.Fatalf("UnplayedSize after Seek: got %d; want BufferedSize %d", got, want)
	}
	if got, want := p.BufferedSize(), 4*bytesPerFrame; got != want {
		t.Fatalf("BufferedSize after Seek refilled: got %d; want %d", got, want)
	}
	if _, seeks, _ := src.stats(); seeks != seeksBefore+1 {
		t.Fatalf("Seek did not reach the source exactly once: got %d seeks; want %d", seeks, seeksBefore+1)
	}

	// Newly sent bytes are counted, and the count stays within what the source
	// has delivered since the player exists.
	readFrames(m, 2)
	if got := p.UnplayedSize(); got != p.BufferedSize() {
		t.Fatalf("UnplayedSize with no report: got %d; want BufferedSize %d", got, p.BufferedSize())
	}
	_, _, pos := src.stats()
	if got := p.UnplayedSize(); got < 0 || got > int(pos) {
		t.Fatalf("UnplayedSize %d outside [0, delivered %d]", got, pos)
	}
	if deliveredBeforeSeek >= int(pos) {
		t.Fatalf("the player did not re-read the source after Seek: position stayed %d", pos)
	}

	// Reset zeroes both sides of the account.
	p.Reset()
	if got := p.BufferedSize(); got != 0 {
		t.Fatalf("BufferedSize after Reset: got %d; want 0", got)
	}
	if got := p.UnplayedSize(); got != 0 {
		t.Fatalf("UnplayedSize after Reset: got %d; want 0", got)
	}
	// A stale report must not resurrect discarded bytes.
	m.SetPlayed(0, time.Now())
	time.Sleep(frameTime(1))
	if got := p.UnplayedSize(); got != 0 {
		t.Fatalf("UnplayedSize after Reset with a stale report: got %d; want 0", got)
	}
}
