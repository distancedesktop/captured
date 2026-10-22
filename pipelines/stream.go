package pipelines

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

// grabber is what a pipeline implements; stream does the rest.
//
// open/grab/close all run on one locked OS thread, so grabbers that hold
// thread-bound state (EGL/GL contexts) need no extra plumbing.
type grabber interface {
	size() (w, h int)
	// grab fills dst (w*h*4 bytes). Paced grabbers return immediately with the
	// current image; pulled grabbers block until the source has a new one.
	grab(dst []byte) error
	close() error
}

// interrupter is implemented by pulled grabbers whose grab can block forever
// (an idle compositor sends nothing); Close calls it to unblock grab.
type interrupter interface {
	interrupt()
}

type Stats struct {
	Frames    uint64  `json:"frames"`
	Dropped   uint64  `json:"dropped"`
	GrabAvgMs float64 `json:"grab_avg_ms"`
	GrabMaxMs float64 `json:"grab_max_ms"`
	Err       string  `json:"error,omitempty"`
}

type bufPool struct {
	size int
	free chan []byte
}

func newBufPool(size, n int) *bufPool {
	return &bufPool{size: size, free: make(chan []byte, n)}
}

func (p *bufPool) get() []byte {
	select {
	case b := <-p.free:
		return b
	default:
		return make([]byte, p.size)
	}
}

func (p *bufPool) put(b []byte) {
	if len(b) != p.size {
		return
	}
	select {
	case p.free <- b:
	default:
	}
}

type stream struct {
	out    chan *Frame
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	mu      sync.Mutex
	g       grabber
	frames  atomic.Uint64
	dropped atomic.Uint64
	grabSum atomic.Int64
	grabMax atomic.Int64
	err     atomic.Value
}

// startStream opens the grabber on the stream's own locked thread and starts
// producing frames. paced: grab on a ticker at fps. Otherwise grab back to
// back and let the source set the rate.
//
// The output channel holds one frame. When the consumer falls behind, the
// stale frame is dropped instead of queueing, so latency can't build up here.
func startStream(ctx context.Context, open func() (grabber, error), fps int, paced bool) (Stream, error) {
	if fps <= 0 {
		fps = 60
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &stream{out: make(chan *Frame, 1), cancel: cancel, done: make(chan struct{})}
	opened := make(chan error, 1)

	go func() {
		runtime.LockOSThread()
		defer close(s.done)
		defer close(s.out)
		g, err := open()
		if err != nil {
			opened <- err
			return
		}
		s.mu.Lock()
		s.g = g
		s.mu.Unlock()
		defer g.close()
		opened <- nil
		s.run(ctx, g, fps, paced)
	}()

	if err := <-opened; err != nil {
		cancel()
		return nil, err
	}
	return s, nil
}

func (s *stream) run(ctx context.Context, g grabber, fps int, paced bool) {
	w, h := g.size()
	pool := newBufPool(w*h*4, 3)
	var tick <-chan time.Time
	if paced {
		t := time.NewTicker(time.Second / time.Duration(fps))
		defer t.Stop()
		tick = t.C
	}
	for seq := uint64(0); ; seq++ {
		if paced {
			select {
			case <-ctx.Done():
				return
			case <-tick:
			}
		} else if ctx.Err() != nil {
			return
		}
		buf := pool.get()
		t0 := time.Now()
		if err := g.grab(buf); err != nil {
			if ctx.Err() == nil {
				s.err.Store(err.Error())
			}
			return
		}
		d := time.Since(t0)
		s.grabSum.Add(int64(d))
		if int64(d) > s.grabMax.Load() {
			s.grabMax.Store(int64(d))
		}
		s.frames.Add(1)
		s.push(&Frame{Data: buf, Width: w, Height: h, Seq: seq, Captured: t0, pool: pool})
	}
}

func (s *stream) push(f *Frame) {
	select {
	case s.out <- f:
		return
	default:
	}
	select {
	case old := <-s.out:
		old.Release()
		s.dropped.Add(1)
	default:
	}
	select {
	case s.out <- f:
	default:
		f.Release()
		s.dropped.Add(1)
	}
}

func (s *stream) Frames() <-chan *Frame { return s.out }

func (s *stream) Stats() Stats {
	st := Stats{Frames: s.frames.Load(), Dropped: s.dropped.Load()}
	if st.Frames > 0 {
		st.GrabAvgMs = float64(s.grabSum.Load()) / float64(st.Frames) / 1e6
	}
	st.GrabMaxMs = float64(s.grabMax.Load()) / 1e6
	if e, ok := s.err.Load().(string); ok {
		st.Err = e
	}
	return st
}

func (s *stream) Close() error {
	s.once.Do(func() {
		s.cancel()
		s.mu.Lock()
		if i, ok := s.g.(interrupter); ok {
			i.interrupt()
		}
		s.mu.Unlock()
		for f := range s.out {
			f.Release()
		}
		<-s.done
	})
	return nil
}
