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

package latencyctl

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Distribution summarizes a sample of durations.
//
// The zero value is ready to use and represents an empty sample.
type Distribution struct {
	Count int64
	Min   time.Duration
	Mean  time.Duration
	P95   time.Duration
	Max   time.Duration
}

// Stats is a point-in-time view of the latency a driver produced.
//
// Durations are zero until at least one sample has been recorded.
type Stats struct {
	// LowLatency reports whether the driver ran the low-latency policy.
	LowLatency bool

	// CommitLatency is the time from the device signaling that it is ready for
	// more data to the driver finishing the commit, i.e. how long a commit was
	// delayed. Without the low-latency policy this delay has no bound.
	CommitLatency Distribution

	// OutputLatency is the amount of committed audio the device had not played
	// yet right after a commit, i.e. the audible latency of the stream.
	OutputLatency Distribution

	// Underruns counts the device events at which the device buffer had already
	// run dry before the commit, meaning the deadline was missed.
	Underruns int64

	// Degradations counts how often the latency budget had to be raised to keep
	// the stream flowing.
	Degradations int64

	// Recoveries counts how often a raised budget returned to the nominal
	// budget after the deadlines were met again.
	Recoveries int64

	// TargetLatency is the latency the commits currently aim for. It grows on
	// degradation and shrinks on recovery.
	TargetLatency time.Duration

	// PeakTargetLatency is the largest commit target reached since the start,
	// i.e. the deepest degradation taken.
	PeakTargetLatency time.Duration

	// NominalLatency is the latency budget the driver started with and tries to
	// return to after a degradation.
	NominalLatency time.Duration

	// MaxLatency is the largest latency the degraded budget can reach.
	MaxLatency time.Duration
}

// Recorder collects the latency samples a driver takes at every commit. It is
// safe for a single recorder to be fed from one render thread and read through
// Snapshot from another; callers needing a consistent multi-field view should
// take their own snapshot rather than reading fields individually.
type Recorder struct {
	mu            sync.Mutex
	commitSamples []time.Duration
	outputSamples []time.Duration
	commitCount   int64
	outputCount   int64
	underruns     int64
}

// sampleCapacity is the number of samples retained per distribution. With
// reservoir sampling the retained set stays uniformly random regardless of the
// process lifetime, so a long-running process keeps bounded memory.
const sampleCapacity = 1024

// RecordCommit records a finished commit: delay is how long the commit took
// after the device event, and outputLatency is the amount of queued, not-yet
// played audio afterwards.
func (r *Recorder) RecordCommit(delay, outputLatency time.Duration) {
	if delay < 0 {
		delay = 0
	}
	if outputLatency < 0 {
		outputLatency = 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commitSamples = reservoir(r.commitSamples, r.commitCount, delay, sampleCapacity)
	r.commitCount++
	r.outputSamples = reservoir(r.outputSamples, r.outputCount, outputLatency, sampleCapacity)
	r.outputCount++
}

// RecordUnderrun records that a device event arrived after the buffer had run
// dry. The current output latency at such an event is recorded too.
func (r *Recorder) RecordUnderrun(outputLatency time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.underruns++
	r.outputSamples = reservoir(r.outputSamples, r.outputCount, outputLatency, sampleCapacity)
	r.outputCount++
}

// Snapshot returns the statistics collected so far. lowLatency and the budget
// fields are copied through as given by the driver.
func (r *Recorder) Snapshot(lowLatency bool, target, peak, nominal, max time.Duration, degradations, recoveries int64) Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Stats{
		LowLatency:        lowLatency,
		CommitLatency:     summarize(r.commitSamples, r.commitCount),
		OutputLatency:     summarize(r.outputSamples, r.outputCount),
		Underruns:         r.underruns,
		Degradations:      degradations,
		Recoveries:        recoveries,
		TargetLatency:     target,
		PeakTargetLatency: peak,
		NominalLatency:    nominal,
		MaxLatency:        max,
	}
}

func reservoir(samples []time.Duration, seen int64, v time.Duration, capacity int) []time.Duration {
	if seen < int64(capacity) {
		return append(samples, v)
	}
	// splitmix64 keeps replacement positions spread out without per-recorder
	// random state.
	pos := int(splitmix64(uint64(seen)) % uint64(capacity))
	samples[pos] = v
	return samples
}

func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

func summarize(samples []time.Duration, count int64) Distribution {
	if count == 0 {
		return Distribution{}
	}

	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	var sum time.Duration
	for _, v := range sorted {
		sum += v
	}
	p95Index := int(math.Ceil(0.95*float64(len(sorted)))) - 1
	if p95Index < 0 {
		p95Index = 0
	}
	return Distribution{
		Count: count,
		Min:   sorted[0],
		Mean:  time.Duration(int64(sum) / int64(len(sorted))),
		P95:   sorted[p95Index],
		Max:   sorted[len(sorted)-1],
	}
}
