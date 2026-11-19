// Copyright 2022 The Oto Authors
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
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/ebitengine/oto/v3/internal/latencyctl"
	"github.com/ebitengine/oto/v3/internal/mathutil"
	"github.com/ebitengine/oto/v3/internal/mux"
)

type comThread struct {
	funcCh chan func()
	done   chan struct{}

	stopped bool
	m       sync.Mutex
}

func newCOMThread() (*comThread, error) {
	funcCh := make(chan func())
	errCh := make(chan error)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		// S_FALSE is returned when CoInitializeEx is nested. This is a successful case.
		if err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err != nil && !errors.Is(err, syscall.Errno(windows.S_FALSE)) {
			errCh <- err
			return
		}
		// CoUninitialize should be called even when CoInitializeEx returns S_FALSE.
		defer windows.CoUninitialize()

		close(errCh)

		for f := range funcCh {
			f()
		}
	}()

	if err := <-errCh; err != nil {
		return nil, err
	}

	return &comThread{
		funcCh: funcCh,
		done:   done,
	}, nil
}

// Run calls f on the COM thread and waits for it. f is not called after Stop.
func (c *comThread) Run(f func()) {
	c.m.Lock()
	defer c.m.Unlock()

	if c.stopped {
		return
	}

	ch := make(chan struct{})
	c.funcCh <- func() {
		f()
		close(ch)
	}
	<-ch
}

// Stop terminates the COM thread. Stop must not be called on the COM thread.
func (c *comThread) Stop() {
	c.m.Lock()
	defer c.m.Unlock()

	if c.stopped {
		return
	}
	c.stopped = true
	close(c.funcCh)
	<-c.done
}

type wasapiContext struct {
	sampleRate        int
	channelCount      int
	mux               *mux.Mux
	bufferSizeInBytes int
	lowLatency        bool

	comThread     *comThread
	err           atomicError
	suspended     bool
	suspendedCond *sync.Cond

	sampleReadyEvent windows.Handle
	client           *_IAudioClient2
	bufferFrames     uint32
	renderClient     *_IAudioRenderClient
	currentDeviceID  string
	enumerator       *_IMMDeviceEnumerator
	oomRetryCount    int

	// devicePeriod is the interval between the device's ready events, captured
	// at initialization in low-latency mode.
	devicePeriod time.Duration

	// controller drives the low-latency commit policy and holds its statistics.
	controller *latencyctl.Controller

	// latencyRecorder is kept for the whole context lifetime and shared by every
	// controller created across device restarts, so the latency statistics are
	// not reset when the device is re-acquired.
	latencyRecorder *latencyctl.Recorder

	// recorder collects latency statistics in the regular commit path, so both
	// modes can be accepted against the same measurements.
	recorder latencyctl.Recorder

	// committedMixed is the mux's global sample index just after the last
	// commit. Together with the device padding it yields the frame currently
	// playing, and stays correct across a device restart because the mux keeps
	// counting while the device is drained.
	committedMixed int64

	buf []float32

	m sync.Mutex
}

var (
	errDeviceSwitched = errors.New("oto: device switched")
)

const (
	wasapiOOMRetryLimit = 3

	// wasapiRestartRetryLimit caps the transient-error re-acquisition attempts
	// before the error is surfaced. With the backoff intervals below, this is
	// roughly ten seconds of continuous outage.
	wasapiRestartRetryLimit = 24

	// wasapiReacquireMinInterval and wasapiReacquireMaxInterval bound the backoff
	// between device re-acquisition attempts, each a full COM round trip. Mux
	// draining continues at real time regardless.
	wasapiReacquireMinInterval = 50 * time.Millisecond
	wasapiReacquireMaxInterval = 500 * time.Millisecond

	// wasapiLowLatencyMinBuffer and wasapiLowLatencyMaxBuffer bound the device
	// buffer requested in low-latency mode. Three device periods (typically
	// ~30ms) leave one period of headroom over the nominal two-period budget.
	wasapiLowLatencyMinBuffer = 20 * time.Millisecond
	wasapiLowLatencyMaxBuffer = 50 * time.Millisecond
)

