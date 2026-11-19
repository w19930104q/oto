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
	"encoding/binary"
	"math"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/ebitengine/oto/v3/internal/mux"
)

func TestStatsWaterLevelMatchesConsumption(t *testing.T) {
	synctest.Test(t, testStatsWaterLevelMatchesConsumption)
}

func testStatsWaterLevelMatchesConsumption(t *testing.T) {
	m := newMux(t)
	p := newBufferedPlayer(t, m, 4800)
	readFrames(m, reportedFrame)
	reportPlayed(m)

	// The reported frame is played a delay from now, so the water level is
	// the delay's worth of frames.
	want := mux.Stats{
		MixedFrames:    reportedFrame,
		PlayedKnown:    true,
		PlayedFrames:   reportedFrame - delay,
		BufferedFrames: delay,
		Drift:          mux.DriftStats{Reports: 1},
	}
	if got := m.Stats(); got != want {
		t.Errorf("at the report: Stats() = %+v; want %+v", got, want)
	}
	// The water level agrees with what the player reports as sent and not
	// played yet.
	if got, want := m.Stats().BufferedFrames*bytesPerFrame, int64(p.UnplayedSize()-p.BufferedSize()); got != want {
		t.Errorf("at the report: BufferedFrames = %d bytes; want UnplayedSize()-BufferedSize(), %d", got, want)
	}

	// The water level sinks as the device consumes.
	time.Sleep(frameTime(400))
	s := m.Stats()
	if got, want := s.PlayedFrames, int64(reportedFrame-delay+400); got != want {
		t.Errorf("400 frames after the report: PlayedFrames = %d; want %d", got, want)
	}
	if got, want := s.BufferedFrames, int64(delay-400); got != want {
		t.Errorf("400 frames after the report: BufferedFrames = %d; want %d", got, want)
	}
	if got, want := s.BufferedFrames*bytesPerFrame, int64(p.UnplayedSize()-p.BufferedSize()); got != want {
		t.Errorf("400 frames after the report: BufferedFrames = %d bytes; want UnplayedSize()-BufferedSize(), %d", got, want)
	}

	// The device runs dry: no more than is mixed is played.
	time.Sleep(frameTime(800))
	if got := m.Stats().BufferedFrames; got != 0 {
		t.Errorf("after the device runs dry: BufferedFrames = %d; want 0", got)
	}
}

func TestStatsWithoutAReport(t *testing.T) {
	synctest.Test(t, testStatsWithoutAReport)
}

func testStatsWithoutAReport(t *testing.T) {
	m := newMux(t)
	newBufferedPlayer(t, m, 4800)
	readFrames(m, 480)

	want := mux.Stats{MixedFrames: 480}
	if got := m.Stats(); got != want {
		t.Errorf("Stats() = %+v; want %+v", got, want)
	}
}

func TestStatsDriftOfAnIdealClock(t *testing.T) {
	synctest.Test(t, testStatsDriftOfAnIdealClock)
}

func testStatsDriftOfAnIdealClock(t *testing.T) {
	m := newMux(t)
	newBufferedPlayer(t, m, 4800)

	m.SetPlayed(0, time.Now())
	time.Sleep(500 * time.Millisecond)
	m.SetPlayed(24000, time.Now())

	s := m.Stats()
	if got, want := s.Drift.Reports, int64(2); got != want {
		t.Errorf("Drift.Reports = %d; want %d", got, want)
	}
	if got := s.Drift.Ratio; got != 1 {
		t.Errorf("Drift.Ratio = %v; want 1", got)
	}
}

// playWithReports plays on m while a driver reports the played frame every
// interval, reading chunk frames for each report as its load. The device
// clock plays rate frames a wall second. It returns the final stats.
func playWithReports(t *testing.T, m *mux.Mux, intervals []time.Duration, chunk int, rate int64) mux.Stats {
	t.Helper()

	m.SetPlayed(0, time.Now())
	var elapsed time.Duration
	for _, d := range intervals {
		time.Sleep(d)
		elapsed += d
		readFrames(m, chunk)
		played := int64(elapsed) * rate / int64(time.Second)
		m.SetPlayed(played, time.Now())
	}
	return m.Stats()
}

func TestStatsDriftIsReproducibleUnderDifferentLoads(t *testing.T) {
	synctest.Test(t, testStatsDriftIsReproducibleUnderDifferentLoads)
}

