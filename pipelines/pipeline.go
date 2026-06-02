package pipelines

import "context"

type DisplayMeta struct {
	ID          uint32  `json:"id"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	X           int     `json:"x"`
	Y           int     `json:"y"`
	RefreshRate float64 `json:"refresh_rate"`
}

type FrameFormat string

const (
	FormatBGRA FrameFormat = "bgra"
)

type EncodedFrame struct {
	Data   []byte
	Format FrameFormat
	Width  int
	Height int
}

type FrameStream interface {
	Frames() <-chan EncodedFrame
	Close() error
}

type Pipeline interface {
	ListDisplays(ctx context.Context) ([]DisplayMeta, error)
	SupportedFormats() []FrameFormat
	StartStream(ctx context.Context, displayID uint32, fps int) (FrameStream, error)
}
