package pipelines

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeGrabber struct {
	n      int
	closed bool
}

func (g *fakeGrabber) size() (int, int) { return 2, 2 }

func (g *fakeGrabber) grab(dst []byte) error {
	g.n++
	dst[0] = byte(g.n)
	return nil
}

func (g *fakeGrabber) close() error { g.closed = true; return nil }

func TestSlowConsumerGetsLatestFrame(t *testing.T) {
	g := &fakeGrabber{}
	st, err := startStream(context.Background(), func() (grabber, error) { return g, nil }, 1000, true)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	f := <-st.Frames()
	if f.Seq < 5 {
		t.Fatalf("got frame %d, expected a recent one", f.Seq)
	}
	if len(f.Data) != 16 {
		t.Fatalf("frame is %d bytes", len(f.Data))
	}
	f.Release()
	if st.Stats().Dropped == 0 {
		t.Fatal("expected stale frames to be dropped")
	}
	st.Close()
	if !g.closed {
		t.Fatal("grabber not closed")
	}
}

func TestOpenErrorIsReturned(t *testing.T) {
	want := errors.New("nope")
	_, err := startStream(context.Background(), func() (grabber, error) { return nil, want }, 60, true)
	if !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
}

func TestClampFPS(t *testing.T) {
	for in, want := range map[int]int{0: 60, -5: 60, 30: 30, 2_000_000_000: maxFPS} {
		if got := clampFPS(in); got != want {
			t.Errorf("clampFPS(%d) = %d, want %d", in, got, want)
		}
	}
}
