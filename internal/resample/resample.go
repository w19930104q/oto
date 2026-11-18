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

// Package resample converts a stream at one sample rate into a stream at the
// rate an audio device runs at, and keeps the two in step while they run.
//
// A [Chain] sits between the multiplexer, whose stream is at the source sample
// rate, and a driver, whose device runs at its own rate. When the rates match,
// the chain passes the multiplexer's samples through without copying or
// computing anything. When they differ, a [Resampler] does the conversion, and
// the playback reports the driver makes correct the small rate error that an
// actual device clock has, so it cannot accumulate over a long playback.
package resample

import (
	"math"
	"sync"
	"time"
)

// Resampler converts interleaved float32 frames at the source sample rate into
// frames at the device sample rate. A Resampler is used by one Chain at a time
// and is not called concurrently.
//
// Process receives whole source frames for channelCount channels in in and
// room for whole device frames in out. It consumes inFrames of the source
// frames in in, retaining any it does not output, including the frame an
// interpolated sample straddles, and writes outFrames device frames to out.
// in may be empty, in which case a Resampler outputs from the frames it has
// retained. BufferedFrames reports how many source frames are retained.
//
// SetRate changes the conversion ratio, where ratio is source frames per
// device frame, without dropping the stream. It is used to correct clock
// drift, so the values passed differ only slightly from the nominal
// source-to-device ratio.
type Resampler interface {
	Process(in, out []float32) (inFrames, outFrames int)
	BufferedFrames() int
	SetRate(ratio float64)
}

// NewResampler creates a Resampler for one stream. It is a variable so a test
// can inject another implementation.
var NewResampler = func(sourceRate, deviceRate, channelCount int) Resampler {
	return newLinearResampler(sourceRate, deviceRate, channelCount)
}

// driftGain is the fraction of the gap between the current ratio and the ratio
// the latest playback report calls for that each report closes. The loop is a
// first-order low-pass filter on the rate error: small enough that report
// jitter only nudges the pitch, large enough that a real clock error converges
// in a few seconds.
const driftGain = 1.0 / 64

// maxRateCorrection bounds how far the ratio may move from the nominal one. A
// real device clock is off by tens of parts per million at most, so the bound
// only keeps a bogus report from stretching the pitch audibly.
const maxRateCorrection = 0.005

// minReportSpan is the shortest playback a report span must cover before it
// corrects the rate, so a measurement over a handful of frames does not steer
// the pitch on rounding alone.
const minReportSpan = 50 * time.Millisecond

// Chain converts the multiplexer's stream to the device sample rate and tracks
// the device's playback reports to correct clock drift. Its Read, SetPlayed,
// and ClearPlayed methods have the same signatures as the multiplexer's
// methods of the same names, so a driver uses a *Chain wherever it used the
// multiplexer directly. Frame counts in SetPlayed are in device frames, and
// the Chain translates them to source frames before forwarding the report.
//
// A Chain is safe for concurrent use: a driver's render thread calls Read while
// another goroutine reports playback.
type Chain struct {
	// read, setPlayed, and clearPlayed are the multiplexer's methods.
	read        func(buf []float32)
	setPlayed   func(frame int64, at time.Time)
	clearPlayed func()

	channelCount int
	deviceRate   int

	// resampler is nil when the rates match, in which case Read is a direct
	// passthrough and does no work beyond reading the multiplexer.
	resampler Resampler
	// nominalRatio is source frames per device frame at the configured rates.
	nominalRatio float64
	// ratio is the source frames per device frame the resampler currently uses.
	ratio float64

	mu sync.Mutex
	// tmp holds source frames pulled for one resampled Read.
	tmp []float32

	// produced counts the device frames Read has output.
	produced int64
	// consumed counts the source frames read from the multiplexer.
	consumed int64

	// The anchor is the first playback report since the last ClearPlayed. Drift
	// is measured from it, so a suspend does not compare two different runs.
	anchorSet   bool
	anchorFrame int64
	anchorAt    time.Time
}

// Option customizes a Chain.
type Option func(*options)

type options struct {
	// forceResample makes the chain resample even when the configured source
	// and device rates match. It is used in tests to exercise drift correction
	// where a device's true clock differs from its nominal rate.
	forceResample bool
}

// WithoutPassthrough makes the chain resample even when the configured source
// and device rates are equal. It is intended for tests that model a device
// whose true clock differs from its nominal rate.
func WithoutPassthrough() Option {
	return func(o *options) {
		o.forceResample = true
	}
}