// wasapiLowLatencyBudgets returns the nominal and maximum latency budgets for
// low-latency mode, derived from the device's signaling period. The nominal
// budget keeps two periods queued; the maximum, used after degradation, is the
// whole device buffer.
func wasapiLowLatencyBudgets(period time.Duration) (nominal, max time.Duration) {
	nominal = 2 * period
	if nominal < wasapiLowLatencyMinBuffer {
		nominal = wasapiLowLatencyMinBuffer
	}
	max = wasapiLowLatencyMaxBuffer
	if max < nominal {
		max = nominal
	}
	return nominal, max
}

func newWASAPIContext(sampleRate, channelCount int, mux *mux.Mux, bufferSizeInBytes int, lowLatency bool) (context *wasapiContext, ferr error) {
	t, err := newCOMThread()
	if err != nil {
		return nil, err
	}

	c := &wasapiContext{
		sampleRate:        sampleRate,
		channelCount:      channelCount,
		mux:               mux,
		bufferSizeInBytes: bufferSizeInBytes,
		lowLatency:        lowLatency,
		comThread:         t,
		suspendedCond:     sync.NewCond(&sync.Mutex{}),
	}
	if lowLatency {
		c.latencyRecorder = &latencyctl.Recorder{}
	}

	defer func() {
		if ferr != nil {
			c.close()
		}
	}()

	ev, err := windows.CreateEventEx(nil, nil, 0, windows.EVENT_ALL_ACCESS)
	if err != nil {
		return nil, err
	}
	c.sampleReadyEvent = ev

	if err := c.start(); err != nil {
		return nil, err
	}

	return c, nil
}

func (c *wasapiContext) isDeviceSwitched() (bool, error) {
	// If the audio is suspended, do nothing.
	if c.isSuspended() {
		return false, nil
	}

	var switched bool
	var cerr error
	c.comThread.Run(func() {
		device, err := c.enumerator.GetDefaultAudioEndpoint(eRender, eConsole)
		if err != nil {
			if errors.Is(err, _E_NOTFOUND) {
				// No default endpoint can be temporary while devices are switching.
				cerr = errDeviceSwitched
				return
			}
			cerr = err
			return
		}
		defer device.Release()

		id, err := device.GetId()
		if err != nil {
			cerr = err
			return
		}

		if c.currentDeviceID == id {
			return
		}
		switched = true
	})

	return switched, cerr
}

func (c *wasapiContext) start() error {
	if err := c.initialize(); err != nil {
		return err
	}
	go c.run()
	return nil
}

func (c *wasapiContext) initialize() error {
	var cerr error
	c.comThread.Run(func() {
		cerr = c.startOnCOMThread()
	})
	return cerr
}

func (c *wasapiContext) run() {
	// This goroutine owns the resources across all recovery attempts. The render
	// loop has returned before cleanup can close the event or release interfaces.
	defer c.close()

	for {
		err := c.loop()
		c.comThread.Run(c.releaseOnCOMThread)
		if err == nil {
			return
		}
		// E_OUTOFMEMORY from IAudioRenderClient::GetBuffer has been observed on Xbox.
		// The counter is reset after a successful buffer write, so this cap applies
		// to consecutive failures without intervening progress.
		if errors.Is(err, _E_OUTOFMEMORY) {
			if c.oomRetryCount >= wasapiOOMRetryLimit {
				c.err.Join(err)
				return
			}
			c.oomRetryCount++
		} else if !errors.Is(err, _AUDCLNT_E_DEVICE_INVALIDATED) && !errors.Is(err, _AUDCLNT_E_RESOURCES_INVALIDATED) && !errors.Is(err, errDeviceSwitched) && !errors.Is(err, _RPC_E_DISCONNECTED) {
			c.err.Join(err)
			return
		}

		if err := c.restart(); err != nil {
			c.err.Join(err)
			return
		}
	}
}

