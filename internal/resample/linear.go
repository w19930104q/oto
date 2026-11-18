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

// linearResampler converts frames with linear interpolation. It walks the
// source frames at a fractional position: each output frame takes the source
// frame at the current position and interpolates it with the next one. The
// position advances by the ratio, source frames per device frame, for every
// output frame, so changing the ratio steers the rate without a discontinuity.
type linearResampler struct {
	channelCount int

	// buf holds source frames not consumed yet, whole interleaved frames,
	// starting at head. Appending and dropping a frame each move the bounds, so
	// no frame is copied on the steady path.
	buf  []float32
	head int

	// pos is the position of the next output frame in source frames relative
	// to head. It is in [0, 1) while buf holds a frame ahead.
	pos float64

	// ratio is source frames per device frame.
	ratio float64
}

func newLinearResampler(sourceRate, deviceRate, channelCount int) *linearResampler {
	return &linearResampler{
		channelCount: channelCount,
		ratio:        float64(sourceRate) / float64(deviceRate),
	}
}

func (r *linearResampler) SetRate(ratio float64) {
	r.ratio = ratio
}

func (r *linearResampler) BufferedFrames() int {
	// The frame at pos is interpolated from, not fully consumed; the frames
	// after it are retained.
	return len(r.buf)/r.channelCount - r.head/r.channelCount - int(r.pos)
}

// compact moves the retained frames to the start of buf once the consumed
// prefix is at least as large as the retained part, so capacity stays on the
// order of one block instead of growing over a long playback.
func (r *linearResampler) compact() {
	retained := len(r.buf) - r.head
	if r.head == 0 || r.head < retained {
		return
	}
	n := copy(r.buf, r.buf[r.head:])
	r.buf = r.buf[:n]
	r.head = 0
}

// Process runs the source frames in in through the interpolation.
func (r *linearResampler) Process(in, out []float32) (inFrames, outFrames int) {
	ch := r.channelCount

	if len(in) > 0 {
		r.compact()
		r.buf = append(r.buf, in...)
		inFrames = len(in) / ch
	}

	srcFrames := len(r.buf)/ch - r.head/ch
	want := len(out) / ch
	o := 0
	for o < want {
		i := int(r.pos)
		if i+1 >= srcFrames {
			// No source frame to interpolate toward yet.
			break
		}
		frac := float32(r.pos - float64(i))
		base := r.head + i*ch
		next := base + ch
		for c := range ch {
			a := r.buf[base+c]
			b := r.buf[next+c]
			out[o*ch+c] = a + (b-a)*frac
		}
		o++
		r.pos += r.ratio
		// Skip frames the position has moved past, shifting the position with
		// them so it stays relative to head.
		if d := int(r.pos); d > 0 {
			r.head += d * ch
			r.pos -= float64(d)
			srcFrames -= d
		}
	}
	return inFrames, o
}
