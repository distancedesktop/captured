package pipelines

import (
	"context"
	"errors"
	"fmt"
	"log"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"distancedesktop/captured/av"
)

// grabber is what a pipeline implements; session does the rest.
//
// open/grab/close all run on one locked OS thread, so grabbers that hold
// thread-bound state (EGL/GL contexts) need no extra plumbing.
type grabber interface {
	size() (w, h int)
	// grab fills dst (w*h*4 bytes of BGRA). Paced grabbers return immediately
	// with the current image; pulled grabbers block until the source has a new one.
	grab(dst []byte) error
	close() error
}

// gpuGrabber is implemented by grabbers that can hand frames to the encoder
// without leaving the GPU.
type gpuGrabber interface {
	grabber
	// hw is the libav device type, the device to open ("" for the default) and
	// the sw format of the frames grabHW fills.
	hw() (t av.HWType, device string, f av.PixFmt)
	// bindHW is called once the encoder is open on dev; frames are w x h.
	bindHW(dev *av.HWDevice, w, h int) error
	unbindHW()
	grabHW(f av.Frame) error
}

// interrupter is implemented by pulled grabbers whose grab can block forever
// (an idle compositor sends nothing); Close calls it to unblock grab.
type interrupter interface {
	interrupt()
}

const maxFPS = 1000

// clampFPS turns a requested rate into a usable one: 0 means 60.
func clampFPS(fps int) int {
	switch {
	case fps <= 0:
		return 60
	case fps > maxFPS:
		return maxFPS
	}
	return fps
}

// Config is everything a session needs to start.
type Config struct {
	FPS     int
	Width   int // 0: native
	Height  int
	Codec   string // h264, hevc, av1
	Encoder string // "" or "auto": best available for Codec
	Bitrate int64
	GOP     int // frames between keyframes, 0: only on request
	Preset  string
	Options map[string]string
	// Sink gets every encoded packet on the capture thread. Data is only valid
	// during the call.
	Sink func(av.Packet) error
}

// Info describes a running session.
type Info struct {
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Encoder string `json:"encoder"`
	HW      string `json:"hw"` // device frames go in on, "" for system memory
	// LiveBitrate is false when the encoder can't change bitrate in place;
	// callers restart the session for those.
	LiveBitrate bool `json:"-"`
}

type Stats struct {
	Frames       uint64  `json:"frames"`
	FPS          float64 `json:"fps"`
	Bitrate      int64   `json:"bitrate"` // measured over the last second
	CaptureAvgMs float64 `json:"capture_avg_ms"`
	EncodeAvgMs  float64 `json:"encode_avg_ms"`
	EncodeMaxMs  float64 `json:"encode_max_ms"`
	Err          string  `json:"error,omitempty"`
}

type Session interface {
	Info() Info
	Stats() Stats
	SetBitrate(bps int64)
	Keyframe()
	Close() error
	// Done is closed when the session ends on its own or after Close.
	Done() <-chan struct{}
}

type session struct {
	cfg    Config
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once

	mu    sync.Mutex
	g     grabber
	info  Info
	stats Stats

	bitrate  atomic.Int64
	keyframe atomic.Bool
	err      atomic.Value
}

// startSession opens the grabber and an encoder on the session's own locked
// thread, then captures and encodes in one loop. paced: grab on a ticker at
// fps. Otherwise grab back to back and let the source set the rate.
func startSession(ctx context.Context, open func() (grabber, error), cfg Config, paced bool) (Session, error) {
	cfg.FPS = clampFPS(cfg.FPS)
	if cfg.Codec == "" {
		cfg.Codec = "h264"
	}
	if cfg.Bitrate <= 0 {
		cfg.Bitrate = 10_000_000
	}
	if cfg.Sink == nil {
		return nil, errors.New("no sink")
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &session{cfg: cfg, cancel: cancel, done: make(chan struct{})}
	s.bitrate.Store(cfg.Bitrate)
	opened := make(chan error, 1)

	go func() {
		runtime.LockOSThread()
		defer close(s.done)
		g, err := open()
		if err != nil {
			opened <- err
			return
		}
		s.mu.Lock()
		s.g = g
		s.mu.Unlock()
		defer g.close()
		enc, err := s.openEncoder(g)
		if err != nil {
			opened <- err
			return
		}
		defer func() { s.closeEncoder(g, enc) }()
		opened <- nil
		if err := s.run(ctx, g, enc, paced); err != nil && ctx.Err() == nil {
			s.err.Store(err.Error())
		}
	}()

	if err := <-opened; err != nil {
		cancel()
		return nil, err
	}
	return s, nil
}

type encoder struct {
	*av.Encoder
	dev *av.HWDevice // zero copy device, bound to the grabber
	up  *av.HWDevice // upload target for system memory frames (vaapi)
}

func (s *session) closeEncoder(g grabber, e *encoder) {
	if e.Encoder == nil {
		return
	}
	if e.dev != nil {
		g.(gpuGrabber).unbindHW()
	}
	e.Close()
	e.dev.Close()
	e.up.Close()
}

func (s *session) target(g grabber) (sw, sh, w, h int) {
	sw, sh = g.size()
	w, h = sw, sh
	if s.cfg.Width > 0 && s.cfg.Height > 0 {
		w, h = s.cfg.Width, s.cfg.Height
	}
	return sw, sh, w &^ 1, h &^ 1
}

// openEncoder tries each candidate encoder, zero copy first when the grabber
// can do it, and keeps the first that opens.
func (s *session) openEncoder(g grabber) (*encoder, error) {
	names := []string{s.cfg.Encoder}
	if s.cfg.Encoder == "" || s.cfg.Encoder == "auto" {
		names = av.Candidates(s.cfg.Codec)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("no %s encoder in this libav build", s.cfg.Codec)
	}
	sw, sh, w, h := s.target(g)
	base := av.Config{
		SrcW: sw, SrcH: sh, W: w, H: h, FPS: s.cfg.FPS,
		Bitrate: s.bitrate.Load(), GOP: s.cfg.GOP, Preset: s.cfg.Preset, Options: s.cfg.Options,
	}
	var errs []error
	for _, name := range names {
		cfg := base
		cfg.Encoder = name
		if gg, ok := g.(gpuGrabber); ok {
			t, device, f := gg.hw()
			if av.SupportsHW(name, t) {
				e, err := openHW(gg, cfg, t, device, f)
				if err == nil {
					s.setInfo(e, w, h)
					return e, nil
				}
				log.Printf("%s zero copy: %v; trying system memory", name, err)
			}
		}
		var up *av.HWDevice
		if av.Family(name) == "vaapi" {
			d, err := av.NewHWDevice(av.VAAPI, "")
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				continue
			}
			up, cfg.UploadDev = d, d
		}
		enc, err := av.Open(cfg)
		if err != nil {
			up.Close()
			errs = append(errs, err)
			continue
		}
		e := &encoder{Encoder: enc, up: up}
		s.setInfo(e, w, h)
		return e, nil
	}
	return nil, errors.Join(errs...)
}

