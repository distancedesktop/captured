//go:build linux

package pipelines

import (
	"context"
	"os"
	"strings"
	"testing"
	"unsafe"
)

func TestX11ProbeUnavailable(t *testing.T) {
	for _, tc := range []struct{ display, reason string }{{"", "DISPLAY is unset"}, {":65530", "cannot open X11 display"}} {
		t.Run(tc.reason, func(t *testing.T) {
			t.Setenv("DISPLAY", tc.display)
			if err := (x11XShm{}).Probe(context.Background()); err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("Probe = %v, want %q", err, tc.reason)
			}
		})
	}
}

func TestCopyXImage(t *testing.T) {
	src := []byte{1, 2, 3, 0, 4, 5, 6, 0, 99, 99, 99, 99, 7, 8, 9, 0, 10, 11, 12, 0, 99, 99, 99, 99}
	im := xImage{Width: 2, Height: 2, Data: unsafe.Pointer(&src[0]), BytesPerLine: 12, BitsPerPixel: 32, RedMask: 0xff0000, GreenMask: 0xff00, BlueMask: 0xff}
	dst := make([]byte, 16)
	if err := copyXImage(dst, &im, 2, 2); err != nil {
		t.Fatal(err)
	}
	want := []byte{1, 2, 3, 255, 4, 5, 6, 255, 7, 8, 9, 255, 10, 11, 12, 255}
	if string(dst) != string(want) {
		t.Fatalf("pixels = %v", dst)
	}
	if err := copyXImage(nil, &im, 2, 2); err == nil {
		t.Fatal("accepted empty destination")
	}
	im.BitsPerPixel = 16
	if err := copyXImage(dst, &im, 2, 2); err == nil {
		t.Fatal("accepted unsupported format")
	}
	im.Data = nil
	if err := copyXImage(dst, &im, 2, 2); err == nil {
		t.Fatal("accepted empty image")
	}
}

func TestX11Capture(t *testing.T) {
	if os.Getenv("DISPLAY") == "" {
		t.Skip("needs an X11 display")
	}
	g, err := openX11()
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	t.Logf("MIT-SHM attached: %v", g.attached)
	dst := make([]byte, g.w*g.h*4)
	if err := g.grab(dst); err != nil {
		t.Fatal(err)
	}
	g.dropImage()
	if err := g.grab(dst); err != nil {
		t.Fatalf("XGetImage fallback: %v", err)
	}
	if g.attached {
		t.Fatal("fallback unexpectedly attached SHM")
	}
	if _, err := (x11XShm{}).Start(context.Background(), 1, Config{}); err == nil {
		t.Fatal("accepted unknown display")
	}
}
