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
