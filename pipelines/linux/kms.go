//go:build linux

// Package linux implements the captured pipelines for Linux using libdrm
// (KMS) and libgbm, loaded at runtime via purego so the build needs no C
// headers or cgo. Capture currently yields BGRA frames over the socket;
// real encode (DMA-BUF -> nvh264enc) stays in the agent's ffmpeg path.
package linux

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// ---------------------------------------------------------------------------
// libdrm bindings (loaded lazily via purego). Pointer-returning functions
// return unsafe.Pointer (not uintptr) to keep go vet's unsafeptr check happy
// when we cast them to mirrored C structs.
// ---------------------------------------------------------------------------

const drmLibName = "libdrm.so.2"

var (
	drmOnce sync.Once
	drmLib  uintptr

	drmModeGetResources  func(fd int) unsafe.Pointer
	drmModeFreeResources func(res unsafe.Pointer)
	drmModeGetConnector  func(fd int, id uint32) unsafe.Pointer
	drmModeFreeConnector func(c unsafe.Pointer)
	drmModeGetEncoder    func(fd int, id uint32) unsafe.Pointer
	drmModeFreeEncoder   func(e unsafe.Pointer)
	drmModeGetCrtc       func(fd int, id uint32) unsafe.Pointer
	drmModeFreeCrtc      func(c unsafe.Pointer)
	drmModeGetFB2        func(fd int, id uint32) unsafe.Pointer
	drmModeFreeFB2       func(f unsafe.Pointer)
	drmPrimeHandleToFD   func(fd int, handle uint32, flags int) int
)

func loadDRM() {
	drmOnce.Do(func() {
		h, err := purego.Dlopen(drmLibName, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			drmLib = 0
			return
		}
		drmLib = h
		purego.RegisterLibFunc(&drmModeGetResources, drmLib, "drmModeGetResources")
		purego.RegisterLibFunc(&drmModeFreeResources, drmLib, "drmModeFreeResources")
		purego.RegisterLibFunc(&drmModeGetConnector, drmLib, "drmModeGetConnector")
		purego.RegisterLibFunc(&drmModeFreeConnector, drmLib, "drmModeFreeConnector")
		purego.RegisterLibFunc(&drmModeGetEncoder, drmLib, "drmModeGetEncoder")
		purego.RegisterLibFunc(&drmModeFreeEncoder, drmLib, "drmModeFreeEncoder")
		purego.RegisterLibFunc(&drmModeGetCrtc, drmLib, "drmModeGetCrtc")
		purego.RegisterLibFunc(&drmModeFreeCrtc, drmLib, "drmModeFreeCrtc")
		purego.RegisterLibFunc(&drmModeGetFB2, drmLib, "drmModeGetFB2")
		purego.RegisterLibFunc(&drmModeFreeFB2, drmLib, "drmModeFreeFB2")
		purego.RegisterLibFunc(&drmPrimeHandleToFD, drmLib, "drmPrimeHandleToFD")
	})
}

// ---------------------------------------------------------------------------
// Mirrored C structs (amd64 / LP64 layout). Field order and types match the
// libdrm definitions so reads via unsafe.Pointer are correct. Pointer fields
// are unsafe.Pointer (mirroring C uint32_t*); scalar fields keep their C
// types so the in-memory layout matches exactly.
// ---------------------------------------------------------------------------

type drmModeRes struct {
	CountFBs        int32
	FBs             unsafe.Pointer
	CountCrtcs      int32
	Crtcs           unsafe.Pointer
	CountConnectors int32
	Connectors      unsafe.Pointer
	CountEncoders   int32
	Encoders        unsafe.Pointer
	MinWidth        int32
	MaxWidth        int32
	MinHeight       int32
	MaxHeight       int32
}

type drmModeConnector struct {
	ConnectorID     uint32
	EncoderID       uint32
	ConnectorType   uint32
	ConnectorTypeID uint32
	Connection      int32
	MmWidth         uint32
	MmHeight        uint32
	Subpixel        int32
	CountModes      int32
	Modes           unsafe.Pointer
	CountProps      int32
	Props           unsafe.Pointer
	PropValues      unsafe.Pointer
	CountEncoders   int32
	Encoders        unsafe.Pointer
}

type drmModeEncoder struct {
	EncoderID      uint32
	EncoderType    uint32
	CrtcID         uint32
	PossibleCrtcs  uint32
	PossibleClones uint32
}

type drmModeModeInfo struct {
	Clock      uint32
	HDisplay   uint16
	HSyncStart uint16
	HSyncEnd   uint16
	HTotal     uint16
	HSkew      uint16
	VDisplay   uint16
	VSyncStart uint16
	VSyncEnd   uint16
	VTotal     uint16
	VScan      uint16
	VRefresh   uint32
	Flags      uint32
	Type       uint32
	Name       [32]byte
}

