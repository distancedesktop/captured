//go:build darwin

package pipelines

/*
#cgo darwin LDFLAGS: -framework ScreenCaptureKit -framework CoreMedia -framework CoreVideo -framework Foundation
#include "capsck_darwin.h"
#include <stdlib.h>
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"distancedesktop/captured/av"

	sckit "github.com/LocalKinAI/sckit-go"
	"github.com/ebitengine/purego"
)

// screencapturekit: macOS ScreenCaptureKit. Displays come from sckit-go;
// the stream itself is capsck_darwin.m and returns CVPixelBuffers, which go
// to VideoToolbox encoders without leaving the GPU (blit into the pooled
// surface when the output size differs). Frames arrive as the system
// produces them.
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

func (screenCaptureKit) Start(ctx context.Context, displayID uint32, cfg Config) (Session, error) {
	fps := clampFPS(cfg.FPS)
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
	return startSession(ctx, func() (grabber, error) {
		errbuf := make([]byte, 256)
		h := C.cap_sck_start(C.uint32_t(d.sck.ID), C.int(d.Width), C.int(d.Height), C.int(fps),
			(*C.char)(unsafe.Pointer(&errbuf[0])), C.int(len(errbuf)))
		if h == nil {
			return nil, errors.New(C.GoString((*C.char)(unsafe.Pointer(&errbuf[0]))))
		}
		return &sckGrabber{st: h, ctx: ctx, w: d.Width, h: d.Height}, nil
	}, cfg, false)
}

// The stream hands out retained CVPixelBufferRefs. Frames stay on the GPU
// when the encoder is VideoToolbox; the CPU path locks and copies the same
// buffer.
type sckGrabber struct {
	st      unsafe.Pointer
	ctx     context.Context
	w, h    int
	stopped atomic.Bool
}

const sckWaitSlice = 100 * time.Millisecond

var errSCKStopped = errors.New("screencapturekit: stream stopped")

func (g *sckGrabber) size() (int, int) { return g.w, g.h }

// next returns the newest frame's pixel buffer, owned by the caller.
func (g *sckGrabber) next() (unsafe.Pointer, error) {
	errbuf := make([]byte, 256)
	for {
		if g.stopped.Load() {
			return nil, errSCKStopped
		}
		if err := g.ctx.Err(); err != nil {
			return nil, err
		}
		var pb unsafe.Pointer
		ret := C.cap_sck_next(g.st, C.int(sckWaitSlice.Milliseconds()), &pb,
			(*C.char)(unsafe.Pointer(&errbuf[0])), C.int(len(errbuf)))
		if ret == 1 {
			continue
		}
		if ret != 0 {
			return nil, errors.New(C.GoString((*C.char)(unsafe.Pointer(&errbuf[0]))))
		}
		var w, h C.int
		C.cap_sck_pb_size(pb, &w, &h)
		if int(w) != g.w || int(h) != g.h {
			C.cap_sck_pb_release(pb)
			return nil, fmt.Errorf("frame is %dx%d, stream is %dx%d", int(w), int(h), g.w, g.h)
		}
		return pb, nil
	}
}

func (g *sckGrabber) grab(dst []byte) error {
	pb, err := g.next()
	if err != nil {
		return err
	}
	defer C.cap_sck_pb_release(pb)
	if C.cap_sck_pb_copy(pb, unsafe.Pointer(&dst[0]), C.int(g.w), C.int(g.h)) != 0 {
		return errors.New("unexpected pixel buffer layout")
	}
	return nil
}

func (g *sckGrabber) hw() (av.HWType, string, av.PixFmt) { return av.VideoToolbox, "", av.BGRA }

func (g *sckGrabber) bindHW(dev *av.HWDevice, w, h int) error { return nil }

func (g *sckGrabber) unbindHW() {}

func (g *sckGrabber) grabHW(f av.Frame) error {
	pb, err := g.next()
	if err != nil {
		return err
	}
	var w, h C.int
	C.cap_sck_pb_size(pb, &w, &h)
	if err := f.FillVideoToolbox(pb, int(w), int(h)); err != nil {
		return err
	}
	return nil
}

func (g *sckGrabber) interrupt() { g.stopped.Store(true) }

func (g *sckGrabber) close() error {
	C.cap_sck_stop(g.st)
	return nil
}

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
