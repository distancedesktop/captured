//go:build linux

// Package linux implements the captured pipelines for Linux. See kms.go and
// gbm.go for the libdrm/libgbm bindings. Spike scope: list real displays and
// stream BGRA (real KMS readback when possible, else a synthetic pattern) over
// the existing unix-socket protocol; encode stays in the agent's ffmpeg path.
package linux

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"distancedesktop/captured/pipelines"
)

// grabber produces one BGRA frame per call.
type grabber interface {
	grab() (bgra []byte, w, h int, err error)
	close() error
}

// ---------------------------------------------------------------------------
// Pipeline entry point + source selector
// ---------------------------------------------------------------------------

type kmsPipeline struct{}

// New returns a Linux capture pipeline for the given source. Supported values
// are "kms" (default), "pipewire" and "x11"; unknown values fall back to kms.
func New(source string) pipelines.Pipeline {
	switch source {
	case "x11":
		return &x11Pipeline{}
	case "pipewire", "pw":
		return &pipewirePipeline{}
	case "", "kms":
		return &kmsPipeline{}
	default:
		return &kmsPipeline{}
	}
}

func (p *kmsPipeline) SupportedFormats() []pipelines.FrameFormat {
	return []pipelines.FrameFormat{pipelines.FormatBGRA}
}

func (p *kmsPipeline) ListDisplays(ctx context.Context) ([]pipelines.DisplayMeta, error) {
	disps, err := scanDRMDisplays()
	if err != nil {
		return nil, err
	}
	out := make([]pipelines.DisplayMeta, len(disps))
	for i, d := range disps {
		out[i] = pipelines.DisplayMeta{
			ID:          d.ID,
			Width:       d.Width,
			Height:      d.Height,
			X:           d.X,
			Y:           d.Y,
			RefreshRate: d.Refresh,
		}
	}
	return out, nil
}

func (p *kmsPipeline) StartStream(ctx context.Context, displayID uint32, fps int) (pipelines.FrameStream, error) {
	if fps <= 0 {
		fps = 60
	}
	disps, err := scanDRMDisplays()
	if err != nil {
		return nil, err
	}
	var target *linuxDisplay
	for i := range disps {
		if disps[i].ID == displayID {
			target = &disps[i]
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("linux/kms: display %d not found", displayID)
	}

	g, err := newKMSCapture(target)
	if err != nil {
		// Real readback unavailable (tiled fb, no perm, etc.): stream a
		// synthetic BGRA pattern so the socket pipeline still works end to
		// end. The agent's ffmpeg encoding is unaffected.
		fmt.Printf("linux/kms: real capture unavailable (%v); streaming synthetic BGRA\n", err)
		g, err = newSynthCapture(target.Width, target.Height)
		if err != nil {
			return nil, err
		}
	}
	return newFrameStream(ctx, g, fps), nil
}

// ---------------------------------------------------------------------------
// Frame stream
// ---------------------------------------------------------------------------

type frameStream struct {
	ch        chan pipelines.EncodedFrame
	cancel    context.CancelFunc
	closeOnce sync.Once
	g         grabber
	done      chan struct{}
}

func newFrameStream(ctx context.Context, g grabber, fps int) *frameStream {
	ctx, cancel := context.WithCancel(ctx)
	fs := &frameStream{
		ch:     make(chan pipelines.EncodedFrame, 4),
		cancel: cancel,
		g:      g,
		done:   make(chan struct{}),
	}
	go func() {
		fs.run(ctx, fps)
		close(fs.done)
	}()
	return fs
}

func (fs *frameStream) run(ctx context.Context, fps int) {
	defer func() {
		_ = fs.g.close()
		close(fs.ch)
	}()
	delay := time.Duration(int64(time.Second) / int64(fps))
	ticker := time.NewTicker(delay)
	defer ticker.Stop()
	for range ticker.C {
		select {
		case <-ctx.Done():
			return
		default:
		}
		bgra, w, h, err := fs.g.grab()
		if err != nil {
			return
		}
		select {
		case fs.ch <- pipelines.EncodedFrame{Data: bgra, Format: pipelines.FormatBGRA, Width: w, Height: h}:
		case <-ctx.Done():
			return
		}
	}
}

func (fs *frameStream) Frames() <-chan pipelines.EncodedFrame {
	return fs.ch
}

// newPulledFrameStream drives a grabber whose grab() blocks until the source
// produces a frame (PipeWire), rather than sampling on a ticker. The source's
// own cadence sets the frame rate.
func newPulledFrameStream(ctx context.Context, g grabber) *frameStream {
	ctx, cancel := context.WithCancel(ctx)
	fs := &frameStream{
		ch:     make(chan pipelines.EncodedFrame, 4),
		cancel: cancel,
		g:      g,
		done:   make(chan struct{}),
	}
	go func() {
		defer func() {
			_ = fs.g.close()
			close(fs.ch)
			close(fs.done)
		}()
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}
			bgra, w, h, err := fs.g.grab()
			if err != nil {
				return
			}
			select {
			case fs.ch <- pipelines.EncodedFrame{Data: bgra, Format: pipelines.FormatBGRA, Width: w, Height: h}:
			case <-ctx.Done():
				return
			}
		}
	}()
	return fs
}

