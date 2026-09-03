//go:build linux

package linux

import (
	"fmt"
	"os"
	"sync"

	"github.com/ebitengine/purego"
)

// ---------------------------------------------------------------------------
// libgbm bindings. Used by the synthetic source to allocate a real linear
// GBM BO, export it as a dma-buf and mmap it — the same primitive the future
// DMA-BUF -> nvh264enc encode path will reuse.
// ---------------------------------------------------------------------------

const gbmLibName = "libgbm.so.1"

const (
	gbmFormatXRGB8888 = 0x34325258
	gbmBoUseRendering = 1 << 2
	gbmBoUseWrite     = 1 << 3
	gbmBoUseLinear    = 1 << 4
)

var (
	gbmOnce sync.Once
	gbmLib  uintptr

	gbmCreateDevice func(fd int) uintptr
	gbmDeviceDestroy func(dev uintptr)
	gbmBoCreate    func(dev uintptr, w, h, format, usage uint32) uintptr
	gbmBoDestroy   func(bo uintptr)
	gbmBoGetFD     func(bo uintptr) int
	gbmBoGetStride func(bo uintptr) uint32
)

func loadGBM() {
	gbmOnce.Do(func() {
		h, err := purego.Dlopen(gbmLibName, purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			gbmLib = 0
			return
		}
		gbmLib = h
		purego.RegisterLibFunc(&gbmCreateDevice, gbmLib, "gbm_create_device")
		purego.RegisterLibFunc(&gbmDeviceDestroy, gbmLib, "gbm_device_destroy")
		purego.RegisterLibFunc(&gbmBoCreate, gbmLib, "gbm_bo_create")
		purego.RegisterLibFunc(&gbmBoDestroy, gbmLib, "gbm_bo_destroy")
		purego.RegisterLibFunc(&gbmBoGetFD, gbmLib, "gbm_bo_get_fd")
		purego.RegisterLibFunc(&gbmBoGetStride, gbmLib, "gbm_bo_get_stride")
	})
}

// gbmBO wraps a linear XRGB8888 GBM buffer mapped for writing.
type gbmBO struct {
	boH    uintptr
	devH   uintptr
	fd     int // dma-buf fd
	stride uint32
	w, h   int
	mapped []byte
	dev    *os.File // keeps the DRM fd alive for the lifetime of the device
}

// newGBMBuffer creates a linear XRGB8888 GBM buffer on the given DRM device
// path, exports it as a dma-buf and maps it read/write.
func newGBMBuffer(devPath string, w, h int) (*gbmBO, error) {
	loadGBM()
	if gbmLib == 0 {
		return nil, fmt.Errorf("libgbm (%s) unavailable", gbmLibName)
	}
	f, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	devH := gbmCreateDevice(int(f.Fd()))
	if devH == 0 {
		f.Close()
		return nil, fmt.Errorf("gbm_create_device failed")
	}
	bo := gbmBoCreate(devH, uint32(w), uint32(h), gbmFormatXRGB8888,
		gbmBoUseRendering|gbmBoUseLinear)
	if bo == 0 {
		gbmDeviceDestroy(devH)
		f.Close()
		return nil, fmt.Errorf("gbm_bo_create failed")
	}
	stride := gbmBoGetStride(bo)
	dmabuf := gbmBoGetFD(bo)
	if dmabuf < 0 {
		gbmBoDestroy(bo)
		gbmDeviceDestroy(devH)
		f.Close()
		return nil, fmt.Errorf("gbm_bo_get_fd failed")
	}
	mapped, err := mmapRW(dmabuf, int(stride)*h)
	if err != nil {
		closeFD(dmabuf)
		gbmBoDestroy(bo)
		gbmDeviceDestroy(devH)
		f.Close()
		return nil, fmt.Errorf("mmap gbm bo: %w", err)
	}
	return &gbmBO{
		boH:    bo,
		devH:   devH,
		fd:     dmabuf,
		stride: stride,
		w:      w,
		h:      h,
		mapped: mapped,
		dev:    f,
	}, nil
}

// Pixels returns the mapped buffer (XRGB8888 layout: B,G,R,X per pixel).
func (b *gbmBO) Pixels() []byte { return b.mapped }

// Stride returns the row stride in bytes.
func (b *gbmBO) Stride() int { return int(b.stride) }

func (b *gbmBO) Close() {
	if b.mapped != nil {
		munmap(b.mapped)
	}
	if b.fd >= 0 {
		closeFD(b.fd)
	}
	if b.boH != 0 {
		gbmBoDestroy(b.boH)
	}
	if b.devH != 0 {
		gbmDeviceDestroy(b.devH)
	}
	if b.dev != nil {
		b.dev.Close()
	}
}
