// Package track provides a beep.Streamer with real-time stream insertion.
package track

import (
	"errors"
	"sync"
	"sync/atomic"

	"github.com/faiface/beep"
)

// Track is a beep.StreamCloser which synchronously switches between a default
// streamer and a settable active streamer to provide a constantly playing
// stream.
//
// The zero value of Track defaults to beep.Silence as its silence streamer.
type Track struct {
	// flags holds flags regarding the status of the active streamer.
	flags int32
	// closed indicates that the track is closed. It must be written atomically
	// and can be read non-atomically iff cmu is held.
	closed int32
	// smu serializes calls to Set. Usually, it is locked by Set and left
	// locked until Stream reads all samples from the active streamer.
	smu sync.Mutex
	// cmu synchronizes Stream and Close.
	cmu sync.Mutex
	// active is the active streamer. Stream prefers streaming from active if
	// it is not nil.
	active beep.Streamer
	// silence is the silence streamer. Stream defaults to silence if there is
	// no active streamer.
	silence beep.Streamer
}

const (
	// flagInit indicates that an active streamer is available.
	flagInit int32 = 1 << iota
	// flagSet indicates that a call to Set is in progress.
	flagSet
	// flagErr indicates that the track entered an erroneous state because the
	// silence streamer failed to fill a required number of samples.
	flagErr
)

// New creates a track defaulting to silence and beginning with start as its
// active streamer. If silence is nil, a default silent stream is used. If
// start is nil, streaming begins with silence. silence must always fill any
// number of samples.
func New(silence, start beep.Streamer) *Track {
	if silence == nil {
		silence = beep.Silence(-1)
	}
	t := Track{
		silence: silence,
		active:  start,
	}
	if start != nil {
		t.flags = flagInit
		t.smu.Lock()
	}
	return &t
}

// Stream returns samples from the track's active streamer, or from its default
// silence track if there is no active streamer. If all samples are streamed
// from the active streamer and the active streamer is a beep.StreamCloser,
// this additionally closes it. Panics if the silence streamer fails to stream
// all required samples.
//
// It is not safe for multiple goroutines to call Stream concurrently, but any
// number of goroutines may call Set.
func (t *Track) Stream(samples [][2]float64) (int, bool) {
	f := atomic.LoadInt32(&t.flags)
	for f&flagSet != 0 {
		// Another goroutine is setting a streamer.
		f = atomic.LoadInt32(&t.flags)
	}
	if f&flagErr != 0 {
		return 0, false
	}
	t.cmu.Lock()
	// Can't defer cmu.Unlock because we might recurse through streamSilence.
	if t.closed != 0 {
		t.cmu.Unlock()
		// smu was unlocked by Close.
		return 0, false
	}
	// Reload flags in case Interrupt just finished.
	if atomic.LoadInt32(&t.flags)&flagInit == 0 {
		// No active streamer. smu was unlocked by a previous call to Stream.
		t.cmu.Unlock()
		return t.streamSilence(samples)
	}
	n, _ := t.active.Stream(samples)
	if n < len(samples) {
		// We've consumed the active streamer. Close it if needed and open up
		// to new setters.
		t.closeActive()
		// We're really unsetting flagInit, but if we're reaching this point,
		// then it's the only set flag.
		atomic.StoreInt32(&t.flags, 0)
		// smu was locked by Set.
		t.smu.Unlock()
		t.cmu.Unlock()
		k, ok := t.streamSilence(samples[n:])
		return n + k, ok
	}
	t.cmu.Unlock()
	return len(samples), true
}

// streamSilence streams samples from the track's silence streamer and panics
// if it fails to provide enough samples.
func (t *Track) streamSilence(samples [][2]float64) (n int, ok bool) {
	if t.silence == nil {
		t.silence = beep.Silence(-1)
	}
	for len(samples) > 0 {
		if f := atomic.LoadInt32(&t.flags); f&(flagInit|flagSet) != 0 {
			// Someone is setting or has set a new active streamer.
			k, _ := t.Stream(samples)
			return n + k, n+k > 0
		}
		if atomic.LoadInt32(&t.closed) != 0 {
			return n, n > 0
		}
		need := len(samples)
		if need > silenceMax {
			need = silenceMax
		}
		k, _ := t.silence.Stream(samples[:need])
		if k < need {
			// We may or may not be concurrent with a call to Set.
			f := atomic.SwapInt32(&t.flags, flagErr)
			for !atomic.CompareAndSwapInt32(&t.flags, flagErr, f|flagErr) {
				f = atomic.SwapInt32(&t.flags, flagErr)
			}
			return n + k, n+k > 0
		}
		n += k
		samples = samples[k:]
	}
	return n, true
}

// silenceMax is the maximum number of samples of silence to stream at once.
// Note that several package tests implicitly assume this value.
const silenceMax = 32

