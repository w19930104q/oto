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
	"io"
	"math"
	"math/rand"
	"os"
	"runtime"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/ebitengine/oto/v3/internal/mux"
)

// Long-run stability regressions: playing the same source for a long run must
// produce the same output as a one-shot playback, memory and handles must
// return to the baseline after the run, and the interrupt/recovery path
// (Pause, PauseAndStopReading, Seek) must be equally stable.

const (
	stabilitySampleRate = 48000
	stabilityChannels   = 2
	// stabilitySourceFrames is the length of one playback. The source is
	// deterministic, so a one-shot playback is a valid reference for a long run.
	stabilitySourceFrames = 1024
	// stabilityChunkSamples is how many samples one ReadFloat32s call consumes.
	stabilityChunkSamples = 512
)

// stabilityIterations is how many times the source is played in a long run.
// OTO_STABILITY_ITERATIONS overrides it for longer runs, e.g.:
//
//	OTO_STABILITY_ITERATIONS=100000 go test ./internal/mux/ -run Stability
func stabilityIterations(t *testing.T) int {
	t.Helper()

	const defaultIterations = 200
	s := os.Getenv("OTO_STABILITY_ITERATIONS")
	if s == "" {
		return defaultIterations
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		t.Fatalf("invalid OTO_STABILITY_ITERATIONS %q: %v", s, err)
	}
	return n
}

// stabilitySource returns a deterministic Float32LE stereo source.
func stabilitySource() []byte {
	src := make([]byte, stabilitySourceFrames*stabilityChannels*4)
	r := rand.New(rand.NewSource(0))
	for i := 0; i < stabilitySourceFrames*stabilityChannels; i++ {
		bits := math.Float32bits(r.Float32()*2 - 1)
		for j := range 4 {
			src[4*i+j] = byte(bits >> (8 * j))
		}
	}
	return src
}

// mixAll plays src to the end on a new player of m and returns the mixed samples.
func mixAll(t *testing.T, m *mux.Mux, src []byte) []float32 {
	t.Helper()

	p := m.NewPlayer(bytes.NewReader(src))
	defer func() {
		_ = p.Close()
	}()
	p.SetBufferSize(stabilityChunkSamples * 4)
	p.Play()

	total := len(src) / 4
	buf := make([]float32, stabilityChunkSamples)
	var out []float32
	for len(out) < total {
		want := min(total-len(out), stabilityChunkSamples)
		waitForBufferedSize(t, p, want*4)
		m.ReadFloat32s(buf[:want])
		out = append(out, buf[:want]...)
	}
	return out
}

// resources is a snapshot of the memory and handles the process holds.
type resources struct {
	heapAlloc   uint64
	heapObjects uint64
	goroutines  int
	// fds is the number of open file descriptors, or -1 when the platform
	// does not report it.
	fds int
}

func snapshotResources(t *testing.T) resources {
	t.Helper()

	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return resources{
		heapAlloc:   ms.HeapAlloc,
		heapObjects: ms.HeapObjects,
		goroutines:  runtime.NumGoroutine(),
		fds:         openFDs(t),
	}
}

func openFDs(t *testing.T) int {
	t.Helper()

	if runtime.GOOS != "linux" {
		return -1
	}
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("could not read /proc/self/fd: %v", err)
	}
	return len(ents)
}

// requireResourcesReturned fails the test if the resources did not return to
// the baseline. Memory is allowed a small slack for the runtime's own growth;
// goroutines and file descriptors must not leak at all.
func requireResourcesReturned(t *testing.T, before, after resources) {
	t.Helper()

	t.Logf("resources: heapAlloc %d -> %d, heapObjects %d -> %d, goroutines %d -> %d, fds %d -> %d",
		before.heapAlloc, after.heapAlloc, before.heapObjects, after.heapObjects,
		before.goroutines, after.goroutines, before.fds, after.fds)

	const heapAllocSlack = 1 << 20
	if after.heapAlloc > before.heapAlloc+heapAllocSlack {
		t.Errorf("heap did not return to the baseline: grew by %d bytes; want at most %d",
			after.heapAlloc-before.heapAlloc, heapAllocSlack)
	}
	const heapObjectsSlack = 1024
	if after.heapObjects > before.heapObjects+heapObjectsSlack {
		t.Errorf("heap objects did not return to the baseline: grew by %d; want at most %d",
			after.heapObjects-before.heapObjects, heapObjectsSlack)
	}
	if after.goroutines > before.goroutines {
		t.Errorf("goroutines leaked: got %d; want %d", after.goroutines, before.goroutines)
	}
	if before.fds >= 0 && after.fds != before.fds {
		t.Errorf("file descriptors leaked: got %d; want %d", after.fds, before.fds)
	}
}

