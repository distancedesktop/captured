package pipelines

import (
	"strings"
	"testing"
)

func TestVAAPIDeviceCandidatesConfigured(t *testing.T) {
	got := vaapiDeviceCandidates("/dev/dri/renderD128")
	if len(got) != 1 || got[0] != "/dev/dri/renderD128" {
		t.Fatalf("got %v", got)
	}
}

func TestVAAPIDeviceCandidatesDefault(t *testing.T) {
	got := vaapiDeviceCandidates("")
	if len(got) == 0 || got[len(got)-1] != "" {
		t.Fatalf("default candidate must be last: %v", got)
	}
	for _, device := range got[:len(got)-1] {
		if !strings.HasPrefix(device, "/dev/dri/renderD") {
			t.Fatalf("unexpected device %q in %v", device, got)
		}
	}
}

func TestDeviceLabel(t *testing.T) {
	if deviceLabel("") != "default" || deviceLabel("/dev/dri/renderD128") != "/dev/dri/renderD128" {
		t.Fatal("bad device label")
	}
}

func TestWithEncoderDeviceNVENC(t *testing.T) {
	base := map[string]string{"preset": "p1"}
	got := withEncoderDevice(base, "h264_nvenc", "1")
	if got["gpu"] != "1" || got["preset"] != "p1" {
		t.Fatalf("got %v", got)
	}
	if _, ok := base["gpu"]; ok {
		t.Fatal("base options mutated")
	}
}

func TestWithEncoderDeviceExistingOptionWins(t *testing.T) {
	got := withEncoderDevice(map[string]string{"gpu": "0"}, "hevc_nvenc", "1")
	if got["gpu"] != "0" {
		t.Fatalf("got %v", got)
	}
}

func TestWithEncoderDeviceOtherFamilies(t *testing.T) {
	for _, name := range []string{"h264_vaapi", "libx264", "h264_qsv"} {
		got := withEncoderDevice(nil, name, "1")
		if len(got) != 0 {
			t.Fatalf("%s got %v", name, got)
		}
	}
}