// close must be called outside the COM thread after rendering has ended.
func (c *wasapiContext) close() {
	c.comThread.Run(c.releaseOnCOMThread)
	if c.sampleReadyEvent != 0 {
		windows.CloseHandle(c.sampleReadyEvent)
		c.sampleReadyEvent = 0
	}
	c.comThread.Stop()
}

func (c *wasapiContext) releaseOnCOMThread() {
	if c.client != nil {
		_, _ = c.client.Stop()
	}
	// The render service must be released on the thread that acquired it,
	// before releasing the audio client that owns it.
	if c.renderClient != nil {
		c.renderClient.Release()
		c.renderClient = nil
	}
	if c.client != nil {
		c.client.Release()
		c.client = nil
	}
	if c.enumerator != nil {
		c.enumerator.Release()
		c.enumerator = nil
	}
}

func (c *wasapiContext) startOnCOMThread() (ferr error) {
	c.releaseOnCOMThread()
	defer func() {
		if ferr != nil {
			c.releaseOnCOMThread()
		}
	}()

	// Recreate the enumerator because it can become disconnected across
	// suspend/resume or an audio service restart.
	e, err := _CoCreateInstance(&uuidMMDeviceEnumerator, nil, uint32(_CLSCTX_ALL), &uuidIMMDeviceEnumerator)
	if err != nil {
		return err
	}
	c.enumerator = (*_IMMDeviceEnumerator)(e)

	device, err := c.enumerator.GetDefaultAudioEndpoint(eRender, eConsole)
	if err != nil {
		if errors.Is(err, _E_NOTFOUND) {
			return errDeviceNotFound
		}
		return err
	}
	defer device.Release()

	id, err := device.GetId()
	if err != nil {
		return err
	}
	c.currentDeviceID = id

	client, err := device.Activate(&uuidIAudioClient2, uint32(_CLSCTX_ALL), nil)
	if err != nil {
		return err
	}
	c.client = (*_IAudioClient2)(client)

	if err := c.client.SetClientProperties(&_AudioClientProperties{
		cbSize:     uint32(unsafe.Sizeof(_AudioClientProperties{})),
		bIsOffload: 0,                    // false
		eCategory:  _AudioCategory_Other, // In the example, AudioCategory_ForegroundOnlyMedia was used, but this value is deprecated.
	}); err != nil {
		return err
	}

	// Check the format is supported by WASAPI.
	// Stereo with 48000 [Hz] is likely supported, but mono and/or other sample rates are unlikely to be supported.
	// Fallback to WinMM in this case anyway.
	const bitsPerSample = 32
	nBlockAlign := c.channelCount * bitsPerSample / 8
	var channelMask uint32
	switch c.channelCount {
	case 1:
		channelMask = _SPEAKER_FRONT_CENTER
	case 2:
		channelMask = _SPEAKER_FRONT_LEFT | _SPEAKER_FRONT_RIGHT
	}
	f := &_WAVEFORMATEXTENSIBLE{
		wFormatTag:      _WAVE_FORMAT_EXTENSIBLE,
		nChannels:       uint16(c.channelCount),
		nSamplesPerSec:  uint32(c.sampleRate),
		nAvgBytesPerSec: uint32(c.sampleRate * nBlockAlign),
		nBlockAlign:     uint16(nBlockAlign),
		wBitsPerSample:  bitsPerSample,
		cbSize:          0x16,
		Samples:         bitsPerSample,
		dwChannelMask:   channelMask,
		SubFormat:       _KSDATAFORMAT_SUBTYPE_IEEE_FLOAT,
	}

	var bufferSizeIn100ns _REFERENCE_TIME
	if c.lowLatency {
		// In low-latency mode the device's own period drives the commits, so
		// size the buffer from it rather than from the fixed 50ms default.
		defaultPeriod, _, err := c.client.GetDevicePeriod()
		if err != nil {
			return err
		}
		period := time.Duration(defaultPeriod) * 100
		if period <= 0 {
			period = 10 * time.Millisecond
		}
		c.devicePeriod = period

		bufferDuration := 3 * period
		if bufferDuration < wasapiLowLatencyMinBuffer {
			bufferDuration = wasapiLowLatencyMinBuffer
		}
		if bufferDuration > wasapiLowLatencyMaxBuffer {
			bufferDuration = wasapiLowLatencyMaxBuffer
		}
		// A caller-supplied BufferSize still caps the buffer in low-latency
		// mode, so an explicit small BufferSize keeps honoring the request.
		if c.bufferSizeInBytes != 0 {
			bufferSizeInFrames := int64(c.bufferSizeInBytes) / int64(nBlockAlign)
			requested, ok := mathutil.MulDiv(bufferSizeInFrames, 1e7, int64(c.sampleRate))
			if !ok {
				return fmt.Errorf("oto: WASAPI buffer duration is unrepresentable")
			}
			if d := time.Duration(requested) * 100; d < bufferDuration {
				bufferDuration = d
			}
		}
		bufferSizeIn100ns = _REFERENCE_TIME(bufferDuration / 100)
	} else if c.bufferSizeInBytes != 0 {
		bufferSizeInFrames := int64(c.bufferSizeInBytes) / int64(nBlockAlign)
		duration, ok := mathutil.MulDiv(bufferSizeInFrames, 1e7, int64(c.sampleRate))
		if !ok {
			return fmt.Errorf("oto: WASAPI buffer duration is unrepresentable")
		}
		bufferSizeIn100ns = _REFERENCE_TIME(duration)
	} else {
		// The default buffer size can be too small and might cause glitch noises.
		// Specify 50[ms] as the buffer size.
		bufferSizeIn100ns = _REFERENCE_TIME(50 * time.Millisecond / 100)
	}

	// Even if the sample rate and/or the number of channels are not supported by the audio driver,
	// AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM should convert the sample rate automatically (#215).
	// AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY used together with
	// AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM tells WASAPI to use a high-quality
	// resampler. Without it, 44100 source played on a 48000 device sounds bad.
	if err := c.client.Initialize(_AUDCLNT_SHAREMODE_SHARED,
		_AUDCLNT_STREAMFLAGS_EVENTCALLBACK|_AUDCLNT_STREAMFLAGS_NOPERSIST|_AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM|_AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY,
		bufferSizeIn100ns, 0, f, nil); err != nil {
		return err
	}

	frames, err := c.client.GetBufferSize()
	if err != nil {
		return err
	}
	c.bufferFrames = frames

	if c.lowLatency {
		nominal, maxLatency := wasapiLowLatencyBudgets(c.devicePeriod)
		// The maximum budget cannot exceed the buffer the device granted.
		if maxFrames := time.Duration(frames) * time.Second / time.Duration(c.sampleRate); maxLatency > maxFrames {
			maxLatency = maxFrames
		}
		// A device restart re-enters here with a new device but the same
		// long-lived recorder, so the latency samples accumulate across
		// acquisitions; Adopt carries over the degradation counters.
		prev := c.controller
		c.controller = latencyctl.NewControllerWithRecorder(c.sampleRate, c.devicePeriod, int(frames), nominal, maxLatency, c.latencyRecorder)
		c.controller.Adopt(prev)
	}

	renderClient, err := c.client.GetService(&uuidIAudioRenderClient)
	if err != nil {
		return err
	}
	c.renderClient = (*_IAudioRenderClient)(renderClient)

	if err := c.client.SetEventHandle(c.sampleReadyEvent); err != nil {
		return err
	}

	// TODO: Should some errors be allowed? See WASAPIManager.cpp in the official example SimpleWASAPIPlaySound.

	if err := c.client.Start(); err != nil {
		return err
	}

	return nil
}