func newStabilityMux() *mux.Mux {
	return mux.New(stabilitySampleRate, stabilityChannels, mux.FormatFloat32LE)
}

// A long run of one-shot playbacks must mix the same samples as a single
// playback, however often the source is played.
func TestLongRunOutputConsistent(t *testing.T) {
	iterations := stabilityIterations(t)
	src := stabilitySource()

	m := newStabilityMux()
	defer m.Stop()

	baseline := mixAll(t, m, src)
	if slices.Equal(baseline, make([]float32, len(baseline))) {
		t.Fatal("the baseline playback was silent")
	}

	for i := range iterations {
		if got := mixAll(t, m, src); !slices.Equal(got, baseline) {
			t.Fatalf("iteration %d: the mixed output diverged from the one-shot playback", i)
		}
	}
}

// After a long run, the memory and handles the run held must be released.
func TestLongRunResourcesReturnToBaseline(t *testing.T) {
	iterations := stabilityIterations(t)
	src := stabilitySource()

	goroutinesAtStart := runtime.NumGoroutine()

	m := newStabilityMux()

	// Warm up so that steady-state allocations (e.g. pooled buffers) are
	// populated before the baseline is taken.
	for range min(10, iterations) {
		mixAll(t, m, src)
	}
	before := snapshotResources(t)

	for range iterations {
		mixAll(t, m, src)
	}
	after := snapshotResources(t)
	requireResourcesReturned(t, before, after)

	// Stopping the mux must end its loop goroutine.
	m.Stop()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > goroutinesAtStart {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines did not return to %d after Stop: got %d", goroutinesAtStart, runtime.NumGoroutine())
		}
		time.Sleep(time.Millisecond)
	}
}

// mixWithInterrupts plays src to the end like mixAll, but interrupts the
// playback: it pauses and resumes, stops reading and resumes, or seeks back
// to the start and replays, depending on iteration. The returned samples are
// what one uninterrupted playback of src would produce.
func mixWithInterrupts(t *testing.T, m *mux.Mux, src []byte, iteration int) []float32 {
	t.Helper()

	p := m.NewPlayer(bytes.NewReader(src))
	defer func() {
		_ = p.Close()
	}()
	p.SetBufferSize(stabilityChunkSamples * 4)
	p.Play()

	total := len(src) / 4
	buf := make([]float32, stabilityChunkSamples)
	var out []float32
	sought := false
	for len(out) < total {
		// Interrupt in the middle of the playback.
		if len(out) == total/2 {
			switch iteration % 3 {
			case 0:
				p.Pause()
				p.Play()
			case 1:
				p.PauseAndStopReading()
				// While reading is stopped, the buffer must not change.
				buffered := p.BufferedSize()
				time.Sleep(5 * time.Millisecond)
				if got := p.BufferedSize(); got != buffered {
					t.Fatalf("BufferedSize changed while reading was stopped: got %d; want %d", got, buffered)
				}
				p.Play()
			case 2:
				if !sought {
					sought = true
					if _, err := p.Seek(0, io.SeekStart); err != nil {
						t.Fatal(err)
					}
					// The replay replaces what was mixed before the seek.
					out = out[:0]
					continue
				}
			}
		}
		want := min(total-len(out), stabilityChunkSamples)
		waitForBufferedSize(t, p, want*4)
		m.ReadFloat32s(buf[:want])
		out = append(out, buf[:want]...)
	}
	return out
}

// The interrupt/recovery path must be as stable as uninterrupted playback:
// the output must match a one-shot playback, and the resources must return to
// the baseline after a long run of interrupted playbacks.
func TestLongRunInterruptRecoveryStable(t *testing.T) {
	iterations := stabilityIterations(t)
	src := stabilitySource()

	m := newStabilityMux()
	defer m.Stop()

	baseline := mixAll(t, m, src)

	// Warm up before taking the baseline of the resources.
	for i := range min(10, iterations) {
		mixWithInterrupts(t, m, src, i)
	}
	before := snapshotResources(t)

	for i := range iterations {
		if got := mixWithInterrupts(t, m, src, i); !slices.Equal(got, baseline) {
			t.Fatalf("iteration %d: the mixed output after interrupts diverged from the one-shot playback", i)
		}
	}
	after := snapshotResources(t)
	requireResourcesReturned(t, before, after)
}
