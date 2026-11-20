//go:build linux

package pipelines

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

type x11XShm struct{}

func init() { Register(x11XShm{}, 5) }

func (x11XShm) Name() string { return "x11_xshm" }

var (
	x11Once            sync.Once
	x11Err             error
	x11Errors          atomic.Uint64
	xOpenDisplay       func(string) unsafe.Pointer
	xCloseDisplay      func(unsafe.Pointer) int32
	xDefaultScreen     func(unsafe.Pointer) int32
	xRootWindow        func(unsafe.Pointer, int32) uintptr
	xDefaultVisual     func(unsafe.Pointer, int32) unsafe.Pointer
	xDefaultDepth      func(unsafe.Pointer, int32) int32
	xDisplayWidth      func(unsafe.Pointer, int32) int32
	xDisplayHeight     func(unsafe.Pointer, int32) int32
	xGetImage          func(unsafe.Pointer, uintptr, int32, int32, uint32, uint32, uintptr, int32) unsafe.Pointer
	xDestroyImage      func(unsafe.Pointer) int32
	xSync              func(unsafe.Pointer, int32) int32
	xSetErrorHandler   func(uintptr) uintptr
	xShmQueryExtension func(unsafe.Pointer) int32
	xShmCreateImage    func(unsafe.Pointer, unsafe.Pointer, uint32, int32, unsafe.Pointer, *xShmInfo, uint32, uint32) unsafe.Pointer
	xShmAttach         func(unsafe.Pointer, *xShmInfo) int32
	xShmDetach         func(unsafe.Pointer, *xShmInfo) int32
	xShmGetImage       func(unsafe.Pointer, uintptr, unsafe.Pointer, int32, int32, uintptr) int32
	shmget             func(int32, uintptr, int32) int32
	shmat              func(int32, unsafe.Pointer, int32) unsafe.Pointer
	shmdt              func(unsafe.Pointer) int32
	shmctl             func(int32, int32, unsafe.Pointer) int32
)

func loadX11() error {
	x11Once.Do(func() {
		h, err := purego.Dlopen("libX11.so.6", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			x11Err = fmt.Errorf("libX11.so.6: %w", err)
			return
		}
		purego.RegisterLibFunc(&xOpenDisplay, h, "XOpenDisplay")
		purego.RegisterLibFunc(&xCloseDisplay, h, "XCloseDisplay")
		purego.RegisterLibFunc(&xDefaultScreen, h, "XDefaultScreen")
		purego.RegisterLibFunc(&xRootWindow, h, "XRootWindow")
		purego.RegisterLibFunc(&xDefaultVisual, h, "XDefaultVisual")
		purego.RegisterLibFunc(&xDefaultDepth, h, "XDefaultDepth")
		purego.RegisterLibFunc(&xDisplayWidth, h, "XDisplayWidth")
		purego.RegisterLibFunc(&xDisplayHeight, h, "XDisplayHeight")
		purego.RegisterLibFunc(&xGetImage, h, "XGetImage")
		purego.RegisterLibFunc(&xDestroyImage, h, "XDestroyImage")
		purego.RegisterLibFunc(&xSync, h, "XSync")
		purego.RegisterLibFunc(&xSetErrorHandler, h, "XSetErrorHandler")
		var initThreads func() int32
		purego.RegisterLibFunc(&initThreads, h, "XInitThreads")
		if initThreads() == 0 {
			x11Err = fmt.Errorf("XInitThreads failed")
			return
		}
		xSetErrorHandler(purego.NewCallback(func(unsafe.Pointer, unsafe.Pointer) int32 { x11Errors.Add(1); return 0 }))
		if h, err = purego.Dlopen("libXext.so.6", purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
			return
		}
		purego.RegisterLibFunc(&xShmQueryExtension, h, "XShmQueryExtension")
		purego.RegisterLibFunc(&xShmCreateImage, h, "XShmCreateImage")
		purego.RegisterLibFunc(&xShmAttach, h, "XShmAttach")
		purego.RegisterLibFunc(&xShmDetach, h, "XShmDetach")
		purego.RegisterLibFunc(&xShmGetImage, h, "XShmGetImage")
		if h, err = purego.Dlopen("libc.so.6", purego.RTLD_NOW|purego.RTLD_GLOBAL); err != nil {
			xShmQueryExtension = nil
			return
		}
		purego.RegisterLibFunc(&shmget, h, "shmget")
		purego.RegisterLibFunc(&shmat, h, "shmat")
		purego.RegisterLibFunc(&shmdt, h, "shmdt")
		purego.RegisterLibFunc(&shmctl, h, "shmctl")
	})
	return x11Err
}

// Leading XImage fields, with native C long and pointer widths.
type xImage struct {
	Width, Height, XOffset, Format                   int32
	Data                                             unsafe.Pointer
	ByteOrder, BitmapUnit, BitmapBitOrder, BitmapPad int32
	Depth, BytesPerLine, BitsPerPixel                int32
	RedMask, GreenMask, BlueMask                     uintptr
}

type xShmInfo struct {
	Segment  uintptr
	ID       int32
	Addr     unsafe.Pointer
	ReadOnly int32
}

