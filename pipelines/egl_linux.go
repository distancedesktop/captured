//go:build linux

package pipelines

import (
	"fmt"
	"strings"
	"sync"
	"unsafe"

	"github.com/ebitengine/purego"
)

// EGL + GLES + GBM, loaded at runtime with purego.

const (
	eglNone                = 0x3038
	eglExtensions          = 0x3055
	eglWidth               = 0x3057
	eglHeight              = 0x3056
	eglOpenGLESAPI         = 0x30A0
	eglContextMajorVersion = 0x3098
	eglPlatformGBM         = 0x31D7
	eglLinuxDmaBuf         = 0x3270
	eglLinuxDrmFourcc      = 0x3271
	eglImagePreserved      = 0x30D2
	glTexture2D            = 0x0DE1
	glFramebuffer          = 0x8D40
	glColorAttachment0     = 0x8CE0
	glFramebufferComplete  = 0x8CD5
	glBGRAExt              = 0x80E1
	glRGBA                 = 0x1908
	glUnsignedByte         = 0x1401
	glTextureMinFilter     = 0x2801
	glTextureMagFilter     = 0x2800
	glNearest              = 0x2600
	glPackAlignment        = 0x0D05
)

// Per-plane dma-buf attributes: fd, offset, pitch, modifier lo, modifier hi.
var eglPlaneAttrs = [4][5]int32{
	{0x3272, 0x3273, 0x3274, 0x3443, 0x3444},
	{0x3275, 0x3276, 0x3277, 0x3445, 0x3446},
	{0x3278, 0x3279, 0x327A, 0x3447, 0x3448},
	{0x3440, 0x3441, 0x3442, 0x3449, 0x344A},
}

var (
	eglOnce sync.Once
	eglErr  error

	gbmCreateDevice  func(fd int) uintptr
	gbmDeviceDestroy func(dev uintptr)

	eglGetProcAddress        func(name string) uintptr
	eglGetPlatformDisplayEXT func(platform uint32, native uintptr, attrs unsafe.Pointer) uintptr
	eglInitialize            func(dpy uintptr, major, minor *int32) uint32
	eglTerminate             func(dpy uintptr) uint32
	eglQueryString           func(dpy uintptr, name int32) string
	eglBindAPI               func(api uint32) uint32
	eglCreateContext         func(dpy, config, share uintptr, attrs *int32) uintptr
	eglDestroyContext        func(dpy, ctx uintptr) uint32
	eglMakeCurrent           func(dpy, draw, read, ctx uintptr) uint32
	eglGetError              func() int32
	eglCreateImageKHR        func(dpy, ctx uintptr, target uint32, buf uintptr, attrs *int32) uintptr
	eglDestroyImageKHR       func(dpy, img uintptr) uint32

	glEGLImageTargetTexture2DOES func(target uint32, img uintptr)
	glGenTextures                func(n int32, ids *uint32)
	glDeleteTextures             func(n int32, ids *uint32)
	glBindTexture                func(target, id uint32)
	glTexParameteri              func(target, name uint32, v int32)
	glGenFramebuffers            func(n int32, ids *uint32)
	glDeleteFramebuffers         func(n int32, ids *uint32)
	glBindFramebuffer            func(target, id uint32)
	glFramebufferTexture2D       func(target, attach, textarget, tex uint32, level int32)
	glCheckFramebufferStatus     func(target uint32) uint32
	glPixelStorei                func(name uint32, v int32)
	glReadPixels                 func(x, y, w, h int32, format, typ uint32, px unsafe.Pointer)
	glGetError                   func() uint32
)

func loadEGL() error {
	eglOnce.Do(func() {
		gbm, err := purego.Dlopen("libgbm.so.1", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			eglErr = fmt.Errorf("libgbm.so.1: %w", err)
			return
		}
		egl, err := purego.Dlopen("libEGL.so.1", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			eglErr = fmt.Errorf("libEGL.so.1: %w", err)
			return
		}
		gles, err := purego.Dlopen("libGLESv2.so.2", purego.RTLD_NOW|purego.RTLD_GLOBAL)
		if err != nil {
			eglErr = fmt.Errorf("libGLESv2.so.2: %w", err)
			return
		}
		purego.RegisterLibFunc(&gbmCreateDevice, gbm, "gbm_create_device")
		purego.RegisterLibFunc(&gbmDeviceDestroy, gbm, "gbm_device_destroy")

		purego.RegisterLibFunc(&eglGetProcAddress, egl, "eglGetProcAddress")
		purego.RegisterLibFunc(&eglInitialize, egl, "eglInitialize")
		purego.RegisterLibFunc(&eglTerminate, egl, "eglTerminate")
		purego.RegisterLibFunc(&eglQueryString, egl, "eglQueryString")
		purego.RegisterLibFunc(&eglBindAPI, egl, "eglBindAPI")
		purego.RegisterLibFunc(&eglCreateContext, egl, "eglCreateContext")
		purego.RegisterLibFunc(&eglDestroyContext, egl, "eglDestroyContext")
		purego.RegisterLibFunc(&eglMakeCurrent, egl, "eglMakeCurrent")
		purego.RegisterLibFunc(&eglGetError, egl, "eglGetError")

		ext := map[string]any{
			"eglGetPlatformDisplayEXT":     &eglGetPlatformDisplayEXT,
			"eglCreateImageKHR":            &eglCreateImageKHR,
			"eglDestroyImageKHR":           &eglDestroyImageKHR,
			"glEGLImageTargetTexture2DOES": &glEGLImageTargetTexture2DOES,
		}
		for name, fn := range ext {
			addr := eglGetProcAddress(name)
			if addr == 0 {
				eglErr = fmt.Errorf("EGL is missing %s", name)
				return
			}
			purego.RegisterFunc(fn, addr)
		}

		purego.RegisterLibFunc(&glGenTextures, gles, "glGenTextures")
		purego.RegisterLibFunc(&glDeleteTextures, gles, "glDeleteTextures")
		purego.RegisterLibFunc(&glBindTexture, gles, "glBindTexture")
		purego.RegisterLibFunc(&glTexParameteri, gles, "glTexParameteri")
		purego.RegisterLibFunc(&glGenFramebuffers, gles, "glGenFramebuffers")
		purego.RegisterLibFunc(&glDeleteFramebuffers, gles, "glDeleteFramebuffers")
		purego.RegisterLibFunc(&glBindFramebuffer, gles, "glBindFramebuffer")
		purego.RegisterLibFunc(&glFramebufferTexture2D, gles, "glFramebufferTexture2D")
		purego.RegisterLibFunc(&glCheckFramebufferStatus, gles, "glCheckFramebufferStatus")
		purego.RegisterLibFunc(&glPixelStorei, gles, "glPixelStorei")
		purego.RegisterLibFunc(&glReadPixels, gles, "glReadPixels")
		purego.RegisterLibFunc(&glGetError, gles, "glGetError")
	})
	return eglErr
}

