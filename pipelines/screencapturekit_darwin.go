//go:build darwin

package pipelines

import (
	"context"
	"fmt"

	sckit "github.com/LocalKinAI/sckit-go"
)

// screencapturekit: macOS ScreenCaptureKit through sckit-go. Frames arrive as
// the system produces them.

type screenCaptureKit struct{}

func init() { Register(screenCaptureKit{}, 10) }

func (screenCaptureKit) Name() string { return "screencapturekit" }

func (s screenCaptureKit) Probe(ctx context.Context) error {
	_, err := sckit.ListDisplays(ctx)
	return err
}

func (screenCaptureKit) ListDisplays(ctx context.Context) ([]Display, error) {
	ds, err := sckit.ListDisplays(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Display, len(ds))
	for i, d := range ds {
		out[i] = Display{ID: d.ID, Width: d.Width, Height: d.Height, X: d.X, Y: d.Y, RefreshRate: 60}
	}
	return out, nil
}

func (screenCaptureKit) Start(ctx context.Context, displayID uint32, fps int) (Stream, error) {
	fps = clampFPS(fps)
	ds, err := sckit.ListDisplays(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range ds {
		if d.ID != displayID {
			continue
		}
		return startStream(ctx, func() (grabber, error) {
			st, err := sckit.NewStream(ctx, d, sckit.WithFrameRate(fps))
			if err != nil {
				return nil, err
			}
			return &sckGrabber{st: st, ctx: ctx, w: d.Width, h: d.Height}, nil
		}, fps, false)
	}
	return nil, fmt.Errorf("display %d not found", displayID)
}

type sckGrabber struct {
	st   *sckit.Stream
	ctx  context.Context
	w, h int
}

func (g *sckGrabber) size() (int, int) { return g.w, g.h }

func (g *sckGrabber) grab(dst []byte) error {
	f, err := g.st.NextFrameBGRA(g.ctx)
	if err != nil {
		return err
	}
	if f.Width != g.w || f.Height != g.h {
		return fmt.Errorf("frame is %dx%d, stream is %dx%d", f.Width, f.Height, g.w, g.h)
	}
	copy(dst, f.Pixels)
	return nil
}

func (g *sckGrabber) interrupt() { g.st.Close() }

func (g *sckGrabber) close() error { return g.st.Close() }
