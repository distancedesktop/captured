//go:build windows

package pipelines

import (
	"context"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dxgi: DXGI Desktop Duplication. AcquireNextFrame blocks until the compositor
// presents something, so the source sets the rate (capped at the requested
// fps). When nothing changes, the last frame is sent again at the requested
// fps so the stream never goes quiet. Each frame is copied into a staging
// texture on the GPU, mapped, and packed into BGRA.
//
// Runs in the interactive user's session. The secure desktop (UAC, lock
// screen) can only be duplicated by SYSTEM; for everyone else the stream keeps
// repeating the last frame until the normal desktop is back. The cursor isn't
// in the image (DXGI reports it separately).

type dxgi struct{}

func init() { Register(dxgi{}, 30) }

func (dxgi) Name() string { return "dxgi" }

func (dxgi) Probe(ctx context.Context) error {
	outs, err := dxgiOutputs()
	if err != nil {
		return err
	}
	if len(outs) == 0 {
		return errors.New("no outputs attached to the desktop")
	}
	return onOwnThread(func() error {
		g, err := openDXGI(outs[0], 60)
		if err != nil {
			return err
		}
		return g.close()
	})
}

func (dxgi) ListDisplays(ctx context.Context) ([]Display, error) {
	outs, err := dxgiOutputs()
	if err != nil {
		return nil, err
	}
	return dxgiDisplays(outs), nil
}

func (dxgi) Start(ctx context.Context, displayID uint32, fps int) (Stream, error) {
	outs, err := dxgiOutputs()
	if err != nil {
		return nil, err
	}
	i, err := pickDisplay(dxgiDisplays(outs), displayID)
	if err != nil {
		return nil, err
	}
	o := outs[i]
	return startStream(ctx, func() (grabber, error) { return openDXGI(o, fps) }, fps, false)
}

// COM plumbing. A COM object is a pointer to a vtable pointer.

type com struct{ vtbl *[128]uintptr }

func (o *com) call(method int, args ...uintptr) uintptr {
	r, _, _ := syscall.SyscallN(o.vtbl[method], append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)...)
	return r
}

func (o *com) release() {
	if o != nil {
		o.call(2)
	}
}

func ptr[T any](p *T) uintptr { return uintptr(unsafe.Pointer(p)) }

// vtable slots (IUnknown 0-2, IDXGIObject 3-6, ID3D11DeviceChild 3-6).
const (
	vQueryInterface        = 0
	vFactoryEnumAdapters1  = 12
	vAdapterEnumOutputs    = 7
	vOutputGetDesc         = 7
	vOutput1DuplicateOut   = 22
	vDuplGetDesc           = 7
	vDuplAcquireNextFrame  = 8
	vDuplReleaseFrame      = 14
	vDeviceCreateTexture2D = 5
	vContextMap            = 14
	vContextUnmap          = 15
	vContextCopyResource   = 47
	vTexture2DGetDesc      = 10
)

var (
	iidIDXGIFactory1   = mustGUID("{770aae78-f26f-4dba-a829-253c83d1b387}")
	iidIDXGIOutput1    = mustGUID("{00cddea8-939b-4b83-a340-a685226666cc}")
	iidID3D11Texture2D = mustGUID("{6f15aaf2-d208-4e89-9ab4-489535d34f9c}")
)

func mustGUID(s string) windows.GUID {
	g, err := windows.GUIDFromString(s)
	if err != nil {
		panic(err)
	}
	return g
}

