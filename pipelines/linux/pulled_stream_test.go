//go:build linux

package linux

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"distancedesktop/captured/pipelines"
)

// blockingGrabber blocks in grab() until interrupt() is called, modelling a
// PipeWire source on an idle compositor that is producing no frames.
type blockingGrabber struct {
	release   chan struct{}
	closed    chan struct{}
	closeOnce sync.Once
	intOnce   sync.Once
}

func newBlockingGrabber() *blockingGrabber {
	return &blockingGrabber{
		release: make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

func (g *blockingGrabber) grab() ([]byte, int, int, error) {
	<-g.release
	return nil, 0, 0, errors.New("interrupted")
}

func (g *blockingGrabber) interrupt() {
	g.intOnce.Do(func() { close(g.release) })
}

func (g *blockingGrabber) close() error {
	g.closeOnce.Do(func() { close(g.closed) })
	return nil
}

// A pulled stream must be closable while its grabber is blocked waiting for a
// frame. Without the interrupt watchdog, Close blocks on the producer goroutine
// which is itself parked in grab(), and the grabber is never closed.
func TestPulledFrameStreamCloseInterruptsBlockedGrab(t *testing.T) {
	g := newBlockingGrabber()
	fs := newPulledFrameStream(context.Background(), g)

	done := make(chan struct{})
	go func() {
		_ = fs.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked while grab() was waiting for a frame")
	}

	select {
	case <-g.closed:
	case <-time.After(time.Second):
		t.Fatal("grabber was not closed")
	}

	if _, ok := <-fs.Frames(); ok {
		t.Fatal("frame channel should be closed and empty")
	}
}

// Cancelling the parent context must tear the stream down for the same reason.
func TestPulledFrameStreamParentCancelInterruptsBlockedGrab(t *testing.T) {
	g := newBlockingGrabber()
	ctx, cancel := context.WithCancel(context.Background())
	fs := newPulledFrameStream(ctx, g)
	cancel()

	select {
	case <-g.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("parent cancellation did not interrupt the blocked grab")
	}
	_ = fs.Close()
}

// countingGrabber yields a fixed number of frames, then reports EOF.
type countingGrabber struct {
	remaining int
	w, h      int
	closed    bool
}

func (g *countingGrabber) grab() ([]byte, int, int, error) {
	if g.remaining == 0 {
		return nil, 0, 0, errors.New("eof")
	}
	g.remaining--
	return make([]byte, g.w*g.h*4), g.w, g.h, nil
}

func (g *countingGrabber) close() error {
	g.closed = true
	return nil
}

// A grabber with no interrupt() must still work: the watchdog is optional.
func TestPulledFrameStreamPlainGrabber(t *testing.T) {
	g := &countingGrabber{remaining: 3, w: 4, h: 2}
	fs := newPulledFrameStream(context.Background(), g)

	var got int
	for f := range fs.Frames() {
		if f.Format != pipelines.FormatBGRA {
			t.Fatalf("format = %q, want %q", f.Format, pipelines.FormatBGRA)
		}
		if f.Width != 4 || f.Height != 2 {
			t.Fatalf("frame = %dx%d, want 4x2", f.Width, f.Height)
		}
		if len(f.Data) != 4*2*4 {
			t.Fatalf("len(Data) = %d, want %d", len(f.Data), 4*2*4)
		}
		got++
	}
	if got != 3 {
		t.Fatalf("received %d frames, want 3", got)
	}
	if !g.closed {
		t.Fatal("grabber was not closed after the producer exited")
	}
	_ = fs.Close()
}

// Close must be idempotent; FrameStream.Close is reachable from both the owner
// and the teardown path in the agent.
func TestPulledFrameStreamCloseTwice(t *testing.T) {
	g := newBlockingGrabber()
	fs := newPulledFrameStream(context.Background(), g)
	if err := fs.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := fs.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