// eglGPU is a surfaceless GLES 3 context on one DRM device, made current on
// the calling (locked) thread.
type eglGPU struct {
	gbm uintptr
	dpy uintptr
	ctx uintptr
}

func newEGLGPU(renderFD int) (*eglGPU, error) {
	if err := loadEGL(); err != nil {
		return nil, err
	}
	g := &eglGPU{}
	if g.gbm = gbmCreateDevice(renderFD); g.gbm == 0 {
		return nil, fmt.Errorf("gbm_create_device failed")
	}
	if g.dpy = eglGetPlatformDisplayEXT(eglPlatformGBM, g.gbm, nil); g.dpy == 0 {
		g.destroy()
		return nil, fmt.Errorf("eglGetPlatformDisplay(GBM) failed: 0x%x", eglGetError())
	}
	var maj, min int32
	if eglInitialize(g.dpy, &maj, &min) == 0 {
		g.dpy = 0
		g.destroy()
		return nil, fmt.Errorf("eglInitialize failed: 0x%x", eglGetError())
	}
	exts := eglQueryString(g.dpy, eglExtensions)
	for _, want := range []string{"EGL_EXT_image_dma_buf_import_modifiers", "EGL_KHR_surfaceless_context", "EGL_KHR_no_config_context"} {
		if !strings.Contains(exts, want) {
			g.destroy()
			return nil, fmt.Errorf("EGL display lacks %s", want)
		}
	}
	eglBindAPI(eglOpenGLESAPI)
	attrs := []int32{eglContextMajorVersion, 3, eglNone}
	if g.ctx = eglCreateContext(g.dpy, 0, 0, &attrs[0]); g.ctx == 0 {
		g.destroy()
		return nil, fmt.Errorf("eglCreateContext failed: 0x%x", eglGetError())
	}
	if eglMakeCurrent(g.dpy, 0, 0, g.ctx) == 0 {
		g.destroy()
		return nil, fmt.Errorf("eglMakeCurrent failed: 0x%x", eglGetError())
	}
	return g, nil
}

// importFB wraps a scanout framebuffer as an EGLImage. The dma-buf fds can be
// closed afterwards; the image holds its own reference.
func (g *eglGPU) importFB(fb drmModeFB2, fds [4]int) (uintptr, error) {
	attrs := []int32{
		eglWidth, int32(fb.Width),
		eglHeight, int32(fb.Height),
		eglLinuxDrmFourcc, int32(fb.PixelFormat),
		eglImagePreserved, 1,
	}
	for i := 0; i < 4; i++ {
		if fds[i] < 0 {
			continue
		}
		a := eglPlaneAttrs[i]
		attrs = append(attrs, a[0], int32(fds[i]), a[1], int32(fb.Offsets[i]), a[2], int32(fb.Pitches[i]))
		if fb.Flags&drmFbModifiers != 0 {
			attrs = append(attrs, a[3], int32(uint32(fb.Modifier)), a[4], int32(uint32(fb.Modifier>>32)))
		}
	}
	attrs = append(attrs, eglNone)
	img := eglCreateImageKHR(g.dpy, 0, eglLinuxDmaBuf, 0, &attrs[0])
	if img == 0 {
		return 0, fmt.Errorf("eglCreateImage(dma-buf, fourcc 0x%x, modifier 0x%x) failed: 0x%x", fb.PixelFormat, fb.Modifier, eglGetError())
	}
	return img, nil
}

func (g *eglGPU) destroy() {
	if g.dpy != 0 {
		eglMakeCurrent(g.dpy, 0, 0, 0)
		if g.ctx != 0 {
			eglDestroyContext(g.dpy, g.ctx)
		}
		eglTerminate(g.dpy)
	}
	if g.gbm != 0 {
		gbmDeviceDestroy(g.gbm)
	}
}