var (
	dxgiDLL  = windows.NewLazySystemDLL("dxgi.dll")
	d3d11DLL = windows.NewLazySystemDLL("d3d11.dll")
	user32   = windows.NewLazySystemDLL("user32.dll")

	procCreateDXGIFactory1            = dxgiDLL.NewProc("CreateDXGIFactory1")
	procD3D11CreateDevice             = d3d11DLL.NewProc("D3D11CreateDevice")
	procSetProcessDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")
	procGetDisplayConfigBufferSizes   = user32.NewProc("GetDisplayConfigBufferSizes")
	procQueryDisplayConfig            = user32.NewProc("QueryDisplayConfig")
	procDisplayConfigGetDeviceInfo    = user32.NewProc("DisplayConfigGetDeviceInfo")
	procEnumDisplaySettingsW          = user32.NewProc("EnumDisplaySettingsW")
	procOpenInputDesktop              = user32.NewProc("OpenInputDesktop")
	procSetThreadDesktop              = user32.NewProc("SetThreadDesktop")
	procCloseDesktop                  = user32.NewProc("CloseDesktop")
	winmm                             = windows.NewLazySystemDLL("winmm.dll")
	procTimeBeginPeriod               = winmm.NewProc("timeBeginPeriod")
	procTimeEndPeriod                 = winmm.NewProc("timeEndPeriod")
)

// HRESULTs, with what they mean for capture.

type hresult uint32

const (
	hrAccessDenied        hresult = 0x80070005
	hrNoInterface         hresult = 0x80004002
	hrInvalidArg          hresult = 0x80070057
	hrOutOfMemory         hresult = 0x8007000E
	hrInvalidCall         hresult = 0x887A0001
	hrNotFound            hresult = 0x887A0002
	hrUnsupported         hresult = 0x887A0004
	hrDeviceRemoved       hresult = 0x887A0005
	hrDeviceReset         hresult = 0x887A0007
	hrNotCurrentlyAvail   hresult = 0x887A0022
	hrAccessLost          hresult = 0x887A0026
	hrWaitTimeout         hresult = 0x887A0027
	hrSessionDisconnected hresult = 0x887A0028
)

var hrText = map[hresult]string{
	hrAccessDenied:        "E_ACCESSDENIED: the secure desktop (UAC, lock screen) is showing, or captured isn't running in the logged-in user's session",
	hrNoInterface:         "E_NOINTERFACE",
	hrInvalidArg:          "E_INVALIDARG",
	hrOutOfMemory:         "E_OUTOFMEMORY",
	hrInvalidCall:         "DXGI_ERROR_INVALID_CALL",
	hrNotFound:            "DXGI_ERROR_NOT_FOUND",
	hrUnsupported:         "DXGI_ERROR_UNSUPPORTED: this GPU/driver can't duplicate this output",
	hrDeviceRemoved:       "DXGI_ERROR_DEVICE_REMOVED: the GPU was removed or its driver restarted",
	hrDeviceReset:         "DXGI_ERROR_DEVICE_RESET",
	hrNotCurrentlyAvail:   "DXGI_ERROR_NOT_CURRENTLY_AVAILABLE: too many apps are already duplicating this output",
	hrAccessLost:          "DXGI_ERROR_ACCESS_LOST",
	hrWaitTimeout:         "DXGI_ERROR_WAIT_TIMEOUT",
	hrSessionDisconnected: "DXGI_ERROR_SESSION_DISCONNECTED: the remote desktop session is disconnected",
}

func (h hresult) Error() string {
	if s, ok := hrText[h]; ok {
		return fmt.Sprintf("%s (0x%08X)", s, uint32(h))
	}
	return fmt.Sprintf("HRESULT 0x%08X", uint32(h))
}

func hrErr(r uintptr) error {
	if int32(r) >= 0 {
		return nil
	}
	return hresult(r)
}

// transient: the desktop is temporarily out of reach (secure desktop, mode
// switch, fullscreen app taking over, RDP reconnect). Worth retrying.
func transient(err error) bool {
	var h hresult
	if !errors.As(err, &h) {
		return false
	}
	switch h {
	case hrAccessDenied, hrAccessLost, hrNotCurrentlyAvail, hrSessionDisconnected:
		return true
	}
	return false
}

// Without this, a scaled desktop reports virtualized (DPI-scaled) sizes.
var dpiOnce sync.Once

func dpiAware() {
	dpiOnce.Do(func() {
		if procSetProcessDpiAwarenessContext.Find() == nil {
			procSetProcessDpiAwarenessContext.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
		}
	})
}

