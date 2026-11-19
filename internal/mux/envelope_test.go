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
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/ebitengine/oto/v3/internal/mux"
)

// float32LEBytes returns samples samples of value as FormatFloat32LE data.
func float32LEBytes(value float32, samples int) []byte {
	bs := make([]byte, 4*samples)
	bits := math.Float32bits(value)
	for i := range samples {
		bs[4*i] = byte(bits)
		bs[4*i+1] = byte(bits >> 8)
		bs[4*i+2] = byte(bits >> 16)
		bs[4*i+3] = byte(bits >> 24)
	}
	return bs
}

// newFullPlayer creates a playing player whose source is data, buffered in
// full. As the source has ended, the buffer is never refilled, and mixing the
// player is deterministic.
func newFullPlayer(t *testing.T, m *mux.Mux, data []byte) *mux.Player {
	t.Helper()

	p := newPlayer(t, m, bytes.NewReader(data))
	p.SetBufferSize(len(data))
	p.Play()
	waitForBufferedSize(t, p, len(data))
	return p
}

// readChunks reads from m in chunks of the given sizes and returns the
// concatenated samples.
func readChunks(m *mux.Mux, chunks ...int) []float32 {
	var out []float32
	for _, c := range chunks {
		buf := make([]float32, c)
		m.ReadFloat32s(buf)
		out = append(out, buf...)
	}
	return out
}

func TestGainEnvelopeCurve(t *testing.T) {
	const samples = 96

	// The envelope ramps from 0 to 1 over 48 frames (1ms at 48kHz) and then
	// holds 1.
	m := mux.New(48000, 1, mux.FormatFloat32LE)
	p := newFullPlayer(t, m, float32LEBytes(1, samples))
	p.SetGainEnvelope([]mux.EnvelopePoint{
		{At: 0, Gain: 0},
		{At: time.Millisecond, Gain: 1},
	})

	buf := readChunks(m, samples)
	for i, got := range buf {
		want := float32(1)
		if i < 48 {
			want = float32(float64(i) / 48)
		}
		if diff := math.Abs(float64(got - want)); diff > 1e-6 {
			t.Fatalf("sample %d: got %v; want %v", i, got, want)
		}
	}
}

// The gain curve must depend only on the mixed output, not on how the output
// is chunked into buffers, and must change smoothly without jumps.
func TestGainEnvelopeIsChunkIndependentAndSmooth(t *testing.T) {
	const samples = 4800

	envelope := []mux.EnvelopePoint{
		{At: 0, Gain: 0},
		{At: 50 * time.Millisecond, Gain: 1},
		{At: 75 * time.Millisecond, Gain: 0.25},
	}

	// The steepest slope of the envelope is 0.75 over 1200 frames.
	const maxSlope = 0.75 / 1200

	renders := map[string][]int{
		"at once":          {samples},
		"even":             {1200, 1200, 1200, 1200},
		"uneven":           {7, 13, 256, 1024, 3500},
		"sample by sample": nil, // Filled below.
	}
	ones := make([]int, 0, samples)
	for range samples {
		ones = append(ones, 1)
	}
	renders["sample by sample"] = ones

	var reference []float32
	for name, chunks := range renders {
		m := mux.New(48000, 1, mux.FormatFloat32LE)
		p := newFullPlayer(t, m, float32LEBytes(1, samples))
		p.SetGainEnvelope(envelope)
		got := readChunks(m, chunks...)

		if reference == nil {
			reference = got
		} else {
			for i := range got {
				if got[i] != reference[i] {
					t.Fatalf("%s: sample %d: got %v; want %v (the gain curve depends on the chunk sizes)", name, i, got[i], reference[i])
				}
			}
		}

		for i := 1; i < len(got); i++ {
			if diff := math.Abs(float64(got[i] - got[i-1])); diff > maxSlope+1e-6 {
				t.Fatalf("%s: the gain jumped by %v between samples %d and %d; want at most %v", name, diff, i-1, i, maxSlope)
			}
		}
	}
}

