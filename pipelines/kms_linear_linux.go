//go:build linux

package pipelines

import (
	"context"
	"fmt"
	"os"
	"syscall"
)

// kms_linear: map the scanout dma-buf and copy it out on the CPU. Only works
// when the framebuffer is linear (virtio-gpu, simpledrm, some iGPUs). Tiled
// scanouts are refused instead of faked.

type kmsLinear struct{}

func init() { Register(kmsLinear{}, 10) }

func (kmsLinear) Name() string { return "kms_linear" }

func notNvidia(driver string) bool { return driver != "nvidia-drm" }

func (kmsLinear) Probe(ctx context.Context) error {
	outs, err := scanKMS(notNvidia)
	if err != nil {
		return err
	}
	if len(outs) == 0 {
		return fmt.Errorf("no lit non-NVIDIA KMS outputs")
	}
	return probeFBHandles(outs[0])
}

func (kmsLinear) ListDisplays(ctx context.Context) ([]Display, error) {
	outs, err := scanKMS(notNvidia)
	if err != nil {
		return nil, err
	}
	return kmsDisplays(outs), nil
}

func (kmsLinear) Start(ctx context.Context, displayID uint32, fps int) (Stream, error) {
	outs, err := scanKMS(notNvidia)
	if err != nil {
		return nil, err
	}
	i, err := pickDisplay(kmsDisplays(outs), displayID)
	if err != nil {
		return nil, err
	}
	o := outs[i]
	return startStream(ctx, func() (grabber, error) { return openKMSLinear(o) }, fps, true)
}

type linearFB struct {
	ino     uint64
	mapping []byte
	mem     []byte
	pitch   int
	opaque  bool // XRGB: the X byte is undefined, force alpha to 0xff
}

type kmsLinearGrabber struct {
	f    *os.File
	fd   int
	crtc uint32
	w, h int
	fbs  map[uint32]*linearFB
}

func openKMSLinear(o kmsOutput) (grabber, error) {
	f, err := os.OpenFile(o.Card, os.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	g := &kmsLinearGrabber{f: f, fd: int(f.Fd()), crtc: o.CrtcID, w: o.Width, h: o.Height, fbs: map[uint32]*linearFB{}}
	id, err := currentFB(g.fd, g.crtc)
	if err == nil {
		_, err = g.fb(id)
	}
	if err != nil {
		g.close()
		return nil, fmt.Errorf("kms_linear %s: %w", o.Name, err)
	}
	return g, nil
}

func (g *kmsLinearGrabber) fb(id uint32) (*linearFB, error) {
	ino, err := fbIdentity(g.fd, id)
	if err != nil {
		return nil, err
	}
	if m, ok := g.fbs[id]; ok {
		if m.ino == ino {
			return m, nil
		}
		syscall.Munmap(m.mapping)
		delete(g.fbs, id)
	}
	fb, err := getFB2(g.fd, id)
	if err != nil {
		return nil, err
	}
	defer closeFBHandles(g.fd, fb)
	switch {
	case fb.Flags&drmFbModifiers != 0 && fb.Modifier != drmFormatModLinear:
		return nil, fmt.Errorf("scanout is tiled (modifier 0x%x); use kms_nvidia or pipewire_mutter", fb.Modifier)
	case fb.PixelFormat != drmFormatXRGB8888 && fb.PixelFormat != drmFormatARGB8888:
		return nil, fmt.Errorf("unsupported scanout format 0x%x", fb.PixelFormat)
	case int(fb.Width) != g.w || int(fb.Height) != g.h:
		return nil, fmt.Errorf("framebuffer is %dx%d, mode is %dx%d", fb.Width, fb.Height, g.w, g.h)
	case fb.Handles[0] == 0:
		return nil, fmt.Errorf("%s", errNeedsCapSysAdmin)
	}
	dmabuf, err := primeFD(g.fd, fb.Handles[0])
	if err != nil {
		return nil, err
	}
	defer syscall.Close(dmabuf)
	pitch := int(fb.Pitches[0])
	off := int(fb.Offsets[0])
	mapping, err := syscall.Mmap(dmabuf, 0, off+pitch*g.h, syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("mmap scanout: %w", err)
	}
	if len(g.fbs) >= 4 {
		g.dropFBs()
	}
	m := &linearFB{ino: ino, mapping: mapping, mem: mapping[off:], pitch: pitch, opaque: fb.PixelFormat == drmFormatXRGB8888}
	g.fbs[id] = m
	return m, nil
}

func (g *kmsLinearGrabber) dropFBs() {
	for id, m := range g.fbs {
		syscall.Munmap(m.mapping)
		delete(g.fbs, id)
	}
}

func (g *kmsLinearGrabber) size() (int, int) { return g.w, g.h }

// XRGB8888 is B,G,R,X in memory, which already is BGRA byte order.
func (g *kmsLinearGrabber) grab(dst []byte) error {
	m, err := scanoutFB(g.fd, g.crtc, g.fb)
	if err != nil {
		return err
	}
	row := g.w * 4
	if m.pitch == row {
		copy(dst, m.mem[:row*g.h])
	} else {
		for y := 0; y < g.h; y++ {
			copy(dst[y*row:(y+1)*row], m.mem[y*m.pitch:y*m.pitch+row])
		}
	}
	if m.opaque {
		setOpaque(dst)
	}
	return nil
}

func setOpaque(px []byte) {
	for i := 3; i < len(px); i += 4 {
		px[i] = 0xff
	}
}

func (g *kmsLinearGrabber) close() error {
	g.dropFBs()
	return g.f.Close()
}