// onOwnThread runs f on a fresh OS thread that is thrown away afterwards, so
// SetThreadDesktop can't leak onto a thread other goroutines use.
func onOwnThread(f func() error) error {
	ch := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		ch <- f()
	}()
	return <-ch
}

// Output enumeration.

type dxgiRect struct{ Left, Top, Right, Bottom int32 }

type dxgiOutputDesc struct {
	DeviceName         [32]uint16
	DesktopCoordinates dxgiRect
	AttachedToDesktop  int32
	Rotation           uint32
	Monitor            uintptr
}

type dxgiOutput struct {
	device   string // GDI name, \\.\DISPLAY1
	name     string
	rect     dxgiRect
	rotation uint32
	hz       float64
}

func newFactory() (*com, error) {
	if err := procCreateDXGIFactory1.Find(); err != nil {
		return nil, fmt.Errorf("dxgi.dll: %w", err)
	}
	var f *com
	r, _, _ := procCreateDXGIFactory1.Call(ptr(&iidIDXGIFactory1), ptr(&f))
	if err := hrErr(r); err != nil {
		return nil, fmt.Errorf("CreateDXGIFactory1: %w", err)
	}
	return f, nil
}

// eachOutput calls fn for every output of every adapter, in DXGI order. fn
// owns neither object; returning true stops the walk with both still alive
// (the caller releases them).
func eachOutput(f *com, fn func(a, o *com, d *dxgiOutputDesc) bool) (a, o *com, err error) {
	for ai := 0; ; ai++ {
		var ad *com
		r := f.call(vFactoryEnumAdapters1, uintptr(ai), ptr(&ad))
		if hresult(r) == hrNotFound {
			return nil, nil, nil
		}
		if err := hrErr(r); err != nil {
			return nil, nil, fmt.Errorf("EnumAdapters1: %w", err)
		}
		for oi := 0; ; oi++ {
			var out *com
			r := ad.call(vAdapterEnumOutputs, uintptr(oi), ptr(&out))
			if hresult(r) == hrNotFound {
				break
			}
			if err := hrErr(r); err != nil {
				ad.release()
				return nil, nil, fmt.Errorf("EnumOutputs: %w", err)
			}
			var d dxgiOutputDesc
			if err := hrErr(out.call(vOutputGetDesc, ptr(&d))); err != nil {
				out.release()
				ad.release()
				return nil, nil, fmt.Errorf("IDXGIOutput::GetDesc: %w", err)
			}
			if fn(ad, out, &d) {
				return ad, out, nil
			}
			out.release()
		}
		ad.release()
	}
}

