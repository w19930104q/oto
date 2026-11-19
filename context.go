// Copyright 2021 The Oto Authors
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

package oto

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ebitengine/oto/v3/internal/mathutil"
	"github.com/ebitengine/oto/v3/internal/mux"
)

var (
	contextCreated       bool
	contextCreationMutex sync.Mutex
)

// Context is the main object in Oto. It interacts with the audio drivers.
//
// To play sound with Oto, first create a context. Then use the context to create
// an arbitrary number of players. Then use the players to play sound.
//
// Creating multiple contexts is NOT supported.
type Context struct {
	context *context
}

// Format is the format of sources.
type Format int

const (
	// FormatFloat32LE is the format of 32-bit floats in little endian.
	FormatFloat32LE Format = iota

	// FormatUnsignedInt8 is the format of 8-bit integers.
	FormatUnsignedInt8

	// FormatSignedInt16LE is the format of 16-bit integers in little endian.
	FormatSignedInt16LE
)

// NewContextOptions represents options for NewContext.
type NewContextOptions struct {
	// SampleRate specifies the number of samples that should be played during one second.
	// Typical values are 44100 or 48000. One context has only one sample rate. You cannot play multiple audio
	// sources with different sample rates at the same time.
	SampleRate int

	// ChannelCount specifies the number of channels. One channel is mono playback. Two
	// channels are stereo playback. No other values are supported.
	ChannelCount int

	// Format specifies the format of sources.
	Format Format

	// BufferSize specifies a buffer size in the underlying device.
	// BufferSize must not be negative.
	//
	// If 0 is specified, the driver's default buffer size is used.
	// Set BufferSize to adjust the buffer size if you want to adjust latency or reduce noise.
	// A buffer size that is too big increases the latency.
	// On the other hand, a buffer size that is too small can cause glitch noises due to buffer shortage.
	BufferSize time.Duration

	// ApplicationName specifies the name of the client application.
	// It is used for PulseAudio's volume control UI and so on.
	ApplicationName string
}

// NewContext creates a new context with given options.
// The options must not be nil.
// A context creates and holds ready-to-use Player objects.
// NewContext returns a context, a channel that closes when initialization finishes, and an error, if any.
// After the channel closes, call Context.Err to check whether initialization succeeded.
//
// NewContext can be called only once in a process. Any later call returns an error.
func NewContext(options *NewContextOptions) (*Context, chan struct{}, error) {
	if options == nil {
		return nil, nil, fmt.Errorf("oto: options must not be nil")
	}
	if options.SampleRate <= 0 {
		return nil, nil, fmt.Errorf("oto: sample rate must be positive: %d", options.SampleRate)
	}
	if options.ChannelCount != 1 && options.ChannelCount != 2 {
		return nil, nil, fmt.Errorf("oto: channel count must be 1 or 2: %d", options.ChannelCount)
	}
	if options.BufferSize < 0 {
		return nil, nil, fmt.Errorf("oto: buffer size must not be negative: %s", options.BufferSize)
	}

	contextCreationMutex.Lock()
	defer contextCreationMutex.Unlock()

	if contextCreated {
		return nil, nil, fmt.Errorf("oto: context is already created")
	}
	contextCreated = true

	bufferSizeInBytes, err := durationToBufferSize(options.BufferSize, options.SampleRate, options.ChannelCount)
	if err != nil {
		return nil, nil, err
	}
	ctx, ready, err := newContext(options.SampleRate, options.ChannelCount, mux.Format(options.Format), bufferSizeInBytes, options.ApplicationName)
	if err != nil {
		return nil, nil, err
	}
	return &Context{context: ctx}, ready, nil
}

func durationToBufferSize(duration time.Duration, sampleRate, channelCount int) (int, error) {
	if duration == 0 {
		return 0, nil
	}
	if duration < 0 || sampleRate <= 0 || channelCount <= 0 {
		return 0, fmt.Errorf("oto: invalid buffer duration, sample rate, or channel count")
	}

	// The underlying driver always uses 32-bit floats.
	const maxInt = int(^uint(0) >> 1)
	if channelCount > maxInt/4 {
		return 0, fmt.Errorf("oto: buffer frame size exceeds int range")
	}
	bytesPerFrame := channelCount * 4

	frames, ok := mathutil.MulDiv(int64(duration), int64(sampleRate), int64(time.Second))
	if !ok || frames > int64(maxInt/bytesPerFrame) {
		return 0, fmt.Errorf("oto: buffer size exceeds int range")
	}
	return int(frames) * bytesPerFrame, nil
}