// NewChain returns a Chain that reads from read, a multiplexer's
// ReadFloat32s, for a source of sourceRate frames a second and a device of
// deviceRate frames a second, both with channelCount channels. Playback
// reports made with SetPlayed are corrected and forwarded to setPlayed, the
// multiplexer's method, and ClearPlayed is forwarded to clearPlayed.
func NewChain(read func(buf []float32), setPlayed func(frame int64, at time.Time), clearPlayed func(), sourceRate, deviceRate, channelCount int, opts ...Option) *Chain {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	c := &Chain{
		read:         read,
		setPlayed:    setPlayed,
		clearPlayed:  clearPlayed,
		channelCount: channelCount,
		deviceRate:   deviceRate,
		nominalRatio: float64(sourceRate) / float64(deviceRate),
		ratio:        float64(sourceRate) / float64(deviceRate),
	}
	if o.forceResample || sourceRate != deviceRate {
		c.resampler = NewResampler(sourceRate, deviceRate, channelCount)
	}
	return c
}

// Read fills buf with device frames, reading and, where needed, resampling the
// multiplexer's stream. len(buf) must be a multiple of the channel count.
func (c *Chain) Read(buf []float32) {
	if c.resampler == nil {
		// Zero-overhead path: the rates match, so the multiplexer's frames go
		// to the device untouched, with no copy and no extra locking.
		c.read(buf)
		return
	}
	c.readResampled(buf)
}

// readResampled fills buf by pulling source frames from the multiplexer and
// running them through the resampler.
func (c *Chain) readResampled(buf []float32) {
	ch := c.channelCount
	want := len(buf) / ch
	got := 0

	c.mu.Lock()
	defer c.mu.Unlock()

	for got < want {
		// The resampler interpolates one frame ahead, so pull one source frame
		// more than the ratio says the remaining output needs, minus frames it
		// still retains.
		need := int(math.Ceil(float64(want-got)*c.ratio)) + 1 - c.resampler.BufferedFrames()
		if need > 0 {
			if cap(c.tmp) < need*ch {
				c.tmp = make([]float32, need*ch)
			} else {
				c.tmp = c.tmp[:need*ch]
			}
			c.read(c.tmp)
			c.consumed += int64(need)
			_, outFrames := c.resampler.Process(c.tmp, buf[got*ch:])
			got += outFrames
			continue
		}
		_, outFrames := c.resampler.Process(nil, buf[got*ch:])
		if outFrames == 0 {
			// The retained frames cannot fill the output. This cannot happen
			// while the multiplexer keeps supplying frames, as the need
			// computation always leaves one frame to interpolate toward.
			panic("resample: the resampler could not output a frame it had buffered")
		}
		got += outFrames
	}
	c.produced += int64(got)
}

// SetPlayed reports that device frame frame is played at time at, where frame
// 0 is the first device frame Read output. It corrects clock drift from the
// report, translates the frame to the multiplexer's source-frame timeline, and
// forwards it.
func (c *Chain) SetPlayed(frame int64, at time.Time) {
	if c.resampler == nil {
		// The timelines are the same: forward the report unchanged.
		c.setPlayed(frame, at)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Source frames still held by the resampler, and source frames in the
	// device buffers: the produced frames the device has not played yet.
	buffered := int64(c.resampler.BufferedFrames())
	inFlight := float64(c.produced-frame) * c.ratio
	// The source frame the device plays now.
	sourcePlayed := float64(c.consumed-buffered) - inFlight

	// Measure the device's clock directly against wall time: in the time that
	// passed since the first report, a correct device plays elapsed*deviceRate
	// frames. The frames it actually played reveal its rate error, independent
	// of the source and of the current ratio. The first report anchors the
	// measurement.
	if !c.anchorSet {
		c.anchorSet = true
		c.anchorFrame = frame
		c.anchorAt = at
	} else if at.Sub(c.anchorAt) >= minReportSpan {
		elapsed := at.Sub(c.anchorAt).Seconds()
		if elapsed > 0 {
			// rel is positive when the device runs fast: it played more frames
			// in the elapsed wall time than its nominal rate implies.
			rel := float64(frame-c.anchorFrame)/elapsed/float64(c.deviceRate) - 1
			rel = max(-maxRateCorrection, min(maxRateCorrection, rel))
			// A fast device gets fewer source frames per device frame, so the
			// ratio moves opposite to the error.
			target := c.nominalRatio * (1 - rel)
			target = max(c.nominalRatio*(1-maxRateCorrection), min(c.nominalRatio*(1+maxRateCorrection), target))
			c.ratio += driftGain * (target - c.ratio)
			c.resampler.SetRate(c.ratio)
		}
	}

	forwarded := int64(math.Round(sourcePlayed))
	if forwarded < 0 {
		forwarded = 0
	}
	c.setPlayed(forwarded, at)
}

// ClearPlayed drops the last playback report, on a suspend or a device error.
// The corrected ratio stands: the device's clock has not changed. The drift
// anchor restarts, so reports after a resume are not compared with ones from
// before the suspend.
func (c *Chain) ClearPlayed() {
	if c.resampler != nil {
		c.mu.Lock()
		c.anchorSet = false
		c.mu.Unlock()
	}
	c.clearPlayed()
}
