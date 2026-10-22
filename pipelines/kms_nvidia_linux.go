//go:build linux

package pipelines

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// kms_nvidia: grab the scanout framebuffer straight from the NVIDIA driver.
// NVIDIA scanouts are block-linear (tiled), so the CPU can't read them; the
// dma-buf is imported into EGL with its modifier, bound to an FBO and read back
// with glReadPixels, which de-tiles on the GPU. This is the same route
// Sunshine's KMS capture takes.
//
// Needs CAP_SYS_ADMIN for the framebuffer handles. The hardware cursor lives on
// its own plane and is not in the image.

type kmsNvidia struct{}

func init() { Register(kmsNvidia{}, 30) }

func (kmsNvidia) Name() string { return "kms_nvidia" }

func isNvidia(driver string) bool { return driver == "nvidia-drm" }

func (kmsNvidia) Probe(ctx context.Context) error {
	if err := loadEGL(); err != nil {
		return err
	}
	outs, err := scanKMS(isNvidia)
	if err != nil {
		return err
	}
	if len(outs) == 0 {
		return fmt.Errorf("no lit outputs on an nvidia-drm device")
	}
	return probeFBHandles(outs[0])
}

func (kmsNvidia) ListDisplays(ctx context.Context) ([]Display, error) {
	outs, err := scanKMS(isNvidia)
	if err != nil {
		return nil, err
	}
	return kmsDisplays(outs), nil
}

func (kmsNvidia) Start(ctx context.Context, displayID uint32, fps int) (Stream, error) {
	outs, err := scanKMS(isNvidia)
	if err != nil {
		return nil, err
	}
	i, err := pickDisplay(kmsDisplays(outs), displayID)
	if err != nil {
		return nil, err
	}
	o := outs[i]
	return startStream(ctx, func() (grabber, error) { return openKMSNvidia(o) }, fps, true)
}

type gpuFB struct {
	ino uint64
	img uintptr
	tex uint32
	fbo uint32
}

type kmsNvidiaGrabber struct {
	card   *os.File
	render *os.File
	fd     int
	crtc   uint32
	w, h   int
	gpu    *eglGPU
	fbs    map[uint32]*gpuFB
	format uint32
}

func openKMSNvidia(o kmsOutput) (grabber, error) {
	card, err := os.OpenFile(o.Card, os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	g := &kmsNvidiaGrabber{card: card, fd: int(card.Fd()), crtc: o.CrtcID, w: o.Width, h: o.Height, fbs: map[uint32]*gpuFB{}, format: glBGRAExt}
	fail := func(err error) (grabber, error) {
		g.close()
		return nil, fmt.Errorf("kms_nvidia %s: %w", o.Name, err)
	}
	gpuFD := g.fd
	if node := drmGetRenderDeviceNameFromFd(g.fd); node != "" {
		if g.render, err = os.OpenFile(node, os.O_RDWR|syscall.O_CLOEXEC, 0); err == nil {
			gpuFD = int(g.render.Fd())
		}
	}
	if g.gpu, err = newEGLGPU(gpuFD); err != nil {
		return fail(err)
	}
	glPixelStorei(glPackAlignment, 4)
	id, err := currentFB(g.fd, g.crtc)
	if err != nil {
		return fail(err)
	}
	if _, err := g.fb(id); err != nil {
		return fail(err)
	}
	if err := g.grab(make([]byte, g.w*g.h*4)); err != nil {
		return fail(err)
	}
	return g, nil
}

func (g *kmsNvidiaGrabber) fb(id uint32) (*gpuFB, error) {
	ino, err := fbIdentity(g.fd, id)
	if err != nil {
		return nil, err
	}
	if f, ok := g.fbs[id]; ok {
		if f.ino == ino {
			return f, nil
		}
		g.freeFB(f)
		delete(g.fbs, id)
	}
	fb, err := getFB2(g.fd, id)
	if err != nil {
		return nil, err
	}
	defer closeFBHandles(g.fd, fb)
	if fb.Handles[0] == 0 {
		return nil, fmt.Errorf("%s", errNeedsCapSysAdmin)
	}
	if int(fb.Width) != g.w || int(fb.Height) != g.h {
		return nil, fmt.Errorf("framebuffer is %dx%d, mode is %dx%d", fb.Width, fb.Height, g.w, g.h)
	}
	fds := [4]int{-1, -1, -1, -1}
	defer func() {
		for _, fd := range fds {
			if fd >= 0 {
				syscall.Close(fd)
			}
		}
	}()
	for i, h := range fb.Handles {
		if h == 0 {
			continue
		}
		if fds[i], err = primeFD(g.fd, h); err != nil {
			return nil, err
		}
	}
	img, err := g.gpu.importFB(fb, fds)
	if err != nil {
		return nil, err
	}
	f := &gpuFB{img: img, ino: ino}
	glGenTextures(1, &f.tex)
	glBindTexture(glTexture2D, f.tex)
	glTexParameteri(glTexture2D, glTextureMinFilter, glNearest)
	glTexParameteri(glTexture2D, glTextureMagFilter, glNearest)
	glEGLImageTargetTexture2DOES(glTexture2D, img)
	glGenFramebuffers(1, &f.fbo)
	glBindFramebuffer(glFramebuffer, f.fbo)
	glFramebufferTexture2D(glFramebuffer, glColorAttachment0, glTexture2D, f.tex, 0)
	if st := glCheckFramebufferStatus(glFramebuffer); st != glFramebufferComplete {
		g.freeFB(f)
		return nil, fmt.Errorf("framebuffer incomplete (0x%x) for fourcc 0x%x modifier 0x%x", st, fb.PixelFormat, fb.Modifier)
	}
	// Compositors flip between 2-3 buffers; anything beyond that is churn.
	if len(g.fbs) >= 4 {
		for k, old := range g.fbs {
			g.freeFB(old)
			delete(g.fbs, k)
		}
	}
	g.fbs[id] = f
	return f, nil
}

func (g *kmsNvidiaGrabber) freeFB(f *gpuFB) {
	if f.fbo != 0 {
		glDeleteFramebuffers(1, &f.fbo)
	}
	if f.tex != 0 {
		glDeleteTextures(1, &f.tex)
	}
	if f.img != 0 {
		eglDestroyImageKHR(g.gpu.dpy, f.img)
	}
}

func (g *kmsNvidiaGrabber) size() (int, int) { return g.w, g.h }

func (g *kmsNvidiaGrabber) grab(dst []byte) error {
	id, err := currentFB(g.fd, g.crtc)
	if err != nil {
		return err
	}
	f, err := g.fb(id)
	if err != nil {
		return err
	}
	glBindFramebuffer(glFramebuffer, f.fbo)
	glReadPixels(0, 0, int32(g.w), int32(g.h), g.format, glUnsignedByte, unsafe.Pointer(&dst[0]))
	if e := glGetError(); e != 0 {
		if g.format == glBGRAExt {
			// No EXT_read_format_bgra: read RGBA and swap on the CPU.
			g.format = glRGBA
			return g.grab(dst)
		}
		return fmt.Errorf("glReadPixels failed: 0x%x", e)
	}
	if g.format == glRGBA {
		for i := 0; i < len(dst); i += 4 {
			dst[i], dst[i+2] = dst[i+2], dst[i]
		}
	}
	return nil
}

func (g *kmsNvidiaGrabber) close() error {
	if g.gpu != nil {
		for k, f := range g.fbs {
			g.freeFB(f)
			delete(g.fbs, k)
		}
		g.gpu.destroy()
	}
	if g.render != nil {
		g.render.Close()
	}
	return g.card.Close()
}
