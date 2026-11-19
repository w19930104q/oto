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

package mux

import "testing"

func TestDefaultBufferSize(t *testing.T) {
	const maxInt = int(^uint(0) >> 1)

	tests := []struct {
		name         string
		sampleRate   int
		channelCount int
		format       Format
		want         int
	}{
		{
			name:         "half a second aligned to samples",
			sampleRate:   48000,
			channelCount: 2,
			format:       FormatFloat32LE,
			want:         48000 / 2 * 2 * 4,
		},
		{
			name:         "odd sample rate rounds down to whole frames",
			sampleRate:   44101,
			channelCount: 1,
			format:       FormatFloat32LE,
			want:         44101 / 2 * 1 * 4,
		},
		{
			name:         "extreme sample rate does not overflow int",
			sampleRate:   maxInt,
			channelCount: 2,
			format:       FormatFloat32LE,
			want:         maxInt / 8 * 8,
		},
		{
			name:         "extreme sample rate mono does not overflow int",
			sampleRate:   maxInt,
			channelCount: 1,
			format:       FormatFloat32LE,
			want:         maxInt / 4 * 4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := New(tt.sampleRate, tt.channelCount, tt.format)
			defer m.Stop()
			if got := m.defaultBufferSize(); got != tt.want {
				t.Errorf("defaultBufferSize() = %d, want %d", got, tt.want)
			}
		})
	}
}
