// capbench: drives a running captured over its sockets and measures what comes out.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

func cpuTicks(pid int) int64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return -1
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndex(s, ")")+2:])
	u, _ := strconv.ParseInt(f[11], 10, 64)
	k, _ := strconv.ParseInt(f[12], 10, 64)
	return u + k
}

func pct(d []float64, p float64) float64 {
	if len(d) == 0 {
		return 0
	}
	i := int(float64(len(d)-1) * p)
	return d[i]
}

func main() {
	display := flag.Uint("display", 0, "display id")
	fps := flag.Int("fps", 60, "requested fps")
	secs := flag.Float64("secs", 5, "seconds to read")
	pid := flag.Int("pid", 0, "captured pid for cpu accounting")
	dump := flag.String("dump", "", "write the last frame as raw BGRA here")
	flag.Parse()

	c, err := net.Dial("unix", "/tmp/captured.socket")
	if err != nil {
		fmt.Println("control dial:", err)
		os.Exit(1)
	}
	r := bufio.NewReader(c)
	call := func(req map[string]any) string {
		b, _ := json.Marshal(req)
		c.Write(append(b, '\n'))
		line, _ := r.ReadString('\n')
		return strings.TrimSpace(line)
	}
	fmt.Println("info:", call(map[string]any{"type": "info"}))
	fmt.Println("displays:", call(map[string]any{"type": "list-displays"}))
	t0 := time.Now()
	fmt.Println("start:", call(map[string]any{"type": "start-stream", "display_id": *display, "fps": *fps}), "in", time.Since(t0))

	var m net.Conn
	for i := 0; i < 50; i++ {
		if m, err = net.Dial("unix", "/tmp/captured-media.socket"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if m == nil {
		fmt.Println("media dial:", err)
		os.Exit(1)
	}
	mr := bufio.NewReaderSize(m, 1<<22)

	cpu0 := cpuTicks(*pid)
	var gaps []float64
	var last time.Time
	var first time.Duration = -1
	frames, dups := 0, 0
	var lastHash uint64
	var bytes int64
	var w, h uint32
	var buf []byte
	hdr := make([]byte, 8)
	start := time.Now()
	for time.Since(start).Seconds() < *secs {
		if _, err := io.ReadFull(mr, hdr); err != nil {
			fmt.Println("media read:", err)
			break
		}
		w, h = binary.BigEndian.Uint32(hdr[0:4]), binary.BigEndian.Uint32(hdr[4:8])
		n := int(w) * int(h) * 4
		if cap(buf) < n {
			buf = make([]byte, n)
		}
		buf = buf[:n]
		if _, err := io.ReadFull(mr, buf); err != nil {
			fmt.Println("media read:", err)
			break
		}
		now := time.Now()
		if first < 0 {
			first = now.Sub(t0)
		} else {
			gaps = append(gaps, float64(now.Sub(last).Microseconds())/1000)
		}
		last = now
		hs := fnv.New64a()
		for i := 0; i < n; i += 4093 {
			hs.Write(buf[i : i+1])
		}
		if v := hs.Sum64(); v == lastHash {
			dups++
		} else {
			lastHash = v
		}
		frames++
		bytes += int64(n)
	}
	el := time.Since(start).Seconds()
	cpu1 := cpuTicks(*pid)
	fmt.Println("stats:", call(map[string]any{"type": "stats"}))
	call(map[string]any{"type": "stop-stream"})
	sort.Float64s(gaps)
	fmt.Printf("frames=%d in %.2fs -> %.1f fps, %dx%d, %.1f MB/s, first frame after %v\n", frames, el, float64(frames)/el, w, h, float64(bytes)/el/1e6, first)
	fmt.Printf("gap ms p50=%.2f p95=%.2f p99=%.2f max=%.2f; identical-to-previous=%d\n", pct(gaps, .5), pct(gaps, .95), pct(gaps, .99), pct(gaps, 1), dups)
	if cpu0 >= 0 && cpu1 >= 0 {
		fmt.Printf("captured cpu: %.0f%% of one core\n", float64(cpu1-cpu0)/100/el*100)
	}
	if *dump != "" && len(buf) > 0 {
		os.WriteFile(*dump, buf, 0o644)
		fmt.Println("dumped", *dump)
	}
}