// Replacing an envelope must continue from the gain the previous envelope
// reached, without a jump.
func TestGainEnvelopeRestartIsContinuous(t *testing.T) {
	const samples = 48

	m := mux.New(48000, 1, mux.FormatFloat32LE)
	p := newFullPlayer(t, m, float32LEBytes(1, 2*samples))
	p.SetGainEnvelope([]mux.EnvelopePoint{
		{At: 0, Gain: 0},
		{At: time.Millisecond, Gain: 0.5},
	})

	first := readChunks(m, samples)
	// The first envelope reached 0.5.
	if got, want := first[samples-1], float32(0.5*47.0/48.0); got != want {
		t.Fatalf("last sample of the first envelope: got %v; want %v", got, want)
	}

	// The second envelope ramps from the gain the first one reached, 0.5, to 1.
	p.SetGainEnvelope([]mux.EnvelopePoint{
		{At: time.Millisecond, Gain: 1},
	})
	second := readChunks(m, samples)
	for i, got := range second {
		want := float32(0.5 + 0.5*float64(i)/48)
		if diff := math.Abs(float64(got - want)); diff > 1e-6 {
			t.Fatalf("sample %d of the second envelope: got %v; want %v", i, got, want)
		}
	}
	if diff := math.Abs(float64(second[0] - first[samples-1])); diff > 0.5/48+1e-6 {
		t.Fatalf("the gain jumped by %v when the envelope was replaced", diff)
	}
}

// The samples of one frame share the frame's envelope gain.
func TestGainEnvelopeStereo(t *testing.T) {
	const frames = 96

	m := mux.New(48000, 2, mux.FormatFloat32LE)
	p := newFullPlayer(t, m, float32LEBytes(1, 2*frames))
	p.SetGainEnvelope([]mux.EnvelopePoint{
		{At: 0, Gain: 0},
		{At: time.Millisecond, Gain: 1},
	})

	buf := readChunks(m, 2*frames)
	for f := range frames {
		if buf[2*f] != buf[2*f+1] {
			t.Fatalf("frame %d: the channels have different gains: %v and %v", f, buf[2*f], buf[2*f+1])
		}
		want := float32(1)
		if f < 48 {
			want = float32(float64(f) / 48)
		}
		if diff := math.Abs(float64(buf[2*f] - want)); diff > 1e-6 {
			t.Fatalf("frame %d: got %v; want %v", f, buf[2*f], want)
		}
	}
}

func TestGainEnvelopeSanitizesPoints(t *testing.T) {
	t.Run("invalid gains are 0", func(t *testing.T) {
		const samples = 5 * 48

		m := mux.New(48000, 1, mux.FormatFloat32LE)
		p := newFullPlayer(t, m, float32LEBytes(1, samples))
		p.SetGainEnvelope([]mux.EnvelopePoint{
			{At: 0, Gain: math.NaN()},
			{At: time.Millisecond, Gain: -1},
			{At: 2 * time.Millisecond, Gain: math.Inf(1)},
			{At: 3 * time.Millisecond, Gain: 1e39},
			{At: 4 * time.Millisecond, Gain: 0.5},
		})

		buf := readChunks(m, samples)
		for i, got := range buf {
			var want float32
			switch {
			case i >= 4*48:
				want = 0.5
			case i >= 3*48:
				// The ramp from the 0 at 3ms to the 0.5 at 4ms.
				want = float32(0.5 * float64(i-3*48) / 48)
			}
			if diff := math.Abs(float64(got - want)); diff > 1e-6 {
				t.Fatalf("sample %d: got %v; want %v", i, got, want)
			}
		}
	})

	t.Run("negative time is the start", func(t *testing.T) {
		m := mux.New(48000, 1, mux.FormatFloat32LE)
		p := newFullPlayer(t, m, float32LEBytes(1, 16))
		p.SetGainEnvelope([]mux.EnvelopePoint{
			{At: -time.Second, Gain: 0.3},
		})

		buf := readChunks(m, 16)
		for i, got := range buf {
			if want := float32(0.3); got != want {
				t.Fatalf("sample %d: got %v; want %v", i, got, want)
			}
		}
	})

	t.Run("no points clears the envelope", func(t *testing.T) {
		m := mux.New(48000, 1, mux.FormatFloat32LE)
		p := newFullPlayer(t, m, float32LEBytes(1, 32))
		p.SetGainEnvelope([]mux.EnvelopePoint{
			{At: 0, Gain: 0},
		})
		if got := readChunks(m, 16)[0]; got != 0 {
			t.Fatalf("sample with a 0 envelope: got %v; want 0", got)
		}
		p.SetGainEnvelope(nil)
		for i, got := range readChunks(m, 16) {
			if got != 1 {
				t.Fatalf("sample %d after clearing the envelope: got %v; want 1", i, got)
			}
		}
	})
}

