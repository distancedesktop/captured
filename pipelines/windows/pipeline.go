//go:build windows

// Package windows implements the captured pipeline for Windows using plain
// GDI (user32/gdi32), loaded via purego so the build needs no C headers or
// cgo. BitBlt readback is the spike path; DXGI Desktop Duplication is the
// follow-up for GPU-side capture.
package windows

import (
	"context"
	"fmt"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"

	"distancedesktop/captured/pipelines"
)

var _ pipelines.Pipeline = (*windowsPipeline)(nil)

// ---------------------------------------------------------------------------
// user32/gdi32 bindings
// ---------------------------------------------------------------------------

var (
	user32Once sync.Once
	user32Lib  uintptr
	gdi32Lib   uintptr

	enumDisplayMonitors func(hdc uintptr, clip unsafe.Pointer, proc uintptr, data uintptr) uintptr
	getMonitorInfoW     func(hmon uintptr, mi unsafe.Pointer) uintptr
	setProcessDPIAware  func() uintptr

	getDC              func(hwnd uintptr) uintptr
	releaseDC          func(hwnd, hdc uintptr) uintptr
	createCompatibleDC func(hdc uintptr) uintptr
	createDIBSection   func(hdc uintptr, bmi unsafe.Pointer, usage uint32, bits *unsafe.Pointer, section uintptr, offset uint32) uintptr
	selectObject       func(hdc, hobj uintptr) uintptr
	bitBlt             func(dst uintptr, x, y, w, h int, src uintptr, xSrc, ySrc int, rop uint32) uintptr
	deleteDC           func(hdc uintptr) uintptr
	deleteObject       func(hobj uintptr) uintptr
)

const (
	ropSrcCopy     = 0x00CC0020
	ropCaptureBlt  = 0x40000000
	dibRGBColors   = 0
	monitorPrimary = 1
)

type rect struct {
	Left, Top, Right, Bottom int32
}

type monitorInfo struct {
	cbSize    uint32
	rcMonitor rect
	rcWork    rect
	dwFlags   uint32
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]uint32
}

func loadGDI() {
	user32Once.Do(func() {
		// user32/gdi32 are system DLLs and always present; NewLazyDLL panics
		// only if the load fails.
		user32Lib = syscall.NewLazyDLL("user32.dll").Handle()
		gdi32Lib = syscall.NewLazyDLL("gdi32.dll").Handle()
		purego.RegisterLibFunc(&enumDisplayMonitors, user32Lib, "EnumDisplayMonitors")
		purego.RegisterLibFunc(&getMonitorInfoW, user32Lib, "GetMonitorInfoW")
		purego.RegisterLibFunc(&setProcessDPIAware, user32Lib, "SetProcessDPIAware")
		purego.RegisterLibFunc(&getDC, user32Lib, "GetDC")
		purego.RegisterLibFunc(&releaseDC, user32Lib, "ReleaseDC")
		purego.RegisterLibFunc(&createCompatibleDC, gdi32Lib, "CreateCompatibleDC")
		purego.RegisterLibFunc(&createDIBSection, gdi32Lib, "CreateDIBSection")
		purego.RegisterLibFunc(&selectObject, gdi32Lib, "SelectObject")
		purego.RegisterLibFunc(&bitBlt, gdi32Lib, "BitBlt")
		purego.RegisterLibFunc(&deleteDC, gdi32Lib, "DeleteDC")
		purego.RegisterLibFunc(&deleteObject, gdi32Lib, "DeleteObject")

		// Capture and enumeration should use physical pixels, not the
		// virtualized coordinates a DPI-unaware process would see.
		setProcessDPIAware()
	})
}

// ---------------------------------------------------------------------------
// Display enumeration
// ---------------------------------------------------------------------------

type winDisplay struct {
	Left, Top, Right, Bottom int32
	Primary                  bool
}

