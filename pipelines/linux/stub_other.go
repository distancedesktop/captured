//go:build !linux

// Package linux provides the Linux capture pipeline. On non-Linux hosts it
// compiles to a stub so the package remains importable; the real
// implementation lives behind the `linux` build tag.
package linux

import (
	"context"
	"fmt"

	"distancedesktop/captured/pipelines"
)

type unsupportedPipeline struct{}

// New always returns the unsupported stub on non-Linux platforms.
func New(source string) pipelines.Pipeline { return &unsupportedPipeline{} }

func (p *unsupportedPipeline) ListDisplays(ctx context.Context) ([]pipelines.DisplayMeta, error) {
	return nil, fmt.Errorf("linux pipeline is only supported on linux")
}

func (p *unsupportedPipeline) SupportedFormats() []pipelines.FrameFormat {
	return nil
}

func (p *unsupportedPipeline) StartStream(ctx context.Context, displayID uint32, fps int) (pipelines.FrameStream, error) {
	return nil, fmt.Errorf("linux pipeline is only supported on linux")
}