func openHW(g gpuGrabber, cfg av.Config, t av.HWType, device string, f av.PixFmt) (*encoder, error) {
	dev, err := av.NewHWDevice(t, device)
	if err != nil {
		return nil, err
	}
	cfg.HW, cfg.HWFormat = dev, f
	enc, err := av.Open(cfg)
	if err != nil {
		dev.Close()
		return nil, err
	}
	if err := g.bindHW(dev, cfg.W, cfg.H); err != nil {
		enc.Close()
		dev.Close()
		return nil, err
	}
	return &encoder{Encoder: enc, dev: dev}, nil
}

func (s *session) setInfo(e *encoder, w, h int) {
	s.mu.Lock()
	s.info = Info{Width: w, Height: h, Encoder: e.Name, HW: string(e.HW), LiveBitrate: e.LiveBitrate()}
	s.mu.Unlock()
	log.Printf("encoding %dx%d@%d with %s (frames in %s)", w, h, s.cfg.FPS, e.Name, map[bool]string{true: string(e.HW), false: "system memory"}[e.HW != ""])
}

func (s *session) run(ctx context.Context, g grabber, enc *encoder, paced bool) error {
	var tick <-chan time.Time
	if paced {
		t := time.NewTicker(time.Second / time.Duration(s.cfg.FPS))
		defer t.Stop()
		tick = t.C
	}
	start := time.Now()
	bitrate := s.bitrate.Load()
	var capSum, encSum, encMax time.Duration
	var frames, winFrames uint64
	var winBytes int64
	winStart := start
	sink := func(p av.Packet) error {
		winBytes += int64(len(p.Data))
		return s.cfg.Sink(p)
	}
	for {
		if paced {
			select {
			case <-ctx.Done():
				return nil
			case <-tick:
			}
		} else if ctx.Err() != nil {
			return nil
		}
		if b := s.bitrate.Load(); b != bitrate {
			bitrate = b
			if enc.LiveBitrate() {
				enc.SetBitrate(b)
			}
		}
		if s.keyframe.Swap(false) {
			enc.Keyframe()
		}
		f, err := enc.Frame()
		if err != nil {
			return err
		}
		t0 := time.Now()
		if enc.dev != nil {
			err = g.(gpuGrabber).grabHW(f)
		} else {
			err = g.grab(f.Bytes())
		}
		if err != nil {
			f.Free()
			return err
		}
		t1 := time.Now()
		if err := enc.Encode(f, t0.Sub(start).Microseconds(), sink); err != nil {
			return err
		}
		t2 := time.Now()
		frames++
		winFrames++
		capSum += t1.Sub(t0)
		encSum += t2.Sub(t1)
		encMax = max(encMax, t2.Sub(t1))
		s.mu.Lock()
		s.stats.Frames = frames
		s.stats.CaptureAvgMs = float64(capSum) / float64(frames) / 1e6
		s.stats.EncodeAvgMs = float64(encSum) / float64(frames) / 1e6
		s.stats.EncodeMaxMs = float64(encMax) / 1e6
		if d := t2.Sub(winStart); d >= time.Second {
			s.stats.FPS = float64(winFrames) / d.Seconds()
			s.stats.Bitrate = int64(float64(winBytes*8) / d.Seconds())
			winStart, winFrames, winBytes = t2, 0, 0
		}
		s.mu.Unlock()
	}
}

func (s *session) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

func (s *session) Stats() Stats {
	s.mu.Lock()
	st := s.stats
	s.mu.Unlock()
	if e, ok := s.err.Load().(string); ok {
		st.Err = e
	}
	return st
}

func (s *session) SetBitrate(bps int64) {
	if bps > 0 {
		s.bitrate.Store(bps)
	}
}

func (s *session) Keyframe()             { s.keyframe.Store(true) }
func (s *session) Done() <-chan struct{} { return s.done }

func (s *session) Close() error {
	s.once.Do(func() {
		s.cancel()
		s.mu.Lock()
		if i, ok := s.g.(interrupter); ok {
			i.interrupt()
		}
		s.mu.Unlock()
		<-s.done
	})
	return nil
}
