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

import "testing"

func TestDarwinOneBufferSizeInBytes(t *testing.T) {
	tests := []struct {
		name              string
		bufferSizeInBytes int
		channelCount      int
		want              int
		wantErr           bool
	}{
		{
			name:              "total size split over the queue buffers",
			bufferSizeInBytes: 4 * 2 * 4,
			channelCount:      2,
			want:              8,
		},
		{
			name:              "rounds down to whole frames",
			bufferSizeInBytes: 4*2*4 + 3,
			channelCount:      2,
			want:              8,
		},
		{
			name:              "one frame per queue is the smallest valid size",
			bufferSizeInBytes: 4 * 2 * 4,
			channelCount:      2,
			want:              8,
		},
		{
			name:              "sub-frame per queue fails instead of becoming zero",
			bufferSizeInBytes: 2*4 - 1,
			channelCount:      2,
			wantErr:           true,
		},
		{
			name:              "mono sub-frame per queue fails instead of becoming zero",
			bufferSizeInBytes: 3,
			channelCount:      1,
			wantErr:           true,
		},
		{
			name:              "mono smallest valid size",
			bufferSizeInBytes: 4 * 4,
			channelCount:      1,
			want:              4,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := darwinOneBufferSizeInBytes(tt.bufferSizeInBytes, tt.channelCount)
			if tt.wantErr {
				if err == nil {
					t.Errorf("darwinOneBufferSizeInBytes returned %d without an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("darwinOneBufferSizeInBytes failed: %v", err)
			}
			if got != tt.want {
				t.Errorf("darwinOneBufferSizeInBytes = %d, want %d", got, tt.want)
			}
		})
	}
}
