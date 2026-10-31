// capbench spawns captured the way the agent does, streams for a while and
// reports what came out: fps, frame gaps, bitrate, keyframes, encode stats
// and captured's CPU use.
//
//	capbench -captured ./captured -duration 10s -set video.fps=60 -set video.encoder=h264_nvenc -o out.h264
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

type kv []string

func (k *kv) String() string     { return strings.Join(*k, ",") }
func (k *kv) Set(v string) error { *k = append(*k, v); return nil }

var (
	bin       = flag.String("captured", "./captured", "captured binary")
	duration  = flag.Duration("duration", 10*time.Second, "how long to stream")
	out       = flag.String("o", "", "write the raw bitstream here")
	bitrateAt = flag.String("bitrate-at", "", "change video.bitrate mid run: <time>=<bps>, e.g. 5s=2000000")
	sets      kv
)

type rpc struct {
	mu   sync.Mutex
	in   io.Writer
	id   int
	wait map[int]chan json.RawMessage
}

func (r *rpc) call(method string, params any) (json.RawMessage, error) {
	r.mu.Lock()
	r.id++
	id := r.id
	ch := make(chan json.RawMessage, 1)
	r.wait[id] = ch
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	_, err := r.in.Write(append(b, '\n'))
	r.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var m struct {
		Result json.RawMessage
		Error  *struct{ Message string }
	}
	if err := json.Unmarshal(<-ch, &m); err != nil {
		return nil, err
	}
	if m.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, m.Error.Message)
	}
	return m.Result, nil
}

func (r *rpc) read(rd io.Reader) {
	sc := bufio.NewScanner(rd)
	sc.Buffer(nil, 4<<20)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var m struct {
			ID     *int
			Method string
			Params json.RawMessage
		}
		json.Unmarshal(line, &m)
		if m.ID == nil {
			log.Printf("notification %s %s", m.Method, m.Params)
			continue
		}
		r.mu.Lock()
		ch := r.wait[*m.ID]
		delete(r.wait, *m.ID)
		r.mu.Unlock()
		if ch != nil {
			ch <- line
		}
	}
}

func main() {
	flag.Var(&sets, "set", "property for start, key=value (value is JSON, bare strings work too); repeatable")
	flag.Parse()

	params := map[string]json.RawMessage{}
	for _, s := range sets {
		k, v, _ := strings.Cut(s, "=")
		if !json.Valid([]byte(v)) {
			v = fmt.Sprintf("%q", v)
		}
		params[k] = json.RawMessage(v)
	}

	mr, mw, err := os.Pipe()
	if err != nil {
		log.Fatal(err)
	}
	cmd := exec.Command(*bin)
	cmd.Stderr = os.Stderr
	cmd.ExtraFiles = []*os.File{mw}
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}
	mw.Close()
	r := &rpc{in: stdin, wait: map[int]chan json.RawMessage{}}
	go r.read(stdout)

	var dump io.Writer = io.Discard
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			log.Fatal(err)
		}
		defer f.Close()
		dump = f
	}

	var mu sync.Mutex
	var arrivals []time.Time
	var bytes int64
	var keys int
	var formats []string
	go func() {
		hdr := make([]byte, 16)
		for {
			if _, err := io.ReadFull(mr, hdr); err != nil {
				return
			}
			n := binary.BigEndian.Uint32(hdr)
			buf := make([]byte, n)
			if _, err := io.ReadFull(mr, buf); err != nil {
				return
			}
			now := time.Now()
			mu.Lock()
			switch hdr[4] {
			case 2:
				formats = append(formats, string(buf))
			case 0:
				arrivals = append(arrivals, now)
				bytes += int64(n)
				if hdr[5]&1 != 0 {
					keys++
				}
				dump.Write(buf)
			}
			mu.Unlock()
		}
	}()

	t0 := time.Now()
	res, err := r.call("start", params)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("started in %v: %s", time.Since(t0).Round(time.Millisecond), res)
	cpu0, wall0 := cpuTime(cmd.Process.Pid), time.Now()

	if *bitrateAt != "" {
		at, bps, _ := strings.Cut(*bitrateAt, "=")
		d, _ := time.ParseDuration(at)
		time.AfterFunc(d, func() {
			res, err := r.call("set", map[string]json.RawMessage{"video.bitrate": json.RawMessage(bps)})
			log.Printf("set bitrate: %s %v", res, err)
		})
	}
	time.Sleep(*duration)
	cpu1, wall1 := cpuTime(cmd.Process.Pid), time.Now()
	stats, _ := r.call("get", []string{"stats.fps", "stats.capture_avg_ms", "stats.encode_avg_ms", "stats.encode_max_ms", "stats.dropped", "stats.bitrate", "video.active_encoder", "video.hw", "video.size"})
	r.call("stop", nil)
	stdin.Close()
	cmd.Wait()

	mu.Lock()
	defer mu.Unlock()
	fmt.Printf("formats: %s\n", strings.Join(formats, " "))
	fmt.Printf("captured: %s\n", stats)
	if len(arrivals) < 2 {
		fmt.Println("no packets")
		return
	}
	span := arrivals[len(arrivals)-1].Sub(arrivals[0]).Seconds()
	gaps := make([]float64, 0, len(arrivals))
	for i := 1; i < len(arrivals); i++ {
		gaps = append(gaps, float64(arrivals[i].Sub(arrivals[i-1]).Microseconds())/1000)
	}
	sort.Float64s(gaps)
	pct := func(p float64) float64 { return gaps[int(p*float64(len(gaps)-1))] }
	fmt.Printf("packets: %d in %.1fs = %.1f fps, %d keyframes, %.2f Mbit/s\n",
		len(arrivals), span, float64(len(arrivals)-1)/span, keys, float64(bytes*8)/span/1e6)
	fmt.Printf("gap ms: p50 %.1f p95 %.1f p99 %.1f max %.1f\n", pct(.5), pct(.95), pct(.99), gaps[len(gaps)-1])
	if cpu0 >= 0 && cpu1 >= 0 {
		fmt.Printf("captured cpu: %.1f%%\n", 100*(cpu1-cpu0).Seconds()/wall1.Sub(wall0).Seconds())
	}
}
