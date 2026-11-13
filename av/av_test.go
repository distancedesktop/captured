package av

import "testing"

func TestCPUEncode(t *testing.T) {
	cands := Candidates("h264")
	name := ""
	for _, c := range cands {
		if Family(c) == "libx264" || c == "libopenh264" {
			name = c
		}
	}
	if name == "" {
		t.Skip("no software h264 encoder", cands)
	}
	for _, size := range [][2]int{{640, 360}, {320, 180}} {
		e, err := Open(Config{Encoder: name, SrcW: 640, SrcH: 360, W: size[0], H: size[1], FPS: 60, Bitrate: 2e6})
		if err != nil {
			t.Fatal(err)
		}
		n, keys := 0, 0
		for i := 0; i < 30; i++ {
			f, err := e.Frame()
			if err != nil {
				t.Fatal(err)
			}
			b := f.Bytes()
			for j := range b {
				b[j] = byte(i + j)
			}
			if i == 20 {
				e.Keyframe()
			}
			if i == 10 {
				e.SetBitrate(1e6)
			}
			err = e.Encode(f, int64(i)*16666, func(p Packet) error {
				n++
				if p.Key {
					keys++
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		e.Close()
		if n != 30 || keys != 2 {
			t.Fatalf("%v: got %d packets, %d keys", size, n, keys)
		}
	}
}