func (c *wasapiContext) loop() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// S_FALSE is returned when CoInitializeEx is nested. This is a successful case.
	if err := windows.CoInitializeEx(0, windows.COINIT_MULTITHREADED); err != nil && !errors.Is(err, syscall.Errno(windows.S_FALSE)) {
		return err
	}
	// CoUninitialize should be called even when CoInitializeEx returns S_FALSE.
	defer windows.CoUninitialize()

	if err := c.loopOnRenderThread(); err != nil {
		return err
	}

	return nil
}

func (c *wasapiContext) loopOnRenderThread() error {
	last := time.Now()
	for {
		c.suspendedCond.L.Lock()
		for c.suspended {
			c.suspendedCond.Wait()
		}
		c.suspendedCond.L.Unlock()

		// Keep checking suspension and device changes even if the endpoint stops
		// signaling its sample-ready event.
		evt, err := windows.WaitForSingleObject(c.sampleReadyEvent, 500)
		if err != nil {
			return err
		}
		if evt != windows.WAIT_OBJECT_0 && evt != uint32(windows.WAIT_TIMEOUT) {
			return fmt.Errorf("oto: WaitForSingleObject failed: returned value: %d", evt)
		}

		if evt == windows.WAIT_OBJECT_0 {
			// The timestamp is taken before the commit so the recorded commit
			// latency measures the scheduling and mixing delay itself.
			eventAt := time.Now()
			if c.lowLatency {
				if err := c.writeLowLatencyOnRenderThread(eventAt); err != nil {
					return err
				}
			} else {
				if err := c.writeOnRenderThread(eventAt); err != nil {
					return err
				}
			}
		}

		// Checking the current default audio device might be an expensive operation.
		// Check this repeatedly but with some time interval.
		if now := time.Now(); now.Sub(last) >= 500*time.Millisecond {
			switched, err := c.isDeviceSwitched()
			if err != nil {
				return err
			}
			if switched {
				return errDeviceSwitched
			}
			last = now
		}
	}
}

