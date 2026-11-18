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

// virtualDevice models a device whose clock runs off: deviceRate is its
// nominal rate and ppm its clock error in parts per million, positive when the
// device runs fast. It pulls frames through a chain as a driver would and
// reports the frames it has played. Its playback clock is fully virtual, so a
// test does not depend on the host's wall clock.
type virtualDevice struct {
	chain *resample.Chain

	deviceRate int
	ppm        float64

	produced int64
	played   int64
	// exact is the exact frame count the device's true clock has reached; the
	// fractional part carries between blocks and runs.
	exact float64
	// clock is the device's own playback time, advanced by its true rate.
	clock time.Duration
	base  time.Time
}

// run pulls seconds of device audio in 20 ms blocks and reports playback after
// each block, the way ALSA and PulseAudio do. A device with a clock error
// plays block*(1+ppm/1e6) frames per block; the fractional frame carries over
// between blocks, as a real device's frame counter does, instead of being
// rounded away each time.
func (d *virtualDevice) run(seconds int, channels int) {
	if d.base.IsZero() {
		d.base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	nominalBlock := d.deviceRate / 50 // 20 ms at the nominal rate
	buf := make([]float32, (nominalBlock+1)*channels)
	blocks := seconds * 50
	for range blocks {
		d.exact += float64(nominalBlock) * (1 + d.ppm/1e6)
		take := int(math.Round(d.exact - float64(d.produced)))
		d.chain.Read(buf[:take*channels])
		d.produced += int64(take)
		d.played = d.produced
		// The block takes 20 ms of the device's own time regardless of its error.
		d.clock += 20 * time.Millisecond
		d.chain.SetPlayed(d.played, d.base.Add(d.clock))
	}
}

// TestDriftCompensationSlowDevice drives a chain with a device running 1,000
// ppm slow and requires the resampling ratio to converge onto the rate that
// matches the device, instead of letting the error accumulate.
func TestDriftCompensationSlowDevice(t *testing.T) {
	const sourceRate, deviceRate, channels = 48000, 48000, 2
	src := &sineSource{sampleRate: sourceRate, channelCount: channels, frequency: 440}
	chain := resample.NewChain(src.read, func(int64, time.Time) {}, func() {}, sourceRate, deviceRate, channels,
		resample.WithoutPassthrough())

	dev := &virtualDevice{chain: chain, deviceRate: deviceRate, ppm: -1000}
	dev.run(30, channels)

	got := chain.RatioForTesting()
	// A slow device consumes fewer device frames per source second, so the
	// ratio of source frames per device frame rises by about 1,000 ppm.
	want := 1 + 1000.0/1e6
	if math.Abs(got-want)/want > 2e-4 {
		t.Fatalf("ratio %g, want about %g (2e-4 relative)", got, want)
	}
}

// TestDriftCompensationFastDevice is the mirror case.
func TestDriftCompensationFastDevice(t *testing.T) {
	const sourceRate, deviceRate, channels = 48000, 48000, 2
	src := &sineSource{sampleRate: sourceRate, channelCount: channels, frequency: 440}
	chain := resample.NewChain(src.read, func(int64, time.Time) {}, func() {}, sourceRate, deviceRate, channels,
		resample.WithoutPassthrough())

	dev := &virtualDevice{chain: chain, deviceRate: deviceRate, ppm: +500}
	dev.run(30, channels)

	got := chain.RatioForTesting()
	want := 1 - 500.0/1e6
	if math.Abs(got-want)/want > 2e-4 {
		t.Fatalf("ratio %g, want about %g (2e-4 relative)", got, want)
	}
}

// TestNoDriftKeepsTheNominalRatio requires a perfect clock to leave the ratio
// alone, so a correctly running device is never pitch-corrected.
func TestNoDriftKeepsTheNominalRatio(t *testing.T) {
	const sourceRate, deviceRate, channels = 44100, 48000, 2
	src := &sineSource{sampleRate: sourceRate, channelCount: channels, frequency: 440}
	chain := resample.NewChain(src.read, func(int64, time.Time) {}, func() {}, sourceRate, deviceRate, channels)

	dev := &virtualDevice{chain: chain, deviceRate: deviceRate, ppm: 0}
	dev.run(20, channels)

	if got := chain.RatioForTesting(); math.Abs(got-float64(sourceRate)/float64(deviceRate)) > 1e-12 {
		t.Fatalf("ratio drifted to %.12f, want the nominal %.12f", got, float64(sourceRate)/float64(deviceRate))
	}
}

// TestClearPlayedKeepsTheRatio requires a suspend not to undo a correction.
func TestClearPlayedKeepsTheRatio(t *testing.T) {
	const sourceRate, deviceRate, channels = 48000, 48000, 2
	src := &sineSource{sampleRate: sourceRate, channelCount: channels, frequency: 440}
	chain := resample.NewChain(src.read, func(int64, time.Time) {}, func() {}, sourceRate, deviceRate, channels,
		resample.WithoutPassthrough())

	dev := &virtualDevice{chain: chain, deviceRate: deviceRate, ppm: -1000}
	dev.run(10, channels)
	before := chain.RatioForTesting()
	chain.ClearPlayed()
	// A new anchor re-measures from the same clock error, so the ratio keeps
	// correcting in the same direction rather than jumping back to nominal.
	dev.run(5, channels)
	after := chain.RatioForTesting()
	if after <= before {
		t.Fatalf("after ClearPlayed the ratio went from %.10f to %.10f; it must keep correcting toward %.10f, not reset", before, after, 1.001)
	}
	if math.Abs(after-1.001) > 5e-4 {
		t.Fatalf("ratio %.10f is far from the corrected 1.001 after ClearPlayed", after)
	}
}

// TestLongSinePlayback is the acceptance test: a fixed sine run through a
// resampling, drift-correcting chain for a long time must keep producing the
// right signal, must not accumulate drift into a buffer that grows without
// bound, and must keep the rate converged on the device's true clock.
func TestLongSinePlayback(t *testing.T) {
	const (
		sourceRate = 44100
		deviceRate = 48000
		channels   = 2
		frequency  = 1000
		seconds    = 600
	)
	src := &sineSource{sampleRate: sourceRate, channelCount: channels, frequency: float64(frequency)}
	chain := resample.NewChain(src.read, func(int64, time.Time) {}, func() {}, sourceRate, deviceRate, channels)

	// A slow device (negative ppm) needs slightly more source frames per
	// device frame, so the corrected ratio is the nominal one raised by 200 ppm.
	correctedRatio := float64(sourceRate) / float64(deviceRate) * (1 + 200.0/1e6)

	dev := &virtualDevice{chain: chain, deviceRate: deviceRate, ppm: -200}
	dev.base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nominalBlock := deviceRate / 50
	buf := make([]float32, (nominalBlock+1)*channels)
	// zeroStreak is the longest run of consecutive exact-zero samples. A real
	// sine crosses zero only for a sample or two; a dried-up multiplexer
	// produces an endless run of them.
	zeroStreak, streak := 0, 0
	var sumSquares float64
	var n, nonFinite int
	for k := range seconds * 50 {
		dev.exact += float64(nominalBlock) * (1 + dev.ppm/1e6)
		take := int(math.Round(dev.exact - float64(dev.produced)))
		block := buf[:take*channels]
		chain.Read(block)
		dev.produced += int64(take)
		dev.played = dev.produced
		dev.clock += 20 * time.Millisecond
		chain.SetPlayed(dev.played, dev.base.Add(dev.clock))

		for _, v := range block {
			n++
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				nonFinite++
				continue
			}
			if v == 0 {
				streak++
				zeroStreak = max(zeroStreak, streak)
			} else {
				streak = 0
			}
			sumSquares += float64(v) * float64(v)
		}
		if k == 50*30 {
			// After half a minute the correction must have converged onto the
			// slow device's rate, about 200 ppm above the nominal ratio.
			if r := chain.RatioForTesting(); math.Abs(r-correctedRatio)/correctedRatio > 2e-4 {
				t.Fatalf("ratio %g after 30s, want about %g", r, correctedRatio)
			}
		}
	}

	if nonFinite != 0 {
		t.Fatalf("%d non-finite samples in the long playback", nonFinite)
	}
	// Ten zeroed frames is 0.2 ms at 48 kHz; far longer than any zero crossing
	// of a 1 kHz sine, and the signature of an underrun.
	if zeroStreak > 10*channels {
		t.Fatalf("%d consecutive zero samples: the source ran dry", zeroStreak)
	}
	rms := math.Sqrt(sumSquares / float64(n))
	// The RMS of a unit sine is 1/sqrt(2). Resampling a 1 kHz tone changes it
	// only slightly; require a tight band around the expected value.
	if math.Abs(rms-1/math.Sqrt2) > 0.02 {
		t.Fatalf("RMS %g is far from the sine's %.4f: the signal degraded over the long run", rms, 1/math.Sqrt2)
	}
	// After ten minutes the ratio must still hold the correction rather than
	// drifting back, proving the error does not accumulate.
	if r := chain.RatioForTesting(); math.Abs(r-correctedRatio)/correctedRatio > 2e-4 {
		t.Fatalf("ratio %g after %ds, want about %g: drift accumulated", r, seconds, correctedRatio)
	}
}

// TestResamplerBufferStaysBounded runs a long playback and requires the frames
// the resampler retains to stay at a small, fixed bound instead of growing with
// the amount of audio processed, which would leak memory and add latency.
func TestResamplerBufferStaysBounded(t *testing.T) {
	const sourceRate, deviceRate, channels = 44100, 48000, 2
	src := &sineSource{sampleRate: sourceRate, channelCount: channels, frequency: 440}
	chain := resample.NewChain(src.read, func(int64, time.Time) {}, func() {}, sourceRate, deviceRate, channels)

	// The interpolation never needs more than a couple of source frames beyond
	// the current one, so allow a generous bound well under one millisecond.
	const bound = deviceRate / 100
	buf := make([]float32, 1024*channels)
	maxSeen := 0
	for range 60 * 50 {
		chain.Read(buf)
		if b := chain.BufferedFramesForTesting(); b > maxSeen {
			maxSeen = b
		}
	}
	if maxSeen > bound {
		t.Fatalf("the resampler retained %d frames, more than the bound of %d: its buffer grows without limit", maxSeen, bound)
	}
}