type x11Grabber struct {
	display  unsafe.Pointer
	root     uintptr
	w, h     int
	image    unsafe.Pointer
	shm      xShmInfo
	attached bool
}

func openX11() (*x11Grabber, error) {
	name := os.Getenv("DISPLAY")
	if name == "" {
		return nil, fmt.Errorf("DISPLAY is unset")
	}
	if err := loadX11(); err != nil {
		return nil, err
	}
	d := xOpenDisplay(name)
	if d == nil {
		return nil, fmt.Errorf("cannot open X11 display %q", name)
	}
	s := xDefaultScreen(d)
	g := &x11Grabber{display: d, root: xRootWindow(d, s), w: int(xDisplayWidth(d, s)), h: int(xDisplayHeight(d, s)), shm: xShmInfo{ID: -1}}
	if g.w <= 0 || g.h <= 0 || g.w > 32768 || g.h > 32768 {
		g.close()
		return nil, fmt.Errorf("invalid X11 root dimensions")
	}
	if xShmQueryExtension != nil && xShmQueryExtension(d) != 0 {
		g.image = xShmCreateImage(d, xDefaultVisual(d, s), uint32(xDefaultDepth(d, s)), 2, nil, &g.shm, uint32(g.w), uint32(g.h))
		if g.image != nil {
			im := (*xImage)(g.image)
			if im.BytesPerLine > 0 {
				g.shm.ID = shmget(0, uintptr(im.BytesPerLine)*uintptr(g.h), 01000|0600)
				if g.shm.ID >= 0 {
					p := shmat(g.shm.ID, nil, 0)
					if uintptr(p) != ^uintptr(0) {
						g.shm.Addr, im.Data = p, p
						before := x11Errors.Load()
						ok := xShmAttach(d, &g.shm)
						xSync(d, 0)
						g.attached = ok != 0 && before == x11Errors.Load()
					}
					shmctl(g.shm.ID, 0, nil)
				}
			}
			if !g.attached {
				g.dropImage()
			}
		}
	}
	return g, nil
}

func (x11XShm) Probe(ctx context.Context) error {
	g, err := openX11()
	if err != nil {
		return err
	}
	defer g.close()
	return g.grab(make([]byte, g.w*g.h*4))
}

func (x11XShm) ListDisplays(ctx context.Context) ([]Display, error) {
	g, err := openX11()
	if err != nil {
		return nil, err
	}
	defer g.close()
	return []Display{{ID: 0, Name: "X11 root", Width: g.w, Height: g.h}}, nil
}

func (x11XShm) Start(ctx context.Context, displayID uint32, cfg Config) (Session, error) {
	if displayID != 0 {
		return nil, fmt.Errorf("display %d not found (1 available)", displayID)
	}
	return startSession(ctx, func() (grabber, error) { return openX11() }, cfg, true)
}

func (g *x11Grabber) size() (int, int) { return g.w, g.h }

func copyXImage(dst []byte, im *xImage, w, h int) error {
	if im.Width != int32(w) || im.Height != int32(h) || im.Data == nil || im.ByteOrder != 0 || im.BitsPerPixel != 32 || im.RedMask != 0xff0000 || im.GreenMask != 0xff00 || im.BlueMask != 0xff || int(im.BytesPerLine) < w*4 {
		return fmt.Errorf("unsupported X11 image: need little-endian 32-bit BGRX")
	}
	if len(dst) < w*h*4 {
		return fmt.Errorf("short X11 frame buffer")
	}
	src := unsafe.Slice((*byte)(im.Data), int(im.BytesPerLine)*h)
	for y := 0; y < h; y++ {
		copy(dst[y*w*4:(y+1)*w*4], src[y*int(im.BytesPerLine):y*int(im.BytesPerLine)+w*4])
	}
	setOpaque(dst[:w*h*4])
	return nil
}

func (g *x11Grabber) grab(dst []byte) error {
	if g.attached && xShmGetImage(g.display, g.root, g.image, 0, 0, ^uintptr(0)) == 0 {
		g.dropImage()
	}
	if !g.attached {
		g.image = xGetImage(g.display, g.root, 0, 0, uint32(g.w), uint32(g.h), ^uintptr(0), 2)
		if g.image == nil {
			return fmt.Errorf("XGetImage failed (display resized or inaccessible)")
		}
		defer g.dropImage()
	}
	return copyXImage(dst, (*xImage)(g.image), g.w, g.h)
}

func (g *x11Grabber) dropImage() {
	if g.attached {
		xShmDetach(g.display, &g.shm)
		xSync(g.display, 0)
		g.attached = false
	}
	if g.image != nil {
		if g.shm.Addr != nil {
			(*xImage)(g.image).Data = nil
		}
		xDestroyImage(g.image)
		g.image = nil
	}
	if g.shm.Addr != nil {
		shmdt(g.shm.Addr)
		g.shm.Addr = nil
	}
}

func (g *x11Grabber) close() error {
	g.dropImage()
	if g.display != nil {
		xCloseDisplay(g.display)
		g.display = nil
	}
	return nil
}