// writeOnRenderThread is the regular commit path: it fills every free frame of
// the device buffer. Its behavior is unchanged by the low-latency feature; the
// only addition is latency measurement, which is kept separate from the commit
// decision.
func (c *wasapiContext) writeOnRenderThread(eventAt time.Time) error {
	c.m.Lock()
	defer c.m.Unlock()

	paddingFrames, err := c.client.GetCurrentPadding()
	if err != nil {
		return err
	}

	// Defensive check: WASAPI should never report more padding than the buffer size,
	// but if it did, the uint32 subtraction below would wrap around to a huge frame
	// count and GetBuffer would fail, so skip the write instead.
	if paddingFrames >= c.bufferFrames {
		return nil
	}
	frames := c.bufferFrames - paddingFrames

	if err := c.commitFrames(frames); err != nil {
		return err
	}

	queued := paddingFrames + frames
	c.reportPlayed(paddingFrames)
	c.recorder.RecordCommit(time.Since(eventAt), c.framesToDuration(int64(queued)))
	return nil
}

// writeLowLatencyOnRenderThread is the event-driven low-latency path. The
// controller decides how many frames keep the queued audio within the latency
// budget; an event at which the budget is already full commits nothing, so the
// mux is not drained and the audible latency stays bounded.
func (c *wasapiContext) writeLowLatencyOnRenderThread(eventAt time.Time) error {
	c.m.Lock()
	defer c.m.Unlock()

	paddingFrames, err := c.client.GetCurrentPadding()
	if err != nil {
		return err
	}
	if paddingFrames >= c.bufferFrames {
		return nil
	}

	frames64 := c.controller.FramesToCommit(int64(paddingFrames))
	if frames64 <= 0 {
		// The water is still within the budget. Record the event anyway so the
		// commit-latency distribution reflects every device wakeup, and report
		// the current played position.
		c.reportPlayed(paddingFrames)
		c.controller.RecordCommit(time.Since(eventAt), int64(paddingFrames))
		return nil
	}
	// Defensive clamp: the controller already bounds this to the free space,
	// but never hand GetBuffer more than the device can accept regardless.
	if frames64 > int64(c.bufferFrames)-int64(paddingFrames) {
		frames64 = int64(c.bufferFrames) - int64(paddingFrames)
	}
	frames := uint32(frames64)

	if err := c.commitFrames(frames); err != nil {
		return err
	}

	queued := paddingFrames + frames
	c.reportPlayed(paddingFrames)
	c.controller.RecordCommit(time.Since(eventAt), int64(queued))
	return nil
}