// drmModeCrtc as we only read the leading scalar fields; safe to truncate.
type drmModeCrtc struct {
	CrtcID   uint32
	BufferID uint32
	X        uint32
	Y        uint32
	Width    uint32
	Height   uint32
}

type drmModeFB2 struct {
	FbID        uint32
	Width       uint32
	Height      uint32
	PixelFormat uint32
	Modifier    uint64
	Handles     [4]uint32
	Pitches     [4]uint32
	Offsets     [4]uint32
	NumPlanes   uint32
	_           uint32 // padding so Flags (u64) is 8-byte aligned
	Flags       uint64
}

const (
	drmModeConnected     = 1
	drmModeTypePreferred = 1 << 1 // DRM_MODE_TYPE_PREFERRED
	drmFormatModLinear   = 0
	drmFormatXRGB8888    = 0x34325258
	drmFormatARGB8888    = 0x34325241
	drmFormatRGBX8888    = 0x38445258
	drmFormatBGRX8888    = 0x38585242
)

// linuxDisplay is a resolved display we can re-open for streaming.
type linuxDisplay struct {
	ID      uint32
	DevPath string
	ConnID  uint32
	CrtcID  uint32
	Width   int
	Height  int
	X       int
	Y       int
	Refresh float64
}

// scanDRMDisplays enumerates connected DRM connectors across /dev/dri/card*.
func scanDRMDisplays() ([]linuxDisplay, error) {
	loadDRM()
	if drmLib == 0 {
		return nil, fmt.Errorf("linux/kms: libdrm (%s) not available", drmLibName)
	}

	paths, _ := filepath.Glob("/dev/dri/card*")
	sort.Strings(paths)

	var out []linuxDisplay
	var permErr error
	id := uint32(0)

	for _, p := range paths {
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			if os.IsPermission(err) {
				if permErr == nil {
					permErr = fmt.Errorf("linux/kms: cannot open %s: %v - add the user to the 'video' (or 'render') group", p, err)
				}
			}
			continue
		}
		fd := int(f.Fd())

		res := drmModeGetResources(fd)
		if res == nil {
			f.Close()
			continue
		}
		resPtr := (*drmModeRes)(res)
		n := int(resPtr.CountConnectors)
		if n > 0 {
			connIDs := unsafe.Slice((*uint32)(resPtr.Connectors), n)
			for _, cid := range connIDs {
				cptr := drmModeGetConnector(fd, cid)
				if cptr == nil {
					continue
				}
				conn := (*drmModeConnector)(cptr)
				if conn.Connection != drmModeConnected || conn.CountModes <= 0 {
					drmModeFreeConnector(cptr)
					continue
				}
				modes := unsafe.Slice((*drmModeModeInfo)(conn.Modes), conn.CountModes)
				mi := modes[0]
				for _, m := range modes {
					if m.Type&drmModeTypePreferred != 0 {
						mi = m
						break
					}
				}

				x, y := 0, 0
				crtcID := uint32(0)
				if conn.EncoderID != 0 {
					eptr := drmModeGetEncoder(fd, conn.EncoderID)
					if eptr != nil {
						enc := (*drmModeEncoder)(eptr)
						crtcID = enc.CrtcID
						if crtcID != 0 {
							cptr2 := drmModeGetCrtc(fd, crtcID)
							if cptr2 != nil {
								crtc := (*drmModeCrtc)(cptr2)
								x, y = int(crtc.X), int(crtc.Y)
								drmModeFreeCrtc(cptr2)
							}
						}
						drmModeFreeEncoder(eptr)
					}
				}

				out = append(out, linuxDisplay{
					ID:      id,
					DevPath: p,
					ConnID:  cid,
					CrtcID:  crtcID,
					Width:   int(mi.HDisplay),
					Height:  int(mi.VDisplay),
					X:       x,
					Y:       y,
					Refresh: float64(mi.VRefresh),
				})
				id++
				drmModeFreeConnector(cptr)
			}
		}
		drmModeFreeResources(res)
		f.Close()
	}

	if len(out) == 0 {
		if permErr != nil {
			return nil, permErr
		}
		return nil, fmt.Errorf("linux/kms: no connected DRM displays found under /dev/dri/card*")
	}
	return out, nil
}

