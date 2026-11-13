//go:build linux

package pipelines

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"unsafe"

	"github.com/ebitengine/purego"
)

// libdrm, loaded at runtime with purego so the build needs no cgo or headers.

var (
	drmOnce sync.Once
	drmErr  error

	drmModeGetResources          func(fd int) unsafe.Pointer
	drmModeFreeResources         func(p unsafe.Pointer)
	drmModeGetConnector          func(fd int, id uint32) unsafe.Pointer
	drmModeFreeConnector         func(p unsafe.Pointer)
	drmModeGetEncoder            func(fd int, id uint32) unsafe.Pointer
	drmModeFreeEncoder           func(p unsafe.Pointer)
	drmModeGetCrtc               func(fd int, id uint32) unsafe.Pointer
	drmModeFreeCrtc              func(p unsafe.Pointer)
	drmModeGetFB2                func(fd int, id uint32) unsafe.Pointer
	drmModeFreeFB2               func(p unsafe.Pointer)
	drmPrimeHandleToFD           func(fd int, handle uint32, flags uint32, primeFD *int32) int32
	drmCloseBufferHandle         func(fd int, handle uint32) int32
	drmGetVersion                func(fd int) unsafe.Pointer
	drmFreeVersion               func(p unsafe.Pointer)
	drmGetRenderDeviceNameFromFd func(fd int) string
)

func loadDRM() error {
	drmOnce.Do(func() {
		h, err := purego.Dlopen("libdrm.so.2", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			drmErr = fmt.Errorf("libdrm.so.2: %w", err)
			return
		}
		purego.RegisterLibFunc(&drmModeGetResources, h, "drmModeGetResources")
		purego.RegisterLibFunc(&drmModeFreeResources, h, "drmModeFreeResources")
		purego.RegisterLibFunc(&drmModeGetConnector, h, "drmModeGetConnector")
		purego.RegisterLibFunc(&drmModeFreeConnector, h, "drmModeFreeConnector")
		purego.RegisterLibFunc(&drmModeGetEncoder, h, "drmModeGetEncoder")
		purego.RegisterLibFunc(&drmModeFreeEncoder, h, "drmModeFreeEncoder")
		purego.RegisterLibFunc(&drmModeGetCrtc, h, "drmModeGetCrtc")
		purego.RegisterLibFunc(&drmModeFreeCrtc, h, "drmModeFreeCrtc")
		purego.RegisterLibFunc(&drmModeGetFB2, h, "drmModeGetFB2")
		purego.RegisterLibFunc(&drmModeFreeFB2, h, "drmModeFreeFB2")
		purego.RegisterLibFunc(&drmPrimeHandleToFD, h, "drmPrimeHandleToFD")
		purego.RegisterLibFunc(&drmCloseBufferHandle, h, "drmCloseBufferHandle")
		purego.RegisterLibFunc(&drmGetVersion, h, "drmGetVersion")
		purego.RegisterLibFunc(&drmFreeVersion, h, "drmFreeVersion")
		purego.RegisterLibFunc(&drmGetRenderDeviceNameFromFd, h, "drmGetRenderDeviceNameFromFd")
	})
	return drmErr
}

// Mirrors of the libdrm structs (LP64). Only leading fields we read are kept
// where the C struct continues past them.

type drmModeRes struct {
	CountFBs        int32
	FBs             unsafe.Pointer
	CountCrtcs      int32
	Crtcs           unsafe.Pointer
	CountConnectors int32
	Connectors      unsafe.Pointer
}

type drmModeConnector struct {
	ConnectorID     uint32
	EncoderID       uint32
	ConnectorType   uint32
	ConnectorTypeID uint32
	Connection      int32
}

