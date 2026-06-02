package macos

import (
	"context"
	"fmt"
	"sync"

	sckit "github.com/LocalKinAI/sckit-go"

	"distancedesktop/captured/pipelines"
)

var _ pipelines.Pipeline = (*macOSPipeline)(nil)

type macOSPipeline struct{}

func New() pipelines.Pipeline {
	return &macOSPipeline{}
}

func (p *macOSPipeline) ListDisplays(ctx context.Context) ([]pipelines.DisplayMeta, error) {
	displays, err := sckit.ListDisplays(ctx)
	if err != nil {
		return nil, fmt.Errorf("macos: list displays: %w", err)
	}
	out := make([]pipelines.DisplayMeta, len(displays))
	for i, d := range displays {
		out[i] = pipelines.DisplayMeta{
			ID:          d.ID,
			Width:       d.Width,
			Height:      d.Height,
			X:           d.X,
			Y:           d.Y,
			RefreshRate: 60.0,
		}
	}
	return out, nil
}

func (p *macOSPipeline) SupportedFormats() []pipelines.FrameFormat {
	return []pipelines.FrameFormat{pipelines.FormatBGRA}
}

func (p *macOSPipeline) StartStream(ctx context.Context, displayID uint32, fps int) (pipelines.FrameStream, error) {
	if fps <= 0 {
		fps = 60
	}

	displays, err := sckit.ListDisplays(ctx)
	if err != nil {
		return nil, fmt.Errorf("macos: list displays: %w", err)
	}
	var target *sckit.Display
	for _, d := range displays {
		if d.ID == displayID {
			target = &d
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("macos: display %d not found", displayID)
	}

	stream, err := sckit.NewStream(ctx, *target, sckit.WithFrameRate(fps))
	if err != nil {
		return nil, fmt.Errorf("macos: new stream: %w", err)
	}

	ch := make(chan pipelines.EncodedFrame, 4)
	fs := &frameStream{ch: ch, stream: stream}

	go fs.run(ctx)
	return fs, nil
}

type frameStream struct {
	ch     chan pipelines.EncodedFrame
	stream *sckit.Stream
	close  sync.Once
}

func (f *frameStream) Frames() <-chan pipelines.EncodedFrame {
	return f.ch
}

func (f *frameStream) Close() error {
	f.close.Do(func() {
		f.stream.Close()
		close(f.ch)
	})
	return nil
}

func (f *frameStream) run(ctx context.Context) {
	defer f.Close()
	for {
		frame, err := f.stream.NextFrameBGRA(ctx)
		if err != nil {
			return
		}
		pix := make([]byte, len(frame.Pixels))
		copy(pix, frame.Pixels)
		select {
		case f.ch <- pipelines.EncodedFrame{
			Data:   pix,
			Format: pipelines.FormatBGRA,
			Width:  frame.Width,
			Height: frame.Height,
		}:
		case <-ctx.Done():
			return
		}
	}
}
