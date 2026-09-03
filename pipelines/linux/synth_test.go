//go:build linux

package linux

import "testing"

func TestSynthCaptureProducesBGRA(t *testing.T) {
	const w, h = 64, 48
	g, err := newSynthCapture(w, h)
	if err != nil {
		t.Fatalf("newSynthCapture: %v", err)
	}
	defer g.close()

	bgra, gw, gh, err := g.grab()
	if err != nil {
		t.Fatalf("grab: %v", err)
	}
	if gw != w || gh != h {
		t.Fatalf("size: got %dx%d want %dx%d", gw, gh, w, h)
	}
	if len(bgra) != w*h*4 {
		t.Fatalf("len: got %d want %d", len(bgra), w*h*4)
	}
	// Alpha must be opaque for the pure-Go BGRA path.
	if bgra[3] != 0xFF {
		t.Fatalf("alpha: got %d want 255", bgra[3])
	}
}

func TestBGRAConverterFormats(t *testing.T) {
	for _, fmtCode := range []uint32{drmFormatXRGB8888, drmFormatARGB8888, drmFormatRGBX8888, drmFormatBGRX8888} {
		if _, ok := bgraConverter(fmtCode); !ok {
			t.Fatalf("bgraConverter(0x%x) unsupported", fmtCode)
		}
	}
	if _, ok := bgraConverter(0x12345678); ok {
		t.Fatalf("bgraConverter accepted unknown format")
	}
}