func scanDisplays() ([]winDisplay, error) {
	loadGDI()
	var out []winDisplay
	cb := purego.NewCallback(func(hmon, _ uintptr, r *rect, _ uintptr) uintptr {
		mi := monitorInfo{cbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
		if getMonitorInfoW(hmon, unsafe.Pointer(&mi)) != 0 {
			out = append(out, winDisplay{
				Left:    mi.rcMonitor.Left,
				Top:     mi.rcMonitor.Top,
				Right:   mi.rcMonitor.Right,
				Bottom:  mi.rcMonitor.Bottom,
				Primary: mi.dwFlags&monitorPrimary != 0,
			})
		}
		return 1
	})
	if enumDisplayMonitors(0, nil, cb, 0) == 0 {
		return nil, fmt.Errorf("windows: EnumDisplayMonitors failed")
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("windows: no displays reported")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// GDI grabber: BitBlt from the screen DC into a 32bpp DIB section, so the
// pixels are readable in place as packed BGRA.
// ---------------------------------------------------------------------------

type gdiGrabber struct {
	hdcScreen uintptr
	hdcMem    uintptr
	hbm       uintptr
	hbmOld    uintptr
	x, y      int
	w, h      int
	buf       []byte
}

func newGDICapture(x, y, w, h int) (*gdiGrabber, error) {
	hdcScreen := getDC(0)
	if hdcScreen == 0 {
		return nil, fmt.Errorf("windows: GetDC failed")
	}
	hdcMem := createCompatibleDC(hdcScreen)
	if hdcMem == 0 {
		releaseDC(0, hdcScreen)
		return nil, fmt.Errorf("windows: CreateCompatibleDC failed")
	}
	bmi := bitmapInfo{
		Header: bitmapInfoHeader{
			Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
			Width:       int32(w),
			Height:      -int32(h), // top-down rows
			Planes:      1,
			BitCount:    32,
			Compression: 0, // BI_RGB
		},
	}
	// A DIB section gives direct access to the pixels after the BitBlt --
	// no GetDIBits round trip needed.
	var bits unsafe.Pointer
	hbm := createDIBSection(hdcScreen, unsafe.Pointer(&bmi), dibRGBColors, &bits, 0, 0)
	if hbm == 0 || bits == nil {
		deleteDC(hdcMem)
		releaseDC(0, hdcScreen)
		return nil, fmt.Errorf("windows: CreateDIBSection(%dx%d) failed", w, h)
	}
	hbmOld := selectObject(hdcMem, hbm)
	return &gdiGrabber{
		hdcScreen: hdcScreen,
		hdcMem:    hdcMem,
		hbm:       hbm,
		hbmOld:    hbmOld,
		x:         x,
		y:         y,
		w:         w,
		h:         h,
		buf:       unsafe.Slice((*byte)(bits), w*h*4),
	}, nil
}

func (g *gdiGrabber) grab() ([]byte, int, int, error) {
	if bitBlt(g.hdcMem, 0, 0, g.w, g.h, g.hdcScreen, g.x, g.y, ropSrcCopy|ropCaptureBlt) == 0 {
		return nil, 0, 0, fmt.Errorf("windows: BitBlt failed")
	}
	// GDI leaves the reserved byte at 0; opaque alpha keeps downstream
	// encoders from treating the frame as fully transparent.
	for i := 3; i < len(g.buf); i += 4 {
		g.buf[i] = 0xFF
	}
	out := make([]byte, len(g.buf))
	copy(out, g.buf)
	return out, g.w, g.h, nil
}

func (g *gdiGrabber) close() error {
	if g.hbm != 0 {
		selectObject(g.hdcMem, g.hbmOld)
		deleteObject(g.hbm)
		g.hbm = 0
	}
	if g.hdcMem != 0 {
		deleteDC(g.hdcMem)
		g.hdcMem = 0
	}
	if g.hdcScreen != 0 {
		releaseDC(0, g.hdcScreen)
		g.hdcScreen = 0
	}
	return nil
}

// ---------------------------------------------------------------------------
// Pipeline
// ---------------------------------------------------------------------------

type windowsPipeline struct{}

// New returns a Windows capture pipeline backed by GDI readback.
func New() pipelines.Pipeline {
	return &windowsPipeline{}
}

func (p *windowsPipeline) SupportedFormats() []pipelines.FrameFormat {
	return []pipelines.FrameFormat{pipelines.FormatBGRA}
}

func (p *windowsPipeline) ListDisplays(ctx context.Context) ([]pipelines.DisplayMeta, error) {
	disps, err := scanDisplays()
	if err != nil {
		return nil, err
	}
	out := make([]pipelines.DisplayMeta, len(disps))
	for i, d := range disps {
		out[i] = pipelines.DisplayMeta{
			ID:          uint32(i),
			Width:       int(d.Right - d.Left),
			Height:      int(d.Bottom - d.Top),
			X:           int(d.Left),
			Y:           int(d.Top),
			RefreshRate: 60,
		}
	}
	return out, nil
}

func (p *windowsPipeline) StartStream(ctx context.Context, displayID uint32, fps int) (pipelines.FrameStream, error) {
	if fps <= 0 {
		fps = 60
	}
	disps, err := scanDisplays()
	if err != nil {
		return nil, err
	}
	if int(displayID) >= len(disps) {
		return nil, fmt.Errorf("windows: display %d not found (%d available)", displayID, len(disps))
	}
	d := disps[displayID]
	g, err := newGDICapture(int(d.Left), int(d.Top), int(d.Right-d.Left), int(d.Bottom-d.Top))
	if err != nil {
		return nil, err
	}
	return newFrameStream(ctx, g, fps), nil
}

// ---------------------------------------------------------------------------
// Frame stream (ticker-driven, same cadence model as the linux grabber loop)
// ---------------------------------------------------------------------------

type grabber interface {
	grab() (bgra []byte, w, h int, err error)
	close() error
}

type frameStream struct {
	ch        chan pipelines.EncodedFrame
	cancel    context.CancelFunc
	closeOnce sync.Once
	g         grabber
	done      chan struct{}
}

func newFrameStream(ctx context.Context, g grabber, fps int) *frameStream {
	ctx, cancel := context.WithCancel(ctx)
	fs := &frameStream{
		ch:     make(chan pipelines.EncodedFrame, 4),
		cancel: cancel,
		g:      g,
		done:   make(chan struct{}),
	}
	go func() {
		fs.run(ctx, fps)
		close(fs.done)
	}()
	return fs
}

func (fs *frameStream) run(ctx context.Context, fps int) {
	defer func() {
		_ = fs.g.close()
		close(fs.ch)
	}()
	delay := time.Duration(int64(time.Second) / int64(fps))
	ticker := time.NewTicker(delay)
	defer ticker.Stop()
	for range ticker.C {
		select {
		case <-ctx.Done():
			return
		default:
		}
		bgra, w, h, err := fs.g.grab()
		if err != nil {
			return
		}
		select {
		case fs.ch <- pipelines.EncodedFrame{Data: bgra, Format: pipelines.FormatBGRA, Width: w, Height: h}:
		case <-ctx.Done():
			return
		}
	}
}

func (fs *frameStream) Frames() <-chan pipelines.EncodedFrame {
	return fs.ch
}

func (fs *frameStream) Close() error {
	fs.closeOnce.Do(func() {
		fs.cancel()
		<-fs.done
	})
	return nil
}