// commitFrames mixes frames from the mux and releases them to the device. The
// caller holds c.m. A successful commit advances the written-frame counter and
// resets the consecutive OOM counter.
func (c *wasapiContext) commitFrames(frames uint32) error {
	// Get the destination buffer.
	dstBuf, err := c.renderClient.GetBuffer(frames)
	if err != nil {
		return err
	}

	// Calculate the buffer size.
	if buflen := int(frames) * c.channelCount; cap(c.buf) < buflen {
		c.buf = make([]float32, buflen)
	} else {
		c.buf = c.buf[:buflen]
	}

	// Read the buffer from the players.
	c.mux.ReadFloat32s(c.buf)

	// Copy the read buf to the destination buffer.
	copy(unsafe.Slice((*float32)(unsafe.Pointer(dstBuf)), len(c.buf)), c.buf)

	// Release the buffer.
	if err := c.renderClient.ReleaseBuffer(frames, 0); err != nil {
		return err
	}

	c.buf = c.buf[:0]
	// ReadFloat32s advanced the mux's global sample count by exactly the
	// committed number of samples (frames times channels).
	c.committedMixed = c.mux.Mixed()
	c.oomRetryCount = 0
	return nil
}

// reportPlayed tells the mux which sample of the mix is currently playing.
// paddingFrames is the amount still queued in the device, so the played sample
// is the last committed one minus that padding. The mux keeps counting while a
// device is restarted and drained, so no rebase is needed across restarts.
//
// The report is dropped while suspended, mirroring how the other drivers
// invalidate stale playback positions.
func (c *wasapiContext) reportPlayed(paddingFrames uint32) {
	if c.isSuspended() {
		c.mux.ClearPlayed()
		return
	}
	frame := c.committedMixed - int64(paddingFrames)*int64(c.channelCount)
	if frame < 0 {
		frame = 0
	}
	c.mux.SetPlayed(frame, time.Now())
}

// framesToDuration converts a device-frame count to its playback duration.
func (c *wasapiContext) framesToDuration(frames int64) time.Duration {
	if frames <= 0 {
		return 0
	}
	return time.Duration((frames*int64(time.Second) + int64(c.sampleRate)/2) / int64(c.sampleRate))
}

// latencyStats returns the measured latency for the active commit mode.
func (c *wasapiContext) latencyStats() LatencyStats {
	if c.lowLatency {
		c.m.Lock()
		controller := c.controller
		c.m.Unlock()
		if controller == nil {
			return LatencyStats{LowLatency: true}
		}
		return fromLatencyCtlStats(controller.Snapshot())
	}
	return fromLatencyCtlStats(c.recorder.Snapshot(false, 0, 0, 0, 0, 0, 0))
}

func fromLatencyCtlStats(s latencyctl.Stats) LatencyStats {
	return LatencyStats{
		LowLatency:        s.LowLatency,
		CommitLatency:     fromLatencyCtlDistribution(s.CommitLatency),
		OutputLatency:     fromLatencyCtlDistribution(s.OutputLatency),
		Underruns:         s.Underruns,
		Degradations:      s.Degradations,
		Recoveries:        s.Recoveries,
		TargetLatency:     s.TargetLatency,
		PeakTargetLatency: s.PeakTargetLatency,
		NominalLatency:    s.NominalLatency,
		MaxLatency:        s.MaxLatency,
	}
}