func (fs *frameStream) Close() error {
	fs.closeOnce.Do(func() {
		fs.cancel()
		<-fs.done
	})
	return nil
}

// ---------------------------------------------------------------------------
// Synthetic source (used as KMS fallback; optionally GBM-backed)
// ---------------------------------------------------------------------------

type synthGrabber struct {
	width  int
	height int
	frame  int
	bo     *gbmBO
}

func newSynthCapture(w, h int) (grabber, error) {
	paths, _ := filepath.Glob("/dev/dri/card*")
	sort.Strings(paths)
	for _, p := range paths {
		bo, err := newGBMBuffer(p, w, h)
		if err == nil {
			return &synthGrabber{width: w, height: h, bo: bo}, nil
		}
	}
	// No DRM access: fall back to a pure-Go BGRA buffer.
	return &synthGrabber{width: w, height: h}, nil
}

func (g *synthGrabber) grab() ([]byte, int, int, error) {
	g.frame++
	if g.bo != nil {
		writePatternXRGB(g.bo.Pixels(), g.bo.Stride(), g.width, g.height, g.frame)
		return convXRGB(g.bo.Pixels(), g.bo.Stride(), g.width, g.height), g.width, g.height, nil
	}
	buf := make([]byte, g.width*g.height*4)
	writePatternBGRA(buf, g.width, g.height, g.frame)
	return buf, g.width, g.height, nil
}

func (g *synthGrabber) close() error {
	if g.bo != nil {
		g.bo.Close()
	}
	return nil
}

// writePatternBGRA fills dst (w*h*4, BGRA) with a moving gradient.
func writePatternBGRA(dst []byte, w, h, frame int) {
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			o := (y*w + x) * 4
			dst[o] = byte((x + frame) & 0xff)
			dst[o+1] = byte((y + frame) & 0xff)
			dst[o+2] = byte((x + y + frame) & 0xff)
			dst[o+3] = 0xFF
		}
	}
}

// writePatternXRGB fills dst (stride-pitched XRGB8888: B,G,R,X) with the same
// moving gradient.
func writePatternXRGB(dst []byte, stride, w, h, frame int) {
	for y := 0; y < h; y++ {
		row := dst[y*stride:]
		for x := 0; x < w; x++ {
			o := x * 4
			row[o] = byte((x + frame) & 0xff)
			row[o+1] = byte((y + frame) & 0xff)
			row[o+2] = byte((x + y + frame) & 0xff)
			row[o+3] = 0
		}
	}
}