// Set sets the track's playing streamer. If the track is currently playing
// another streamer, this blocks until that streamer has finished. Panics if
// t is closed.
//
// If stream is nil, Set performs all its normal blocking, but does not
// actually set a streamer. This allows callers to join on the completion of
// the current active streamer.
//
// It is safe for any number of goroutines to call Set and for there to be at
// most one goroutine calling Stream concurrently.
func (t *Track) Set(stream beep.Streamer) {
	t.smu.Lock() // t.Stream unlocks t.smu!
	f := atomic.LoadInt32(&t.flags)
	// Wait for any call to Interrupt to complete.
	for !atomic.CompareAndSwapInt32(&t.flags, f&^flagSet, f|flagSet) {
		f = atomic.LoadInt32(&t.flags)
	}
	if f&flagErr != 0 {
		// The track is in an error state. Unlock and unset flagSet so any
		// waiting setters can proceed.
		atomic.AddInt32(&t.flags, -flagSet)
		t.smu.Unlock()
		return
	}
	if atomic.LoadInt32(&t.closed) != 0 {
		// The track is closed. We want to unlock the mutex before panicking so
		// that the program can continue if the panic is recovered.
		atomic.AddInt32(&t.flags, -flagSet)
		t.smu.Unlock()
		panic(errors.New("track: Set on closed track"))
	}
	if stream != nil {
		t.active = stream
		// flagInit is the least significant bit, and flagSet is the bit
		// following it. We know that flagSet is set and flagInit is unset. So,
		// subtracting 1 sets flagInit and unsets flagSet, without possibly
		// changing any other bits.
		atomic.AddInt32(&t.flags, -1)
	} else {
		atomic.AddInt32(&t.flags, -flagSet)
		t.smu.Unlock()
	}
}

// Err returns an error if the silence streamer failed to provide enough
// samples. The returned error is of type *SilenceError, and it wraps the
// silence streamer's error, if any. It does not propagate errors from any
// active streamers.
func (t *Track) Err() error {
	if atomic.LoadInt32(&t.flags)&flagErr == 0 {
		return nil
	}
	return &SilenceError{err: t.silence.Err(), Silence: t.silence}
}

// Close stops streamer playback. If there is an active streamer and it
// implements beep.StreamCloser, this additionally closes it. It is safe to
// call this concurrently with Stream, but the caller must ensure that there is
// no concurrent call to Set.
func (t *Track) Close() error {
	atomic.StoreInt32(&t.closed, 1)
	// Locking ensures that we wait for Stream to finish using any active
	// streamer. Since we already marked the track closed, if there is a
	// concurrent call to Stream, it will see that flag and won't use the
	// active streamer at all. So, we don't actually need to do anything in the
	// critical section; we can unlock immediately, rather than after closing
	// the active streamer.
	t.cmu.Lock()
	t.cmu.Unlock()
	// If there is an active streamer, a concurrent call to Set might be
	// waiting to acquire t.smu. While we don't really allow concurrent calls
	// to Set and Close, we still want to allow Set to progress so that it
	// panics instead of silently deadlocking – especially for testing.
	if atomic.LoadInt32(&t.flags)&flagInit != 0 {
		t.smu.Unlock()
	}
	return t.closeActive()
}

// Interrupt stops the currently playing active streamer, if any. If the active
// streamer implements beep.StreamCloser, this additionally closes it. It is
// safe to call this concurrently with Stream and Set. The returned error is
// the error from closing the active streamer, if any.
func (t *Track) Interrupt() error {
	f := atomic.LoadInt32(&t.flags)
	// Wait for any concurrent call to Set (or Interrupt) to complete.
	for !atomic.CompareAndSwapInt32(&t.flags, f&^flagSet, f|flagSet) {
		f = atomic.LoadInt32(&t.flags)
	}
	t.cmu.Lock()
	err := t.closeActive()
	if f := atomic.LoadInt32(&t.flags); f&flagInit != 0 {
		atomic.AddInt32(&t.flags, -flagInit)
		t.smu.Unlock()
	}
	atomic.AddInt32(&t.flags, -flagSet)
	t.cmu.Unlock()
	return err
}

// closeActive closes the active streamer if it can be closed.
func (t *Track) closeActive() (err error) {
	if c, _ := t.active.(beep.StreamCloser); c != nil {
		err = c.Close()
	}
	t.active = nil
	return err
}

var _ beep.StreamCloser = (*Track)(nil)

// SilenceError is the error returned by Track.Err if the track's silence
// streamer fails to provide enough samples. It wraps the silence streamer's
// error if there is one.
type SilenceError struct {
	// err is the silence streamer's error.
	err error
	// Silence is the track's silence streamer.
	Silence beep.Streamer
}

func (err *SilenceError) Error() string {
	msg := "with no error"
	if err.err != nil {
		msg = "with error " + err.err.Error()
	}
	return "silence streamer provided insufficient samples " + msg
}

// Unwrap returns the wrapped error which was returned by the track's silence
// streamer after it closed.
func (err *SilenceError) Unwrap() error {
	return err.err
}
