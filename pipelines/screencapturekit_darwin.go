//go:build darwin

package pipelines

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	sckit "github.com/LocalKinAI/sckit-go"
	"github.com/ebitengine/purego"
)

// screencapturekit: macOS ScreenCaptureKit through sckit-go. Frames arrive as
// the system produces them.
//
// Display ids are indexes like on Linux, main display first, so display 0
// always works. Sizes are in pixels: ScreenCaptureKit reports points, which
// on a Retina screen would capture a downscaled frame, so the stream asks for
// the display mode's pixel size instead.

type screenCaptureKit struct{}

func init() { Register(screenCaptureKit{}, 10) }

func (screenCaptureKit) Name() string { return "screencapturekit" }

func (screenCaptureKit) Probe(ctx context.Context) error {
	_, err := sckit.ListDisplays(ctx)
	return err
}

type sckDisplay struct {
	Display
	sck sckit.Display
}

func sckDisplays(ctx context.Context) ([]sckDisplay, error) {
	ds, err := sckit.ListDisplays(ctx)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(ds, func(i, j int) bool {
		return ds[i].X == 0 && ds[i].Y == 0 && (ds[j].X != 0 || ds[j].Y != 0)
	})
	out := make([]sckDisplay, len(ds))
	for i, d := range ds {
		w, h, hz := cgMode(d.ID)
		if w == 0 || h == 0 {
			w, h = d.Width, d.Height
		}
		if hz <= 0 {
			hz = 60
		}
		out[i] = sckDisplay{
			Display: Display{ID: uint32(i), Name: fmt.Sprintf("cg%d", d.ID), Width: w, Height: h, X: d.X, Y: d.Y, RefreshRate: hz},
			sck:     d,
		}
	}
	return out, nil
}

func (screenCaptureKit) ListDisplays(ctx context.Context) ([]Display, error) {
	ds, err := sckDisplays(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Display, len(ds))
	for i, d := range ds {
		out[i] = d.Display
	}
	return out, nil
}

func (screenCaptureKit) Start(ctx context.Context, displayID uint32, fps int) (Stream, error) {
	fps = clampFPS(fps)
	ds, err := sckDisplays(ctx)
	if err != nil {
		return nil, err
	}
	all := make([]Display, len(ds))
	for i, d := range ds {
		all[i] = d.Display
	}
	i, err := pickDisplay(all, displayID)
	if err != nil {
		return nil, err
	}
	d := ds[i]
	return startStream(ctx, func() (grabber, error) {
		st, err := sckit.NewStream(ctx, d.sck, sckit.WithFrameRate(fps), sckit.WithResolution(d.Width, d.Height))
		if err != nil {
			return nil, err
		}
		return &sckGrabber{st: st, ctx: ctx, w: st.Width(), h: st.Height()}, nil
	}, fps, false)
}

// sckit.Stream isn't safe to Close while another thread is inside
// NextFrame: the handle is freed under it and the frame wait never wakes. So
// grab waits in short slices and checks a flag, and only close (on the stream
// thread, after grab has returned) tears the stream down.
type sckGrabber struct {
	st      *sckit.Stream
	ctx     context.Context
	w, h    int
	stopped atomic.Bool
}

const sckWaitSlice = 100 * time.Millisecond

var errSCKStopped = errors.New("screencapturekit: stream stopped")

func (g *sckGrabber) size() (int, int) { return g.w, g.h }

func (g *sckGrabber) grab(dst []byte) error {
	for {
		if g.stopped.Load() {
			return errSCKStopped
		}
		if err := g.ctx.Err(); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(g.ctx, sckWaitSlice)
		f, err := g.st.NextFrameBGRA(ctx)
		cancel()
		if errors.Is(err, sckit.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
			continue
		}
		if err != nil {
			return err
		}
		if f.Width != g.w || f.Height != g.h {
			return fmt.Errorf("frame is %dx%d, stream is %dx%d", f.Width, f.Height, g.w, g.h)
		}
		copy(dst, f.Pixels)
		return nil
	}
}

func (g *sckGrabber) interrupt() { g.stopped.Store(true) }

func (g *sckGrabber) close() error { return g.st.Close() }

// CoreGraphics, for the display mode's pixel size and refresh rate.
var cg struct {
	once        sync.Once
	ok          bool
	copyMode    func(display uint32) uintptr
	pixelWidth  func(mode uintptr) uintptr
	pixelHeight func(mode uintptr) uintptr
	refreshRate func(mode uintptr) float64
	release     func(mode uintptr)
}

func cgMode(display uint32) (w, h int, hz float64) {
	cg.once.Do(func() {
		lib, err := purego.Dlopen("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			return
		}
		purego.RegisterLibFunc(&cg.copyMode, lib, "CGDisplayCopyDisplayMode")
		purego.RegisterLibFunc(&cg.pixelWidth, lib, "CGDisplayModeGetPixelWidth")
		purego.RegisterLibFunc(&cg.pixelHeight, lib, "CGDisplayModeGetPixelHeight")
		purego.RegisterLibFunc(&cg.refreshRate, lib, "CGDisplayModeGetRefreshRate")
		purego.RegisterLibFunc(&cg.release, lib, "CGDisplayModeRelease")
		cg.ok = true
	})
	if !cg.ok {
		return 0, 0, 0
	}
	m := cg.copyMode(display)
	if m == 0 {
		return 0, 0, 0
	}
	defer cg.release(m)
	return int(cg.pixelWidth(m)), int(cg.pixelHeight(m)), cg.refreshRate(m)
}
