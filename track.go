// Package track provides a beep.Streamer with real-time stream insertion.
package track

import (
	"errors"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/faiface/beep"
)

// Track is a beep.StreamCloser which synchronously switches between a default
// streamer and a settable active streamer to provide a constantly playing
// stream.
//
// The zero value of Track is an invalid state.
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
	// flagErr indicates that the track entered an erroneous state, either from
	// calling Set concurrently with or after Close or from the silence
	// streamer failing to fill all required samples.
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
	if f&flagInit == 0 {
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
		// then it's the only set flag. Also, there are no concurrent writers
		// to t.flags because we still hold the mutex, so we don't need to
		// store atomically.
		t.flags = 0
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
	for len(samples) > 0 {
		if f := atomic.LoadInt32(&t.flags); f&(flagInit|flagSet) != 0 {
			// Someone is setting or has set a new active streamer.
			k, _ := t.Stream(samples)
			return n + k, n+k > 0
		}
		if atomic.LoadInt32(&t.closed) != 0 {
			return n, true
		}
		need := len(samples)
		if need > silenceMax {
			need = silenceMax
		}
		k, _ := t.silence.Stream(samples[:need])
		if k < need {
			// We may or may not be concurrent with a call to Set.
			f := atomic.SwapInt32(&t.flags, flagErr|flagSet)
			for !atomic.CompareAndSwapInt32(&t.flags, f, f&^flagSet|flagErr) {
				f = atomic.SwapInt32(&t.flags, flagErr|flagSet)
			}
			panic(errors.New("track: not enough samples: need " + strconv.Itoa(need) + ", got " + strconv.Itoa(k)))
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
// It is safe for any number of goroutines to call Set and for there to be at
// most one goroutine calling Stream concurrently.
func (t *Track) Set(stream beep.Streamer) {
	t.smu.Lock()
	// t.Stream unlocks t.mu!
	// The only flag that may be set is flagErr, so we can just add flagSet.
	atomic.AddInt32(&t.flags, flagSet)
	if atomic.LoadInt32(&t.closed) != 0 {
		// Unless this happens. We want to unlock the mutex before panicking so
		// that the program can continue if the panic is recovered.
		atomic.StoreInt32(&t.flags, flagErr)
		t.smu.Unlock()
		panic(errors.New("track: Set on closed track"))
	}
	t.active = stream
	// Reload flags in case streamSilence set flagErr.
	atomic.StoreInt32(&t.flags, atomic.LoadInt32(&t.flags)&^flagSet|flagInit)
}

// Err returns nil. It does not propagate errors from any active streamers.
func (t *Track) Err() error {
	return nil
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

// closeActive closes the active streamer if it can be closed.
func (t *Track) closeActive() (err error) {
	if c, _ := t.active.(beep.StreamCloser); c != nil {
		err = c.Close()
	}
	t.active = nil
	return err
}

var _ beep.StreamCloser = (*Track)(nil)
