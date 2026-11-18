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

package resample_test

import (
	"bytes"
	"encoding/binary"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/ebitengine/oto/v3/internal/mux"
	"github.com/ebitengine/oto/v3/internal/resample"
)

// recordingResampler is an injected Resampler that records the ratios it is
// given and passes frames through unchanged, so a test can assert the chain
// drives an injected implementation.
type recordingResampler struct {
	channelCount int
	ratios       []float64
	mu           sync.Mutex
}

func (r *recordingResampler) Process(in, out []float32) (int, int) {
	n := min(len(in), len(out))
	copy(out, in[:n])
	return len(in) / r.channelCount, n / r.channelCount
}

func (r *recordingResampler) BufferedFrames() int { return 0 }

func (r *recordingResampler) SetRate(ratio float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ratios = append(r.ratios, ratio)
}

// TestNewResamplerIsInjectable checks that replacing NewResampler makes the
// chain use the injected implementation and that drift correction reaches it
// through SetRate.
func TestNewResamplerIsInjectable(t *testing.T) {
	orig := resample.NewResampler
	defer func() { resample.NewResampler = orig }()

	var injected *recordingResampler
	resample.NewResampler = func(sourceRate, deviceRate, channelCount int) resample.Resampler {
		injected = &recordingResampler{channelCount: channelCount}
		return injected
	}

	const sourceRate, deviceRate, channels = 48000, 48000, 2
	src := &sineSource{sampleRate: sourceRate, channelCount: channels, frequency: 440}
	chain := resample.NewChain(src.read, func(int64, time.Time) {}, func() {}, sourceRate, deviceRate, channels,
		resample.WithoutPassthrough())
	if injected == nil {
		t.Fatal("the injected NewResampler was not used")
	}

	dev := &virtualDevice{chain: chain, deviceRate: deviceRate, ppm: -1000}
	dev.run(5, channels)

	injected.mu.Lock()
	got := len(injected.ratios)
	injected.mu.Unlock()
	if got == 0 {
		t.Fatal("SetRate was never called on the injected resampler")
	}
}

// TestChainDrivesAMux runs the whole pipeline a driver uses: a multiplexer with
// a playing source, converted by a chain for a device at another rate, reporting
// frames back through the chain into the multiplexer. It checks that the
// multiplexer receives translated, non-negative playback reports.
func TestChainDrivesAMux(t *testing.T) {
	const sourceRate, deviceRate, channels = 44100, 48000, 2
	mx := mux.New(sourceRate, channels, mux.FormatFloat32LE)
	t.Cleanup(mx.Stop)

	// A finite source of a quiet sine.
	const seconds = 2
	data := make([]byte, sourceRate*seconds*channels*4)
	for i := range sourceRate * seconds {
		v := math.Float32bits(0.5 * float32(math.Sin(2*math.Pi*440*float64(i)/sourceRate)))
		for c := range channels {
			binary.LittleEndian.PutUint32(data[(i*channels+c)*4:], v)
		}
	}
	p := mx.NewPlayer(bytes.NewReader(data))
	t.Cleanup(func() { _ = p.Close() })
	p.Play()

	var lastReported int64
	var repMu sync.Mutex
	chain := resample.NewChain(mx.ReadFloat32s,
		func(frame int64, at time.Time) {
			repMu.Lock()
			lastReported = frame
			repMu.Unlock()
			mx.SetPlayed(frame, at)
		},
		mx.ClearPlayed, sourceRate, deviceRate, channels)

	// Pull two device seconds in 20 ms blocks, reporting each as fully played.
	block := deviceRate / 50
	buf := make([]float32, block*channels)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var played int64
	for k := 1; k <= seconds*50; k++ {
		chain.Read(buf)
		played += int64(block)
		chain.SetPlayed(played, base.Add(time.Duration(k*20)*time.Millisecond))
	}

	repMu.Lock()
	got := lastReported
	repMu.Unlock()
	// Two device seconds at 48 kHz correspond to about two source seconds at
	// 44.1 kHz, allowing for frames retained in the resampler.
	want := int64(float64(played) * sourceRate / deviceRate)
	if got < 0 {
		t.Fatalf("the multiplexer got a negative played frame %d", got)
	}
	if d := got - want; d < -10 || d > 10 {
		t.Fatalf("reported source frame %d, want about %d", got, want)
	}
}
