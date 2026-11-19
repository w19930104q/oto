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

package oto

import (
	"testing"
	"time"

	"github.com/ebitengine/oto/v3/internal/latencyctl"
)

// TestWASAPILowLatencyBudgets checks the budget derivation stays bounded and
// always keeps the nominal budget at or above the device period.
func TestWASAPILowLatencyBudgets(t *testing.T) {
	cases := []struct {
		name        string
		period      time.Duration
		wantNominal time.Duration
		wantMax     time.Duration
	}{
		{"typical 10ms period", 10 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond},
		{"tiny 5ms period", 5 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond},
		{"coarse 20ms period", 20 * time.Millisecond, 40 * time.Millisecond, 50 * time.Millisecond},
		{"very coarse 30ms period", 30 * time.Millisecond, 60 * time.Millisecond, 60 * time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nominal, max := wasapiLowLatencyBudgets(tc.period)
			if nominal != tc.wantNominal {
				t.Fatalf("nominal = %s, want %s", nominal, tc.wantNominal)
			}
			if max != tc.wantMax {
				t.Fatalf("max = %s, want %s", max, tc.wantMax)
			}
			if nominal < tc.period {
				t.Fatalf("nominal %s below device period %s", nominal, tc.period)
			}
			if max < nominal {
				t.Fatalf("max %s below nominal %s", max, nominal)
			}
		})
	}
}

// TestWASAPIFramesToDuration checks the frame-to-duration conversion used for
// the output-latency statistics.
func TestWASAPIFramesToDuration(t *testing.T) {
	c := &wasapiContext{sampleRate: 48000}
	if got := c.framesToDuration(0); got != 0 {
		t.Fatalf("zero frames = %s, want 0", got)
	}
	if got := c.framesToDuration(480); got != 10*time.Millisecond {
		t.Fatalf("480 frames = %s, want 10ms", got)
	}
	if got := c.framesToDuration(48000); got != time.Second {
		t.Fatalf("48000 frames = %s, want 1s", got)
	}
}

// TestLatencyStatsMapping verifies the internal controller stats are exposed
// through the public LatencyStats without losing fields or units.
func TestLatencyStatsMapping(t *testing.T) {
	controller := latencyctl.NewController(48000, 10*time.Millisecond, 2400, 20*time.Millisecond, 50*time.Millisecond)

	// Drive a healthy stream.
	padding := int64(0)
	for range 100 {
		frames := controller.FramesToCommit(padding)
		queued := padding + frames
		controller.RecordCommit(100*time.Microsecond, queued)
		padding = queued - 480
		if padding < 0 {
			padding = 0
		}
	}

	stats := fromLatencyCtlStats(controller.Snapshot())
	if !stats.LowLatency {
		t.Fatal("mapped stats must report low-latency mode")
	}
	if stats.NominalLatency != 20*time.Millisecond {
		t.Fatalf("nominal = %s, want 20ms", stats.NominalLatency)
	}
	if stats.MaxLatency != 50*time.Millisecond {
		t.Fatalf("max = %s, want 50ms", stats.MaxLatency)
	}
	if stats.CommitLatency.Count == 0 || stats.OutputLatency.Count == 0 {
		t.Fatal("mapped stats must contain measured distributions")
	}
	if stats.Underruns != 0 || stats.Degradations != 0 {
		t.Fatalf("healthy stream must not degrade: underruns=%d degradations=%d", stats.Underruns, stats.Degradations)
	}
}

// TestRegularModeLatencyStatsAreNotLowLatency verifies the regular recorder
// exposes stats marked as the non-low-latency path.
func TestRegularModeLatencyStatsAreNotLowLatency(t *testing.T) {
	var rec latencyctl.Recorder
	rec.RecordCommit(time.Millisecond, 50*time.Millisecond)

	stats := fromLatencyCtlStats(rec.Snapshot(false, 0, 0, 0, 0, 0, 0))
	if stats.LowLatency {
		t.Fatal("regular path must not report low-latency mode")
	}
	if stats.CommitLatency.Count != 1 {
		t.Fatalf("commit count = %d, want 1", stats.CommitLatency.Count)
	}
	if stats.OutputLatency.Count != 1 || stats.OutputLatency.Max != 50*time.Millisecond {
		t.Fatalf("output latency = %+v, want one 50ms sample", stats.OutputLatency)
	}
}
