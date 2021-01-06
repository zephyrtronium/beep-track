package track_test

import (
	"testing"

	"github.com/faiface/beep"
	"github.com/zephyrtronium/beep-track"
)

type someandstop struct {
	l, r   float64
	n      int
	closed bool
}

func (s *someandstop) Stream(samples [][2]float64) (int, bool) {
	if s.n <= 0 || s.closed {
		return 0, false
	}
	if s.n > len(samples) {
		for i := range samples {
			samples[i] = [2]float64{s.l, s.r}
		}
		s.n -= len(samples)
		return len(samples), true
	}
	n := s.n
	for i := 0; i < n; i++ {
		samples[i] = [2]float64{s.l, s.r}
	}
	s.n = 0
	return n, true
}

func (s *someandstop) Err() error {
	return nil
}

func (s *someandstop) Close() error {
	s.closed = true
	return nil
}

var _ beep.StreamCloser = (*someandstop)(nil)

func TestStreamDefaults(t *testing.T) {
	s := track.New(nil, nil)
	r := make([][2]float64, 64)
	n, ok := s.Stream(r)
	if n != len(r) {
		t.Errorf("wrong number of samples: want %d, got %d", len(r), n)
	}
	if !ok {
		t.Errorf("stream read not ok")
	}
	for i, v := range r {
		if v != [2]float64{} {
			t.Errorf("wrong sample at %d: want [0 0], got %f", i, v)
		}
	}
}

func TestStreamStart(t *testing.T) {
	a := &someandstop{l: 1, r: 1, n: 1}
	s := track.New(&someandstop{l: -1, r: -1, n: 1 << 30}, a)
	r := make([][2]float64, 64)
	n, ok := s.Stream(r)
	if n != len(r) {
		t.Errorf("wrong number of samples: want %d, got %d", len(r), n)
	}
	if !ok {
		t.Errorf("stream read not ok")
	}
	if r[0] != [2]float64{1, 1} {
		t.Errorf("wrong first sample: want [1 1], got %f", r[0])
	}
	for i := 1; i < len(r); i++ {
		if r[i] != [2]float64{-1, -1} {
			t.Errorf("wrong non-first sample at %d: want [-1 -1], got %f", i, r[i])
		}
	}
	if !a.closed {
		t.Errorf("active streamer not closed")
	}
}

func TestStreamSetStart(t *testing.T) {
	a := &someandstop{l: 1, r: 1, n: 1}
	s := track.New(&someandstop{l: -1, r: -1, n: 1 << 30}, nil)
	s.Set(a)
	r := make([][2]float64, 64)
	n, ok := s.Stream(r)
	if n != len(r) {
		t.Errorf("wrong number of samples: want %d, got %d", len(r), n)
	}
	if !ok {
		t.Errorf("stream read not ok")
	}
	if r[0] != [2]float64{1, 1} {
		t.Errorf("wrong first sample: want [1 1], got %f", r[0])
	}
	for i := 1; i < len(r); i++ {
		if r[i] != [2]float64{-1, -1} {
			t.Errorf("wrong non-first sample at %d: want [-1 -1], got %f", i, r[i])
		}
	}
	if !a.closed {
		t.Errorf("active streamer not closed")
	}
}

func TestStreamSet(t *testing.T) {
	a := &someandstop{l: 1, r: 1, n: 1}
	s := track.New(&someandstop{l: -1, r: -1, n: 1 << 30}, nil)
	r := make([][2]float64, 64)
	n, ok := s.Stream(r)
	if n != len(r) {
		t.Errorf("wrong number of samples before set: want %d, got %d", len(r), n)
	}
	if !ok {
		t.Errorf("stream read before set not ok")
	}
	for i := 0; i < len(r); i++ {
		if r[i] != [2]float64{-1, -1} {
			t.Errorf("wrong sample before set at %d: want [-1 -1], got %f", i, r[i])
		}
	}
	s.Set(a)
	n, ok = s.Stream(r)
	if n != len(r) {
		t.Errorf("wrong number of samples after set: want %d, got %d", len(r), n)
	}
	if !ok {
		t.Errorf("stream read after set not ok")
	}
	if r[0] != [2]float64{1, 1} {
		t.Errorf("wrong first sample after set: want [1 1], got %f", r[0])
	}
	for i := 1; i < len(r); i++ {
		if r[i] != [2]float64{-1, -1} {
			t.Errorf("wrong non-first sample after set at %d: want [-1 -1], got %f", i, r[i])
		}
	}
	if !a.closed {
		t.Errorf("active streamer not closed")
	}
}

func TestStreamSetConcurrent(t *testing.T) {
	a := &someandstop{l: 1, r: 0, n: 4095}
	b := &someandstop{l: 0, r: 1, n: 4095}
	ch := make(chan string)
	s := track.New(nil, nil)
	go func() {
		r := make([][2]float64, 512)
		var havea, donea, haveb, doneb bool
		// signal to start setting
		ch <- ""
		for {
			s.Stream(r)
			for _, v := range r {
				switch v {
				case [2]float64{}:
					if havea {
						donea = true
					}
					if haveb {
						doneb = true
					}
				case [2]float64{1, 0}:
					if donea {
						ch <- "got samples from a twice"
						close(ch)
						return
					}
					if haveb && !doneb {
						doneb = true
					}
					havea = true
				case [2]float64{0, 1}:
					if doneb {
						ch <- "got samples from b twice"
						close(ch)
						return
					}
					if havea && !donea {
						donea = true
					}
					haveb = true
				}
			}
			if donea && doneb {
				close(ch)
				return
			}
		}
	}()
	<-ch
	// TODO: these don't quit if the test fails
	go s.Set(a)
	go s.Set(b)
	if m, ok := <-ch; ok {
		t.Error(m)
	}
}

func TestCloseSilence(t *testing.T) {
	s := track.New(nil, nil)
	r := make([][2]float64, 64)
	if err := s.Close(); err != nil {
		t.Error("error from close:", err)
	}
	n, ok := s.Stream(r)
	if n > 0 {
		t.Errorf("expected 0, got %d samples after closing: %v", n, r[:n])
	}
	if ok {
		t.Error("unexpected successful stream")
	}
}

func TestCloseActive(t *testing.T) {
	a := &someandstop{l: 1, r: 1, n: 1 << 30}
	s := track.New(nil, a)
	r := make([][2]float64, 64)
	if err := s.Close(); err != nil {
		t.Error("error from close:", err)
	}
	n, ok := s.Stream(r)
	if n > 0 {
		t.Errorf("expected 0, got %d samples after closing: %v", n, r[:n])
	}
	if ok {
		t.Error("unexpected successful stream")
	}
}

func TestSetClose(t *testing.T) {
	defer func() {
		if err := recover(); err == nil {
			t.Error("missing panic setting active streamer on closed track")
		}
	}()
	s := track.New(nil, beep.Silence(1))
	if err := s.Close(); err != nil {
		t.Error("error from close:", err)
	}
	s.Set(beep.Silence(1))
}