// newKMSCapture attempts a real framebuffer readback of the CRTC's current
// scanout buffer via drmModeGetFB2 + prime handle -> dma-buf -> mmap. It
// requires a linearly laid-out framebuffer (most compositors use tiled
// buffers, in which case it returns an error and the caller falls back to a
// synthetic source). This is the stepping stone to the eventual DMA-BUF ->
// nvh264enc encode path.
func newKMSCapture(d *linuxDisplay) (grabber, error) {
	loadDRM()
	if drmLib == 0 {
		return nil, fmt.Errorf("libdrm unavailable")
	}

	f, err := os.OpenFile(d.DevPath, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd())

	if d.CrtcID == 0 {
		cptr := drmModeGetConnector(fd, d.ConnID)
		if cptr == nil {
			f.Close()
			return nil, fmt.Errorf("connector %d gone", d.ConnID)
		}
		conn := (*drmModeConnector)(cptr)
		if conn.EncoderID != 0 {
			eptr := drmModeGetEncoder(fd, conn.EncoderID)
			if eptr != nil {
				d.CrtcID = (*drmModeEncoder)(eptr).CrtcID
				drmModeFreeEncoder(eptr)
			}
		}
		drmModeFreeConnector(cptr)
	}
	if d.CrtcID == 0 {
		f.Close()
		return nil, fmt.Errorf("no CRTC bound to display")
	}

	cptr := drmModeGetCrtc(fd, d.CrtcID)
	if cptr == nil {
		f.Close()
		return nil, fmt.Errorf("get CRTC failed")
	}
	crtc := (*drmModeCrtc)(cptr)
	fbID := crtc.BufferID
	drmModeFreeCrtc(cptr)
	if fbID == 0 {
		f.Close()
		return nil, fmt.Errorf("CRTC has no framebuffer (no active scanout)")
	}

	fb2p := drmModeGetFB2(fd, fbID)
	if fb2p == nil {
		f.Close()
		return nil, fmt.Errorf("drmModeGetFB2 failed")
	}
	fb2 := (*drmModeFB2)(fb2p)
	width, height := int(fb2.Width), int(fb2.Height)
	modifier := fb2.Modifier
	handle := fb2.Handles[0]
	pitch := int(fb2.Pitches[0])
	format := fb2.PixelFormat
	drmModeFreeFB2(fb2p)

	if modifier != drmFormatModLinear {
		f.Close()
		return nil, fmt.Errorf("scanout is tiled (modifier 0x%x); linear readback only", modifier)
	}
	conv, ok := bgraConverter(format)
	if !ok {
		f.Close()
		return nil, fmt.Errorf("unsupported framebuffer format 0x%x", format)
	}

	dmabuf := drmPrimeHandleToFD(fd, handle, 0)
	if dmabuf < 0 {
		f.Close()
		return nil, fmt.Errorf("drmPrimeHandleToFD failed")
	}
	mapped, err := mmapRO(dmabuf, pitch*height)
	if err != nil {
		closeFD(dmabuf)
		f.Close()
		return nil, fmt.Errorf("mmap scanout: %w", err)
	}

	log.Printf("linux/kms: capturing %dx%d (pitch %d, fmt 0x%x) via dma-buf readback",
		width, height, pitch, format)

	return &kmsGrabber{
		f:      f,
		dmabuf: dmabuf,
		mapped: mapped,
		conv:   conv,
		width:  width,
		height: height,
		pitch:  pitch,
	}, nil
}

type kmsGrabber struct {
	f      *os.File
	dmabuf int
	mapped []byte
	conv   func(src []byte, pitch, w, h int) []byte
	width  int
	height int
	pitch  int
}

func (g *kmsGrabber) grab() ([]byte, int, int, error) {
	return g.conv(g.mapped, g.pitch, g.width, g.height), g.width, g.height, nil
}

func (g *kmsGrabber) close() error {
	munmap(g.mapped)
	closeFD(g.dmabuf)
	return g.f.Close()
}

// bgraConverter returns a converter for known 32bpp DRM formats, mapping the
// source memory layout to BGRA.
func bgraConverter(format uint32) (func(src []byte, pitch, w, h int) []byte, bool) {
	switch format {
	case drmFormatXRGB8888, drmFormatARGB8888:
		return convXRGB, true
	case drmFormatRGBX8888:
		return convRGBX, true
	case drmFormatBGRX8888:
		return convBGRX, true
	}
	return nil, false
}

func convXRGB(src []byte, pitch, w, h int) []byte {
	out := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		row := src[y*pitch:]
		for x := 0; x < w; x++ {
			o := (y*w + x) * 4
			out[o], out[o+1], out[o+2], out[o+3] = row[x*4], row[x*4+1], row[x*4+2], 0xFF
		}
	}
	return out
}

func convRGBX(src []byte, pitch, w, h int) []byte {
	out := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		row := src[y*pitch:]
		for x := 0; x < w; x++ {
			o := (y*w + x) * 4
			// memory: R,G,B,X
			out[o], out[o+1], out[o+2], out[o+3] = row[x*4+2], row[x*4+1], row[x*4], 0xFF
		}
	}
	return out
}

func convBGRX(src []byte, pitch, w, h int) []byte {
	out := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		row := src[y*pitch:]
		for x := 0; x < w; x++ {
			o := (y*w + x) * 4
			// memory: B,G,R,X
			out[o], out[o+1], out[o+2], out[o+3] = row[x*4], row[x*4+1], row[x*4+2], 0xFF
		}
	}
	return out
}
