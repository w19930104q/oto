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

package resample

// RatioForTesting reports the current source frames per device frame ratio.
func (c *Chain) RatioForTesting() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ratio
}

// BufferedFramesForTesting reports how many source frames the resampler
// retains. It is zero on the passthrough path.
func (c *Chain) BufferedFramesForTesting() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resampler == nil {
		return 0
	}
	return c.resampler.BufferedFrames()
}