func testStatsDriftIsReproducibleUnderDifferentLoads(t *testing.T) {
	// A device that plays 48,048 frames a wall second: 1.001 times the
	// nominal rate.
	const rate = 48048

	// A light load: a report every 100 ms, reading 240 frames a time.
	m0 := newMux(t)
	newBufferedPlayer(t, m0, 4800)
	intervals0 := make([]time.Duration, 10)
	for i := range intervals0 {
		intervals0[i] = 100 * time.Millisecond
	}
	s0 := playWithReports(t, m0, intervals0, 240, rate)

	// A heavy load: few reports, reading 960 frames a time.
	m1 := newMux(t)
	newBufferedPlayer(t, m1, 4800)
	s1 := playWithReports(t, m1, []time.Duration{300 * time.Millisecond, 300 * time.Millisecond, 400 * time.Millisecond}, 960, rate)

	// The loads differ, and the stats say so.
	if got, want := s0.MixedFrames, int64(2400); got != want {
		t.Errorf("light load: MixedFrames = %d; want %d", got, want)
	}
	if got, want := s1.MixedFrames, int64(2880); got != want {
		t.Errorf("heavy load: MixedFrames = %d; want %d", got, want)
	}
	if got, want := s0.Drift.Reports, int64(11); got != want {
		t.Errorf("light load: Drift.Reports = %d; want %d", got, want)
	}
	if got, want := s1.Drift.Reports, int64(4); got != want {
		t.Errorf("heavy load: Drift.Reports = %d; want %d", got, want)
	}

	// The drift statistic is the same under both loads.
	if s0.Drift.Ratio != s1.Drift.Ratio {
		t.Errorf("Drift.Ratio = %v under the light load, %v under the heavy load; want the same", s0.Drift.Ratio, s1.Drift.Ratio)
	}
	if got, want := s0.Drift.Ratio, 48048.0/48000; got != want {
		t.Errorf("Drift.Ratio = %v; want %v", got, want)
	}
}

func TestStatsDriftRestartsWhenReportsAreCleared(t *testing.T) {
	synctest.Test(t, testStatsDriftRestartsWhenReportsAreCleared)
}

func testStatsDriftRestartsWhenReportsAreCleared(t *testing.T) {
	m := newMux(t)
	newBufferedPlayer(t, m, 4800)

	// A span of a fast device.
	m.SetPlayed(0, time.Now())
	time.Sleep(time.Second)
	m.SetPlayed(48048, time.Now())

	// As when the device is suspended and resumed.
	m.ClearPlayed()
	m.SetPlayed(0, time.Now())
	time.Sleep(time.Second)
	m.SetPlayed(48000, time.Now())

	// The statistics cover only the reports after the clear.
	s := m.Stats()
	if got, want := s.Drift.Reports, int64(2); got != want {
		t.Errorf("Drift.Reports = %d; want %d", got, want)
	}
	if got := s.Drift.Ratio; got != 1 {
		t.Errorf("Drift.Ratio = %v; want 1", got)
	}
}

func TestStatsDriftRestartsWhenThePlayedFrameGoesBack(t *testing.T) {
	synctest.Test(t, testStatsDriftRestartsWhenThePlayedFrameGoesBack)
}

func testStatsDriftRestartsWhenThePlayedFrameGoesBack(t *testing.T) {
	m := newMux(t)
	newBufferedPlayer(t, m, 4800)

	m.SetPlayed(0, time.Now())
	time.Sleep(time.Second)
	m.SetPlayed(48048, time.Now())

	// The played frame goes back, as on a seek.
	m.SetPlayed(0, time.Now())
	time.Sleep(time.Second)
	m.SetPlayed(48096, time.Now())

	// The statistics cover only the reports after the frame went back.
	s := m.Stats()
	if got, want := s.Drift.Reports, int64(2); got != want {
		t.Errorf("Drift.Reports = %d; want %d", got, want)
	}
	if got, want := s.Drift.Ratio, 48096.0/48000; got != want {
		t.Errorf("Drift.Ratio = %v; want %v", got, want)
	}
}

func TestStatsDoNotChangeTheMix(t *testing.T) {
	synctest.Test(t, testStatsDoNotChangeTheMix)
}

func testStatsDoNotChangeTheMix(t *testing.T) {
	const frames = 4800
	src := func() *bytes.Reader {
		bs := make([]byte, 0, frames*bytesPerFrame)
		for i := range frames * 2 {
			bs = binary.LittleEndian.AppendUint32(bs, math.Float32bits(float32(i)/frames))
		}
		return bytes.NewReader(bs)
	}

	// m0 plays undisturbed. m1 is observed as a monitoring driver would.
	m0 := newMux(t)
	newPlayer(t, m0, src()).Play()
	m1 := newMux(t)
	newPlayer(t, m1, src()).Play()
	synctest.Wait()

	var mixed int64
	for _, chunk := range []int{480, 960, 2400, 960} {
		b0 := make([]float32, chunk*2)
		m0.ReadFloat32s(b0)

		b1 := make([]float32, chunk*2)
		m1.ReadFloat32s(b1)
		mixed += int64(chunk)
		m1.SetPlayed(mixed, time.Now())
		_ = m1.Stats()

		if !slices.Equal(b0, b1) {
			t.Fatalf("the mix differs when observed")
		}
	}
}