// The envelope's clock is the player's output: it does not advance while the
// player is paused.
func TestGainEnvelopePausesWithPlayer(t *testing.T) {
	const samples = 96

	m := mux.New(48000, 1, mux.FormatFloat32LE)
	p := newFullPlayer(t, m, float32LEBytes(1, samples))
	p.SetGainEnvelope([]mux.EnvelopePoint{
		{At: 0, Gain: 0},
		{At: 2 * time.Millisecond, Gain: 1},
	})

	first := readChunks(m, samples/2)
	p.Pause()
	for i, got := range readChunks(m, samples/2) {
		if got != 0 {
			t.Fatalf("a paused player played: sample %d: got %v; want silence", i, got)
		}
	}
	p.Play()
	second := readChunks(m, samples/2)

	got := append(first, second...)
	for i, g := range got {
		want := float32(float64(i) / 96)
		if diff := math.Abs(float64(g - want)); diff > 1e-6 {
			t.Fatalf("sample %d: got %v; want %v (the envelope advanced while paused)", i, g, want)
		}
	}
}

// The envelope gain is multiplied with the volume.
func TestGainEnvelopeMultipliesVolume(t *testing.T) {
	m := mux.New(48000, 1, mux.FormatFloat32LE)
	p := newPlayer(t, m, bytes.NewReader(float32LEBytes(1, 16)))
	p.SetBufferSize(16 * 4)
	// Set the volume and the envelope before playing so that the volume is
	// not ramped while mixing.
	p.SetVolume(0.5)
	p.SetGainEnvelope([]mux.EnvelopePoint{
		{At: 0, Gain: 0.5},
	})
	p.Play()
	waitForBufferedSize(t, p, 16*4)

	for i, got := range readChunks(m, 16) {
		if want := float32(0.25); got != want {
			t.Fatalf("sample %d: got %v; want %v", i, got, want)
		}
	}
}

// The mixed output of the players must be the sum of what each player plays
// by itself, for any number of players and any gain curves.
func TestMixedOutputEqualsSumOfSoloRenders(t *testing.T) {
	const samples = 4800

	chunks := []int{777, 1234, 2789}

	for _, playerCount := range []int{1, 2, 3, 5, 8, 16} {
		t.Run(fmt.Sprintf("players=%d", playerCount), func(t *testing.T) {
			// Each player has its own constant sample, volume, and gain
			// curve, including multi-segment envelopes and no envelope.
			type setup struct {
				value    float32
				volume   float64
				envelope []mux.EnvelopePoint
			}
			setups := make([]setup, playerCount)
			for i := range setups {
				setups[i] = setup{
					value:  float32(0.05 * float64(i%5+1)),
					volume: 0.5 + 0.1*float64(i%7),
				}
				switch i % 3 {
				case 0:
					setups[i].envelope = []mux.EnvelopePoint{
						{At: 0, Gain: 0},
						{At: time.Duration(10+i) * time.Millisecond, Gain: 1},
					}
				case 1:
					setups[i].envelope = []mux.EnvelopePoint{
						{At: 5 * time.Millisecond, Gain: 0.5},
						{At: time.Duration(30+i) * time.Millisecond, Gain: 0.25},
						{At: 80 * time.Millisecond, Gain: 0.75},
					}
				}
			}

			play := func() []float32 {
				m := mux.New(48000, 1, mux.FormatFloat32LE)
				players := make([]*mux.Player, playerCount)
				for i, s := range setups {
					p := newFullPlayer(t, m, float32LEBytes(s.value, samples))
					p.SetVolume(s.volume)
					if s.envelope != nil {
						p.SetGainEnvelope(s.envelope)
					}
					players[i] = p
				}
				// Change a volume in the middle of the render, at the same
				// position in every render.
				first := readChunks(m, chunks[0])
				players[0].SetVolume(0.3)
				rest := readChunks(m, chunks[1:]...)
				return append(first, rest...)
			}

			mixed := play()

			// Render each player alone and sum the renders.
			solo := make([]float64, samples)
			for i := range setups {
				m := mux.New(48000, 1, mux.FormatFloat32LE)
				s := setups[i]
				p := newFullPlayer(t, m, float32LEBytes(s.value, samples))
				p.SetVolume(s.volume)
				if s.envelope != nil {
					p.SetGainEnvelope(s.envelope)
				}
				first := readChunks(m, chunks[0])
				if i == 0 {
					p.SetVolume(0.3)
				}
				rest := readChunks(m, chunks[1:]...)
				for j, v := range append(first, rest...) {
					solo[j] += float64(v)
				}
			}

			for i, got := range mixed {
				if diff := math.Abs(float64(got) - solo[i]); diff > 1e-4 {
					t.Fatalf("sample %d: mixed %v; want the sum of the solo renders %v", i, got, solo[i])
				}
			}
		})
	}
}