// NewPlayer creates a new, ready-to-use Player belonging to the Context.
// It is safe to create multiple players.
//
// The returned player must be kept reachable as long as it should keep playing.
// A player is closed when it becomes unreachable, even in the middle of playing.
//
// The format of r is as follows:
//
//	[data]      = [sample 1] [sample 2] [sample 3] ...
//	[sample *]  = [channel 1] [channel 2] ...
//	[channel *] = [byte 1] [byte 2] ...
//
// Byte ordering is little endian.
//
// A player has some amount of an underlying buffer.
// Read data from r is queued to the player's underlying buffer.
// The underlying buffer is consumed by its playing.
// Then, r's position and the current playing position don't necessarily match.
// If you want to seek the position of r, call the player's Seek function,
// which also clears the underlying buffer.
// If you want to stop using r (e.g. you want to close r), call the player's PauseAndStopReading function.
//
// You cannot share r by multiple players.
// Reads from players' sources are performed sequentially. A Read that blocks
// can prevent other players in the same Context from receiving more data.
//
// The returned player is a *Player, which has functions like SetBufferSize and Seek.
// You can modify the buffer size of a player by the SetBufferSize function.
// A small buffer size is useful if you want to play a real-time PCM for example.
// Note that the audio quality might be affected if you modify the buffer size.
//
// If r does not implement io.Seeker, the returned player's Seek returns an error.
//
// NewPlayer is concurrent-safe.
//
// All the functions of a Player returned by NewPlayer are concurrent-safe.
func (c *Context) NewPlayer(r io.Reader) *Player {
	return &Player{
		player: c.context.mux.NewPlayer(r),
	}
}

// Suspend suspends the entire audio play.
//
// Suspend is concurrent-safe.
func (c *Context) Suspend() error {
	return c.context.Suspend()
}

// Resume resumes the entire audio play, which was suspended by Suspend.
//
// Resume is concurrent-safe.
func (c *Context) Resume() error {
	return c.context.Resume()
}

// Err returns an error that occurred in the audio driver, if any.
// Errors reported by Err are fatal: once Err returns a non-nil error,
// this context is no longer usable.
//
// Err is concurrent-safe.
func (c *Context) Err() error {
	return c.context.Err()
}

// Stats is a snapshot of how the context observes playback: the water level
// of the device buffer, and the drift of the device clock from the wall
// clock.
type Stats struct {
	// MixedFrames is the number of frames the context has mixed and sent to
	// the audio driver.
	MixedFrames int64

	// PlayedKnown reports whether PlayedFrames and BufferedFrames are known:
	// whether the driver reports when frames are played and its report holds.
	PlayedKnown bool

	// PlayedFrames is the number of mixed frames the device has played. It is
	// 0 when PlayedKnown is false.
	PlayedFrames int64

	// BufferedFrames is the water level: the frames sent to the audio driver
	// but not played yet. It is 0 when PlayedKnown is false.
	BufferedFrames int64

	// Drift is the statistics of the device clock's deviation from the wall
	// clock.
	Drift DriftStats
}

// DriftStats is statistics of how fast the device clock runs against the wall
// clock, over the span from the driver's first to its last report.
type DriftStats struct {
	// Reports is the number of the driver's reports the span covers. The span
	// restarts when the device is suspended or the played position goes back.
	Reports int64

	// Ratio is the device clock's rate over the span relative to the nominal
	// sample rate: 1 when the clocks agree, above 1 when the device plays
	// faster. It is computed from the span's ends, so it does not depend on
	// how often the driver reports in between. It is 0 while fewer than two
	// reports are in.
	Ratio float64
}

// Stats returns a snapshot of how the context observes playback: how much of
// the mix is buffered in the device and not played yet, and how the device
// clock drifts from the wall clock. Observing does not affect the played
// audio.
//
// Stats is concurrent-safe.
func (c *Context) Stats() Stats {
	s := c.context.mux.Stats()
	return Stats{
		MixedFrames:    s.MixedFrames,
		PlayedKnown:    s.PlayedKnown,
		PlayedFrames:   s.PlayedFrames,
		BufferedFrames: s.BufferedFrames,
		Drift: DriftStats{
			Reports: s.Drift.Reports,
			Ratio:   s.Drift.Ratio,
		},
	}
}

type atomicError struct {
	err error
	m   sync.Mutex
}

// Join records err in addition to the errors recorded so far. A nil err is
// ignored.
func (a *atomicError) Join(err error) {
	if err == nil {
		return
	}

	a.m.Lock()
	defer a.m.Unlock()
	a.err = errors.Join(a.err, err)
}

func (a *atomicError) Load() error {
	a.m.Lock()
	defer a.m.Unlock()
	return a.err
}