func dxgiOutputs() ([]dxgiOutput, error) {
	dpiAware()
	f, err := newFactory()
	if err != nil {
		return nil, err
	}
	defer f.release()
	info := displayConfigInfo()
	var outs []dxgiOutput
	_, _, err = eachOutput(f, func(_, _ *com, d *dxgiOutputDesc) bool {
		if d.AttachedToDesktop == 0 {
			return false
		}
		o := dxgiOutput{device: windows.UTF16ToString(d.DeviceName[:]), rect: d.DesktopCoordinates, rotation: d.Rotation}
		o.name = strings.TrimPrefix(o.device, `\\.\`)
		if i, ok := info[o.device]; ok {
			if i.name != "" {
				o.name += " (" + i.name + ")"
			}
			o.hz = i.hz
		}
		if o.hz == 0 {
			o.hz = gdiRefresh(o.device)
		}
		outs = append(outs, o)
		return false
	})
	return outs, err
}

func dxgiDisplays(outs []dxgiOutput) []Display {
	ds := make([]Display, len(outs))
	for i, o := range outs {
		ds[i] = Display{
			ID:          uint32(i),
			Name:        o.name,
			Width:       int(o.rect.Right - o.rect.Left),
			Height:      int(o.rect.Bottom - o.rect.Top),
			X:           int(o.rect.Left),
			Y:           int(o.rect.Top),
			RefreshRate: o.hz,
		}
	}
	return ds
}

// Monitor names and exact refresh rates come from the DisplayConfig API,
// keyed by GDI device name.

type dcPathInfo struct {
	SrcAdapterLow, SrcAdapterHigh, SrcID, SrcModeIdx, SrcFlags     uint32
	TgtAdapterLow, TgtAdapterHigh, TgtID, TgtModeIdx               uint32
	TgtTech, TgtRotation, TgtScaling, TgtRefreshNum, TgtRefreshDen uint32
	TgtScanline, TgtAvailable, TgtFlags                            uint32
	Flags                                                          uint32
}

type dcHeader struct{ Type, Size, AdapterLow, AdapterHigh, ID uint32 }

type dcSourceName struct {
	Header dcHeader
	GDI    [32]uint16
}

type dcTargetName struct {
	Header            dcHeader
	Flags, Tech       uint32
	EdidMfg, EdidProd uint16
	ConnectorInstance uint32
	FriendlyName      [64]uint16
	MonitorDevicePath [128]uint16
}

type dcInfo struct {
	name string
	hz   float64
}

func displayConfigInfo() map[string]dcInfo {
	const qdcOnlyActivePaths = 2
	out := map[string]dcInfo{}
	if procQueryDisplayConfig.Find() != nil {
		return out
	}
	var np, nm uint32
	if r, _, _ := procGetDisplayConfigBufferSizes.Call(qdcOnlyActivePaths, ptr(&np), ptr(&nm)); r != 0 || np == 0 {
		return out
	}
	paths := make([]dcPathInfo, np)
	modes := make([][64]byte, nm+1)
	if r, _, _ := procQueryDisplayConfig.Call(qdcOnlyActivePaths, ptr(&np), ptr(&paths[0]), ptr(&nm), ptr(&modes[0]), 0); r != 0 {
		return out
	}
	for _, p := range paths[:np] {
		src := dcSourceName{Header: dcHeader{Type: 1, Size: uint32(unsafe.Sizeof(dcSourceName{})), AdapterLow: p.SrcAdapterLow, AdapterHigh: p.SrcAdapterHigh, ID: p.SrcID}}
		if r, _, _ := procDisplayConfigGetDeviceInfo.Call(ptr(&src)); r != 0 {
			continue
		}
		tgt := dcTargetName{Header: dcHeader{Type: 2, Size: uint32(unsafe.Sizeof(dcTargetName{})), AdapterLow: p.TgtAdapterLow, AdapterHigh: p.TgtAdapterHigh, ID: p.TgtID}}
		var i dcInfo
		if r, _, _ := procDisplayConfigGetDeviceInfo.Call(ptr(&tgt)); r == 0 {
			i.name = windows.UTF16ToString(tgt.FriendlyName[:])
		}
		if p.TgtRefreshDen != 0 {
			i.hz = math.Round(float64(p.TgtRefreshNum)/float64(p.TgtRefreshDen)*1000) / 1000
		}
		out[windows.UTF16ToString(src.GDI[:])] = i
	}
	return out
}

func gdiRefresh(device string) float64 {
	name, err := windows.UTF16PtrFromString(device)
	if err != nil {
		return 0
	}
	var dm [220]byte // DEVMODEW
	*(*uint16)(unsafe.Pointer(&dm[68])) = uint16(len(dm))
	const enumCurrentSettings = ^uintptr(0)
	if r, _, _ := procEnumDisplaySettingsW.Call(uintptr(unsafe.Pointer(name)), enumCurrentSettings, ptr(&dm)); r == 0 {
		return 0
	}
	hz := *(*uint32)(unsafe.Pointer(&dm[184]))
	if hz <= 1 { // 0 and 1 mean "hardware default"
		return 0
	}
	return float64(hz)
}

// The grabber.

type dxgiModeDesc struct {
	Width, Height             uint32
	RefreshNum, RefreshDen    uint32
	Format, Scanline, Scaling uint32
}

type dxgiDuplDesc struct {
	Mode           dxgiModeDesc
	Rotation       uint32
	InSystemMemory int32
}

type dxgiFrameInfo struct {
	LastPresentTime     int64
	LastMouseUpdateTime int64
	AccumulatedFrames   uint32
	RectsCoalesced      int32
	ProtectedMaskedOut  int32
	PointerX, PointerY  int32
	PointerVisible      int32
	TotalMetadataSize   uint32
	PointerShapeSize    uint32
}

type d3d11Texture2DDesc struct {
	Width, Height, MipLevels, ArraySize, Format uint32
	SampleCount, SampleQuality                  uint32
	Usage, BindFlags, CPUAccessFlags, MiscFlags uint32
}

type d3d11Mapped struct {
	Data       unsafe.Pointer
	RowPitch   uint32
	DepthPitch uint32
}

const (
	rot90  = 2
	rot180 = 3
	rot270 = 4

	fmtB8G8R8A8       = 87
	fmtB8G8R8X8       = 88
	fmtB8G8R8A8sRGB   = 91
	d3d11UsageStaging = 3
	d3d11CPURead      = 0x20000
	d3d11MapRead      = 1
)

var errInterrupted = errors.New("interrupted")

type dxgiGrabber struct {
	device string
	out1   *com // IDXGIOutput1
	dev    *com // ID3D11Device
	ctx    *com // ID3D11DeviceContext
	dup    *com // IDXGIOutputDuplication, nil while the desktop is out of reach
	stage  *com // staging ID3D11Texture2D
	desk   uintptr

	w, h   int // as the user sees it
	tw, th int // as the texture is laid out (unrotated scanout)
	rot    uint32

	interval time.Duration
	sent     time.Time
	have     bool // stage holds a frame
	fresh    bool // copy the next frame even if only the cursor moved
	timer    bool // timeBeginPeriod(1) is held
	stop     atomic.Bool
}

func openDXGI(o dxgiOutput, fps int) (*dxgiGrabber, error) {
	fps = clampFPS(fps)
	dpiAware()
	if err := procD3D11CreateDevice.Find(); err != nil {
		return nil, fmt.Errorf("d3d11.dll: %w", err)
	}
	g := &dxgiGrabber{device: o.device, interval: time.Second / time.Duration(fps)}
	// AcquireNextFrame's timeout rounds to the system tick (15.6 ms by
	// default), which caps an idle 60 fps stream near 40 fps.
	if procTimeBeginPeriod.Find() == nil {
		procTimeBeginPeriod.Call(1)
		g.timer = true
	}
	fail := func(err error) (*dxgiGrabber, error) {
		g.close()
		return nil, fmt.Errorf("dxgi %s: %w", o.name, err)
	}
	f, err := newFactory()
	if err != nil {
		return fail(err)
	}
	adapter, out, err := eachOutput(f, func(_, _ *com, d *dxgiOutputDesc) bool {
		return windows.UTF16ToString(d.DeviceName[:]) == o.device
	})
	f.release()
	if err != nil {
		return fail(err)
	}
	if out == nil {
		return fail(errors.New("output is gone"))
	}
	err = hrErr(out.call(vQueryInterface, ptr(&iidIDXGIOutput1), ptr(&g.out1)))
	out.release()
	if err != nil {
		adapter.release()
		return fail(fmt.Errorf("IDXGIOutput1 (needs Windows 8+): %w", err))
	}
	const driverTypeUnknown, bgraSupport, sdkVersion = 0, 0x20, 7
	r, _, _ := procD3D11CreateDevice.Call(uintptr(unsafe.Pointer(adapter)), driverTypeUnknown, 0, bgraSupport, 0, 0, sdkVersion, ptr(&g.dev), 0, ptr(&g.ctx))
	adapter.release()
	if err := hrErr(r); err != nil {
		return fail(fmt.Errorf("D3D11CreateDevice: %w", err))
	}
	if err := g.duplicate(); err != nil {
		return fail(err)
	}
	return g, nil
}

// useInputDesktop moves this thread onto whichever desktop has input. That
// only works for SYSTEM on the secure desktop; for a normal user it's a no-op
// and DuplicateOutput keeps failing until the user's desktop is back.
func (g *dxgiGrabber) useInputDesktop() {
	const genericAll = 0x10000000
	h, _, _ := procOpenInputDesktop.Call(0, 0, genericAll)
	if h == 0 {
		return
	}
	if r, _, _ := procSetThreadDesktop.Call(h); r == 0 {
		procCloseDesktop.Call(h)
		return
	}
	if g.desk != 0 {
		procCloseDesktop.Call(g.desk)
	}
	g.desk = h
}

func (g *dxgiGrabber) duplicate() error {
	g.useInputDesktop()
	var dup *com
	if err := hrErr(g.out1.call(vOutput1DuplicateOut, uintptr(unsafe.Pointer(g.dev)), ptr(&dup))); err != nil {
		return fmt.Errorf("DuplicateOutput: %w", err)
	}
	var d dxgiDuplDesc
	dup.call(vDuplGetDesc, ptr(&d))
	tw, th := int(d.Mode.Width), int(d.Mode.Height)
	w, h := tw, th
	if d.Rotation == rot90 || d.Rotation == rot270 {
		w, h = th, tw
	}
	if g.w != 0 && (w != g.w || h != g.h) {
		dup.release()
		return fmt.Errorf("%s changed mode from %dx%d to %dx%d; start a new stream", g.device, g.w, g.h, w, h)
	}
	if g.stage != nil && (tw != g.tw || th != g.th) {
		g.stage.release()
		g.stage, g.have = nil, false
	}
	g.dup, g.w, g.h, g.tw, g.th, g.rot = dup, w, h, tw, th, d.Rotation
	g.fresh = true
	return nil
}

func (g *dxgiGrabber) size() (int, int) { return g.w, g.h }

// grab returns once a new frame has arrived and the previous one went out at
// least an interval ago, or when an interval passes with nothing new (then
// the last frame goes out again).
func (g *dxgiGrabber) grab(dst []byte) error {
	deadline := g.sent.Add(g.interval)
	for !g.stop.Load() {
		if g.dup == nil {
			err := g.duplicate()
			if err != nil {
				if !transient(err) {
					return err
				}
				// UAC, lock screen, mode switch: keep the stream alive
				// with the last frame until the desktop is back.
				if g.have && !time.Now().Before(deadline) {
					return g.deliver(dst)
				}
				time.Sleep(min(50*time.Millisecond, max(time.Until(deadline), time.Millisecond)))
				continue
			}
		}
		wait := time.Until(deadline)
		if !g.have {
			wait = 100 * time.Millisecond
		}
		ms := uintptr(max(0, int64(math.Ceil(float64(wait)/float64(time.Millisecond)))))
		var info dxgiFrameInfo
		var res *com
		r := g.dup.call(vDuplAcquireNextFrame, ms, ptr(&info), ptr(&res))
		switch hresult(r) {
		case hrWaitTimeout:
			if g.have {
				return g.deliver(dst)
			}
			continue
		case hrAccessLost:
			g.dup.release()
			g.dup = nil
			continue
		}
		if err := hrErr(r); err != nil {
			return fmt.Errorf("AcquireNextFrame: %w", err)
		}
		err := g.take(res, &info)
		res.release()
		g.dup.call(vDuplReleaseFrame)
		if err != nil {
			return err
		}
		if g.have && !time.Now().Before(deadline) {
			return g.deliver(dst)
		}
	}
	return errInterrupted
}

// take copies the acquired desktop texture into the staging texture. A frame
// without a present time is only a cursor update; the image is unchanged.
func (g *dxgiGrabber) take(res *com, info *dxgiFrameInfo) error {
	if info.LastPresentTime == 0 && !g.fresh {
		return nil
	}
	var tex *com
	if err := hrErr(res.call(vQueryInterface, ptr(&iidID3D11Texture2D), ptr(&tex))); err != nil {
		return fmt.Errorf("desktop resource isn't a texture: %w", err)
	}
	defer tex.release()
	if g.stage == nil {
		var d d3d11Texture2DDesc
		tex.call(vTexture2DGetDesc, ptr(&d))
		switch d.Format {
		case fmtB8G8R8A8, fmtB8G8R8X8, fmtB8G8R8A8sRGB:
		default:
			return fmt.Errorf("desktop texture format %d isn't 8-bit BGRA", d.Format)
		}
		if int(d.Width) != g.tw || int(d.Height) != g.th {
			return fmt.Errorf("desktop texture is %dx%d, duplication says %dx%d", d.Width, d.Height, g.tw, g.th)
		}
		d.MipLevels, d.ArraySize, d.SampleCount, d.SampleQuality = 1, 1, 1, 0
		d.Usage, d.BindFlags, d.CPUAccessFlags, d.MiscFlags = d3d11UsageStaging, 0, d3d11CPURead, 0
		if err := hrErr(g.dev.call(vDeviceCreateTexture2D, ptr(&d), 0, ptr(&g.stage))); err != nil {
			return fmt.Errorf("CreateTexture2D (staging): %w", err)
		}
	}
	g.ctx.call(vContextCopyResource, uintptr(unsafe.Pointer(g.stage)), uintptr(unsafe.Pointer(tex)))
	g.have, g.fresh = true, false
	return nil
}

func (g *dxgiGrabber) deliver(dst []byte) error {
	g.sent = time.Now()
	var m d3d11Mapped
	if err := hrErr(g.ctx.call(vContextMap, uintptr(unsafe.Pointer(g.stage)), 0, d3d11MapRead, 0, ptr(&m))); err != nil {
		return fmt.Errorf("Map (staging): %w", err)
	}
	defer g.ctx.call(vContextUnmap, uintptr(unsafe.Pointer(g.stage)), 0)
	pitch := int(m.RowPitch)
	src := unsafe.Slice((*byte)(m.Data), pitch*(g.th-1)+g.tw*4)
	packBGRA(dst, src, pitch, g.tw, g.th, g.rot)
	return nil
}

// packBGRA writes the tw x th texture (rows pitch bytes apart) into dst as
// tightly packed BGRA, undoing the output's rotation so dst is upright.
func packBGRA(dst, src []byte, pitch, tw, th int, rot uint32) {
	row := tw * 4
	switch rot {
	case rot90, rot180, rot270:
	default:
		if pitch == row {
			copy(dst, src[:row*th])
			return
		}
		for y := 0; y < th; y++ {
			copy(dst[y*row:(y+1)*row], src[y*pitch:y*pitch+row])
		}
		return
	}
	p := pitch / 4
	s := unsafe.Slice((*uint32)(unsafe.Pointer(&src[0])), p*(th-1)+tw)
	d := unsafe.Slice((*uint32)(unsafe.Pointer(&dst[0])), tw*th)
	switch rot {
	case rot90: // dst is th wide, tw tall
		for y := 0; y < tw; y++ {
			for x := 0; x < th; x++ {
				d[y*th+x] = s[(th-1-x)*p+y]
			}
		}
	case rot180:
		for y := 0; y < th; y++ {
			for x := 0; x < tw; x++ {
				d[y*tw+x] = s[(th-1-y)*p+tw-1-x]
			}
		}
	case rot270:
		for y := 0; y < tw; y++ {
			for x := 0; x < th; x++ {
				d[y*th+x] = s[x*p+tw-1-y]
			}
		}
	}
}

func (g *dxgiGrabber) interrupt() { g.stop.Store(true) }

func (g *dxgiGrabber) close() error {
	g.stage.release()
	g.dup.release()
	g.ctx.release()
	g.dev.release()
	g.out1.release()
	g.stage, g.dup, g.ctx, g.dev, g.out1 = nil, nil, nil, nil, nil
	if g.desk != 0 {
		procCloseDesktop.Call(g.desk)
		g.desk = 0
	}
	if g.timer {
		procTimeEndPeriod.Call(1)
		g.timer = false
	}
	return nil
}