type drmModeEncoder struct {
	EncoderID uint32
	Type      uint32
	CrtcID    uint32
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

type drmModeCrtc struct {
	CrtcID    uint32
	BufferID  uint32
	X         uint32
	Y         uint32
	Width     uint32
	Height    uint32
	ModeValid int32
	Mode      drmModeModeInfo
}

type drmModeFB2 struct {
	FbID        uint32
	Width       uint32
	Height      uint32
	PixelFormat uint32
	Modifier    uint64
	Flags       uint32
	Handles     [4]uint32
	Pitches     [4]uint32
	Offsets     [4]uint32
}

type drmVersion struct {
	Major, Minor, Patch int32
	NameLen             int32
	Name                *byte
}

const (
	drmConnected       = 1
	drmFormatModLinear = 0
	drmFormatXRGB8888  = 0x34325258
	drmFormatARGB8888  = 0x34325241
	drmFbModifiers     = 2 // DRM_MODE_FB_MODIFIERS
)

var connectorTypeNames = map[uint32]string{
	1: "VGA", 2: "DVI-I", 3: "DVI-D", 4: "DVI-A", 5: "Composite", 6: "SVIDEO",
	7: "LVDS", 8: "Component", 9: "DIN", 10: "DP", 11: "HDMI-A", 12: "HDMI-B",
	13: "TV", 14: "eDP", 15: "Virtual", 16: "DSI", 17: "DPI", 18: "Writeback",
	19: "SPI", 20: "USB",
}

// kmsOutput is a connector that is lit right now: it has a CRTC scanning out
// a framebuffer. Unlit connectors can't be captured, so they aren't listed.
type kmsOutput struct {
	Card    string
	Driver  string
	ConnID  uint32
	CrtcID  uint32
	Name    string
	Width   int
	Height  int
	X, Y    int
	Refresh float64
}

func drmDriverName(fd int) string {
	v := drmGetVersion(fd)
	if v == nil {
		return ""
	}
	defer drmFreeVersion(v)
	ver := (*drmVersion)(v)
	if ver.Name == nil || ver.NameLen <= 0 {
		return ""
	}
	return string(unsafe.Slice(ver.Name, ver.NameLen))
}

// scanKMS lists lit outputs on every /dev/dri/card* whose driver passes keep.
func scanKMS(keep func(driver string) bool) ([]kmsOutput, error) {
	if err := loadDRM(); err != nil {
		return nil, err
	}
	cards, _ := filepath.Glob("/dev/dri/card*")
	sort.Strings(cards)
	var out []kmsOutput
	var openErr error
	for _, card := range cards {
		f, err := os.OpenFile(card, os.O_RDWR|syscall.O_CLOEXEC, 0)
		if err != nil {
			openErr = err
			continue
		}
		fd := int(f.Fd())
		driver := drmDriverName(fd)
		if keep(driver) {
			out = append(out, litOutputs(fd, card, driver)...)
		}
		f.Close()
	}
	if len(out) == 0 && openErr != nil {
		return nil, fmt.Errorf("%w (is the user in the video group?)", openErr)
	}
	return out, nil
}

func litOutputs(fd int, card, driver string) []kmsOutput {
	res := drmModeGetResources(fd)
	if res == nil {
		return nil
	}
	defer drmModeFreeResources(res)
	r := (*drmModeRes)(res)
	if r.CountConnectors <= 0 {
		return nil
	}
	var out []kmsOutput
	for _, cid := range unsafe.Slice((*uint32)(r.Connectors), r.CountConnectors) {
		cp := drmModeGetConnector(fd, cid)
		if cp == nil {
			continue
		}
		c := *(*drmModeConnector)(cp)
		drmModeFreeConnector(cp)
		if c.Connection != drmConnected || c.EncoderID == 0 {
			continue
		}
		ep := drmModeGetEncoder(fd, c.EncoderID)
		if ep == nil {
			continue
		}
		crtcID := (*drmModeEncoder)(ep).CrtcID
		drmModeFreeEncoder(ep)
		if crtcID == 0 {
			continue
		}
		crp := drmModeGetCrtc(fd, crtcID)
		if crp == nil {
			continue
		}
		crtc := *(*drmModeCrtc)(crp)
		drmModeFreeCrtc(crp)
		if crtc.BufferID == 0 || crtc.ModeValid == 0 {
			continue
		}
		name := connectorTypeNames[c.ConnectorType]
		if name == "" {
			name = "Unknown"
		}
		out = append(out, kmsOutput{
			Card:    card,
			Driver:  driver,
			ConnID:  cid,
			CrtcID:  crtcID,
			Name:    fmt.Sprintf("%s-%d", name, c.ConnectorTypeID),
			Width:   int(crtc.Mode.HDisplay),
			Height:  int(crtc.Mode.VDisplay),
			X:       int(crtc.X),
			Y:       int(crtc.Y),
			Refresh: modeRefresh(crtc.Mode),
		})
	}
	return out
}

func modeRefresh(m drmModeModeInfo) float64 {
	if m.HTotal == 0 || m.VTotal == 0 {
		return float64(m.VRefresh)
	}
	return float64(m.Clock) * 1000 / (float64(m.HTotal) * float64(m.VTotal))
}

func kmsDisplays(outs []kmsOutput) []Display {
	ds := make([]Display, len(outs))
	for i, o := range outs {
		ds[i] = Display{ID: uint32(i), Name: o.Name, Width: o.Width, Height: o.Height, X: o.X, Y: o.Y, RefreshRate: o.Refresh}
	}
	return ds
}

// currentFB returns the framebuffer the CRTC is scanning out right now. It
// changes on every page flip, so it has to be re-read for every frame.
func currentFB(fd int, crtcID uint32) (uint32, error) {
	p := drmModeGetCrtc(fd, crtcID)
	if p == nil {
		return 0, fmt.Errorf("CRTC %d is gone", crtcID)
	}
	id := (*drmModeCrtc)(p).BufferID
	drmModeFreeCrtc(p)
	if id == 0 {
		return 0, fmt.Errorf("CRTC %d has no framebuffer (display turned off?)", crtcID)
	}
	return id, nil
}

func getFB2(fd int, fbID uint32) (drmModeFB2, error) {
	p := drmModeGetFB2(fd, fbID)
	if p == nil {
		return drmModeFB2{}, fmt.Errorf("drmModeGetFB2(%d) failed", fbID)
	}
	fb := *(*drmModeFB2)(p)
	drmModeFreeFB2(p)
	return fb, nil
}

// closeFBHandles drops the GEM handles GetFB2 opened; they leak otherwise.
func closeFBHandles(fd int, fb drmModeFB2) {
	seen := map[uint32]bool{}
	for _, h := range fb.Handles {
		if h != 0 && !seen[h] {
			seen[h] = true
			drmCloseBufferHandle(fd, h)
		}
	}
}

func primeFD(fd int, handle uint32) (int, error) {
	var out int32
	if drmPrimeHandleToFD(fd, handle, syscall.O_CLOEXEC, &out) != 0 {
		return -1, fmt.Errorf("drmPrimeHandleToFD failed")
	}
	return int(out), nil
}

// scanoutFB resolves the CRTC's current framebuffer through get, retrying when
// a page flip frees the framebuffer between reading its ID and importing it.
func scanoutFB[T any](fd int, crtc uint32, get func(uint32) (T, error)) (t T, err error) {
	for range 3 {
		var id uint32
		if id, err = currentFB(fd, crtc); err != nil {
			return t, err
		}
		if t, err = get(id); err == nil {
			return t, nil
		}
	}
	return t, err
}

// fbIdentity returns the inode of plane 0's dma-buf. Framebuffer IDs get
// reused once freed, so a cached import is only valid while this matches.
func fbIdentity(fd int, fbID uint32) (uint64, error) {
	fb, err := getFB2(fd, fbID)
	if err != nil {
		return 0, err
	}
	defer closeFBHandles(fd, fb)
	if fb.Handles[0] == 0 {
		return 0, fmt.Errorf("%s", errNeedsCapSysAdmin)
	}
	buf, err := primeFD(fd, fb.Handles[0])
	if err != nil {
		return 0, err
	}
	defer syscall.Close(buf)
	var st syscall.Stat_t
	if err := syscall.Fstat(buf, &st); err != nil {
		return 0, err
	}
	return st.Ino, nil
}

// probeFBHandles checks that the kernel hands this process GEM handles for
// the output's framebuffer; without CAP_SYS_ADMIN they come back as zero.
func probeFBHandles(o kmsOutput) error {
	f, err := os.OpenFile(o.Card, os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fd := int(f.Fd())
	id, err := currentFB(fd, o.CrtcID)
	if err != nil {
		return err
	}
	fb, err := getFB2(fd, id)
	if err != nil {
		return err
	}
	closeFBHandles(fd, fb)
	if fb.Handles[0] == 0 {
		return fmt.Errorf("%s", errNeedsCapSysAdmin)
	}
	return nil
}

const errNeedsCapSysAdmin = "the kernel hides framebuffer handles from this process: give the binary CAP_SYS_ADMIN (sudo setcap cap_sys_admin+ep captured), the same thing Sunshine's KMS capture needs"