func fromLatencyCtlDistribution(d latencyctl.Distribution) LatencyDistribution {
	return LatencyDistribution{
		Count: d.Count,
		Min:   d.Min,
		Mean:  d.Mean,
		P95:   d.P95,
		Max:   d.Max,
	}
}

func (c *wasapiContext) Suspend() error {
	c.suspendedCond.L.Lock()
	c.suspended = true
	c.suspendedCond.L.Unlock()
	c.suspendedCond.Signal()

	// Nothing is playing while suspended, so a stale played position must not
	// survive until the next report.
	c.mux.ClearPlayed()
	return nil
}

func (c *wasapiContext) Resume() error {
	c.suspendedCond.L.Lock()
	c.suspended = false
	c.suspendedCond.L.Unlock()
	c.suspendedCond.Signal()

	return nil
}

func (c *wasapiContext) isSuspended() bool {
	c.suspendedCond.L.Lock()
	defer c.suspendedCond.L.Unlock()
	return c.suspended
}

func (c *wasapiContext) Err() error {
	return c.err.Load()
}

// isWASAPIDeviceTransientError reports whether err from (re)starting the client
// indicates a temporarily unusable device rather than a permanent failure.
func isWASAPIDeviceTransientError(err error) bool {
	return errors.Is(err, errDeviceNotFound) ||
		errors.Is(err, _AUDCLNT_E_DEVICE_INVALIDATED) ||
		errors.Is(err, _AUDCLNT_E_RESOURCES_INVALIDATED) ||
		errors.Is(err, _RPC_E_DISCONNECTED)
}

func (c *wasapiContext) restart() error {
	// The device is temporarily unusable, e.g. a headset was unplugged or the
	// machine is resuming from sleep. Recreate it, retrying transient failures a
	// bounded number of times.
	//
	// The mux is drained at ~real time so recovery resumes near the current
	// position instead of replaying a backlog, while the re-acquisition itself
	// backs off so a longer outage does not hammer the audio service.
	var buf [4096]float32
	framesPerRead := len(buf) / c.channelCount
	perRead := time.Duration(float64(time.Second) * float64(framesPerRead) / float64(c.sampleRate))

	reacquireInterval := wasapiReacquireMinInterval
	var nextReacquire time.Time // zero value: attempt immediately
	var retryCount int

	for {
		// Pause retrying entirely while suspended, and reset the retry count on
		// resume: a long suspend must not make the next resume give up
		// immediately.
		c.suspendedCond.L.Lock()
		var wasSuspended bool
		for c.suspended {
			wasSuspended = true
			c.suspendedCond.Wait()
		}
		c.suspendedCond.L.Unlock()
		if wasSuspended {
			reacquireInterval = wasapiReacquireMinInterval
			nextReacquire = time.Time{}
			retryCount = 0
		}

		if now := time.Now(); !now.Before(nextReacquire) {
			err := c.initialize()
			if err == nil {
				// The old device buffer was discarded and the mux kept being
				// drained during the outage, so any playback report measured
				// before the restart is stale. committedMixed is re-anchored on
				// the first commit of the new device.
				c.m.Lock()
				c.committedMixed = c.mux.Mixed()
				c.m.Unlock()
				c.mux.ClearPlayed()
				return nil
			}
			if !isWASAPIDeviceTransientError(err) {
				return err
			}
			retryCount++
			if retryCount >= wasapiRestartRetryLimit {
				return err
			}
			nextReacquire = time.Now().Add(reacquireInterval)
			reacquireInterval = min(reacquireInterval*2, wasapiReacquireMaxInterval)
		}

		c.mux.ReadFloat32s(buf[:framesPerRead*c.channelCount])
		time.Sleep(perRead)
	}
}
