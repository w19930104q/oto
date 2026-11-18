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
	"math"
	"testing"
	"time"

	"github.com/ebitengine/oto/v3/internal/resample"
)

// sineSource generates interleaved float32 frames of a sine wave at one sample
// per call to Read, keeping the phase so any number of reads reproduce one
// continuous signal.
type sineSource struct {
	sampleRate   int
	channelCount int
	frequency    float64
	phase        float64
}

func (s *sineSource) read(buf []float32) {
	frames := len(buf) / s.channelCount
	for i := range frames {
		v := float32(math.Sin(2 * math.Pi * s.frequency * s.phase / float64(s.sampleRate)))
		s.phase++
		for c := range s.channelCount {
			buf[i*s.channelCount+c] = v
		}
	}
}

// referenceLinear resamples a source sine independently, by evaluating the
// interpolation formula at the same fractional source positions the chain
// walks. It is a second implementation of the specification, not the code
// under test.
func referenceLinear(sourceRate, deviceRate, channelCount, frames int, frequency float64) []float32 {
	out := make([]float32, frames*channelCount)
	ratio := float64(sourceRate) / float64(deviceRate)
	pos := 0.0
	for i := range frames {
		i0 := int(pos)
		frac := float32(pos - float64(i0))
		a := math.Sin(2 * math.Pi * frequency * float64(i0) / float64(sourceRate))
		b := math.Sin(2 * math.Pi * frequency * float64(i0+1) / float64(sourceRate))
		v := float32(a + (b-a)*float64(frac))
		for c := range channelCount {
			out[i*channelCount+c] = v
		}
		pos += ratio
	}
	return out
}

// TestLinearResamplerMatchesReference checks the output of a chain driven in
// varied block sizes against an independently computed linear interpolation.
func TestLinearResamplerMatchesReference(t *testing.T) {
	const (
		sourceRate = 44100
		deviceRate = 48000
		channels   = 2
		frequency  = 1000
		frames     = 48000 // one device second
	)
	for _, block := range []int{1, 7, 128, 1024, 4096} {
		t.Run("", func(t *testing.T) {
			src := &sineSource{sampleRate: sourceRate, channelCount: channels, frequency: frequency}
			chain := resample.NewChain(src.read, func(int64, time.Time) {}, func() {}, sourceRate, deviceRate, channels)

			got := make([]float32, 0, frames*channels)
			buf := make([]float32, block*channels)
			for len(got)/channels < frames {
				chain.Read(buf)
				got = append(got, buf...)
			}
			got = got[:frames*channels]

			want := referenceLinear(sourceRate, deviceRate, channels, frames, frequency)
			var maxErr float64
			for i := range want {
				maxErr = max(maxErr, math.Abs(float64(got[i])-float64(want[i])))
			}
			// Linear interpolation of a 1 kHz sine at 44.1 kHz has errors of a
			// few 1e-4; the two implementations should agree to rounding.
			const tolerance = 1e-5
			if maxErr > tolerance {
				t.Fatalf("block %d: max error %g exceeds tolerance %g", block, maxErr, tolerance)
			}
		})
	}
}

// TestDownsamplingMatchesReference covers the other direction.
func TestDownsamplingMatchesReference(t *testing.T) {
	const (
		sourceRate = 48000
		deviceRate = 44100
		channels   = 1
		frequency  = 440
		frames     = 44100
	)
	src := &sineSource{sampleRate: sourceRate, channelCount: channels, frequency: frequency}
	chain := resample.NewChain(src.read, func(int64, time.Time) {}, func() {}, sourceRate, deviceRate, channels)

	got := make([]float32, 0, frames)
	buf := make([]float32, 512)
	for len(got) < frames {
		chain.Read(buf)
		got = append(got, buf...)
	}
	got = got[:frames]

	want := referenceLinear(sourceRate, deviceRate, channels, frames, frequency)
	var maxErr float64
	for i := range want {
		maxErr = max(maxErr, math.Abs(float64(got[i])-float64(want[i])))
	}
	if maxErr > 1e-5 {
		t.Fatalf("max error %g exceeds tolerance 1e-5", maxErr)
	}
}

// TestMatchingRatesIsByteIdentical requires the matched-rate path to leave
// every source sample untouched, byte for byte, whatever the block size.
func TestMatchingRatesIsByteIdentical(t *testing.T) {
	const rate, channels, frames = 48000, 2, 96000
	src := &sineSource{sampleRate: rate, channelCount: channels, frequency: 1234.5}
	chain := resample.NewChain(src.read, func(int64, time.Time) {}, func() {}, rate, rate, channels)

	want := make([]float32, frames*channels)
	ref := &sineSource{sampleRate: rate, channelCount: channels, frequency: 1234.5}
	ref.read(want)

	got := make([]float32, frames*channels)
	for off := 0; off < len(got); {
		n := min(2048*channels, len(got)-off)
		chain.Read(got[off : off+n])
		off += n
	}
	for i := range got {
		if math.Float32bits(got[i]) != math.Float32bits(want[i]) {
			t.Fatalf("sample %d: got %g, want %g (bytes must be identical)", i, got[i], want[i])
		}
	}
}

// recordingSetPlayed captures the frame the chain forwards to the multiplexer.
type recordingSetPlayed struct {
	frames []int64
}

func (r *recordingSetPlayed) set(frame int64, at time.Time) {
	r.frames = append(r.frames, frame)
}

// TestSetPlayedTranslatesToSourceFrames checks the translation from device
// frames to source frames at the nominal rate, with frames retained in the
// resampler and queued in the device accounted for.
func TestSetPlayedTranslatesToSourceFrames(t *testing.T) {
	const sourceRate, deviceRate, channels = 44100, 48000, 2
	src := &sineSource{sampleRate: sourceRate, channelCount: channels, frequency: 440}
	rec := &recordingSetPlayed{}
	chain := resample.NewChain(src.read, rec.set, func() {}, sourceRate, deviceRate, channels)

	// Output two seconds of device frames in blocks.
	const produced = deviceRate * 2
	buf := make([]float32, 1024*channels)
	for n := 0; n < produced; {
		chain.Read(buf)
		n += len(buf) / channels
	}

	// The device has played half of what was produced.
	const played = produced / 2
	chain.SetPlayed(played, time.Now())
	if len(rec.frames) != 1 {
		t.Fatalf("got %d forwarded reports, want 1", len(rec.frames))
	}
	want := int64(math.Round(float64(played) * float64(sourceRate) / float64(deviceRate)))
	if got := rec.frames[0]; got < want-2 || got > want+2 {
		t.Fatalf("forwarded source frame %d, want about %d", got, want)
	}
}
