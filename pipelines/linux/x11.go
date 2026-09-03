//go:build linux

package linux

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/ebitengine/purego"

	"distancedesktop/captured/pipelines"
)

// ---------------------------------------------------------------------------
// libX11 bindings (used by the x11 source selector).
// ---------------------------------------------------------------------------

const x11LibName = "libX11.so.6"

var (
	x11Once sync.Once
	x11Lib  uintptr

	xOpenDisplay   func(name string) uintptr
	xCloseDisplay  func(dpy uintptr) int
	xDefaultScreen func(dpy uintptr) int
	xDisplayWidth  func(dpy uintptr, screen int) int
	xDisplayHeight func(dpy uintptr, screen int) int
)

func loadX11() {
	x11Once.Do(func() {
		h, err := purego.Dlopen(x11LibName, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			x11Lib = 0
			return
		}
		x11Lib = h
		purego.RegisterLibFunc(&xOpenDisplay, x11Lib, "XOpenDisplay")
		purego.RegisterLibFunc(&xCloseDisplay, x11Lib, "XCloseDisplay")
		purego.RegisterLibFunc(&xDefaultScreen, x11Lib, "XDefaultScreen")
		purego.RegisterLibFunc(&xDisplayWidth, x11Lib, "XDisplayWidth")
		purego.RegisterLibFunc(&xDisplayHeight, x11Lib, "XDisplayHeight")
	})
}

type x11Display struct {
	ID      int
	Width   int
	Height  int
	X       int
	Y       int
	Refresh float64
}

// scanX11Displays reports the default X screen size. Full XRandr multi-output
// enumeration and XShm capture are follow-ups; this is enough for the source
// selector to list the primary screen.
func scanX11Displays() ([]x11Display, error) {
	loadX11()
	if x11Lib == 0 {
		return nil, fmt.Errorf("linux/x11: libX11 (%s) not available", x11LibName)
	}
	disp := os.Getenv("DISPLAY")
	if disp == "" {
		return nil, fmt.Errorf("linux/x11: $DISPLAY is not set")
	}
	dpy := xOpenDisplay(disp)
	if dpy == 0 {
		return nil, fmt.Errorf("linux/x11: cannot open X display %q", disp)
	}
	defer xCloseDisplay(dpy)
	screen := xDefaultScreen(dpy)
	w := xDisplayWidth(dpy, screen)
	h := xDisplayHeight(dpy, screen)
	if w == 0 || h == 0 {
		return nil, fmt.Errorf("linux/x11: invalid screen size from X")
	}
	return []x11Display{{ID: 0, Width: w, Height: h, X: 0, Y: 0, Refresh: 60}}, nil
}

type x11Pipeline struct{}

func (p *x11Pipeline) ListDisplays(ctx context.Context) ([]pipelines.DisplayMeta, error) {
	disps, err := scanX11Displays()
	if err != nil {
		return nil, err
	}
	out := make([]pipelines.DisplayMeta, len(disps))
	for i, d := range disps {
		out[i] = pipelines.DisplayMeta{
			ID:          uint32(d.ID),
			Width:       d.Width,
			Height:      d.Height,
			X:           d.X,
			Y:           d.Y,
			RefreshRate: d.Refresh,
		}
	}
	return out, nil
}

func (p *x11Pipeline) SupportedFormats() []pipelines.FrameFormat {
	return []pipelines.FrameFormat{pipelines.FormatBGRA}
}

func (p *x11Pipeline) StartStream(ctx context.Context, displayID uint32, fps int) (pipelines.FrameStream, error) {
	if fps <= 0 {
		fps = 60
	}
	disps, err := scanX11Displays()
	if err != nil {
		return nil, err
	}
	var target *x11Display
	for i := range disps {
		if uint32(disps[i].ID) == displayID {
			target = &disps[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("linux/x11: display %d not found", displayID)
	}
	// X11 pixel capture (XShmGetImage) is not yet implemented.
	return nil, fmt.Errorf("linux/x11: X11 pixel readback not yet implemented")
}
