//go:build !darwin

// Package macos provides the macOS capture pipeline. On non-Darwin hosts it
// compiles to a stub so the package remains importable; the real
// implementation lives behind the `darwin` build tag.
package macos

import (
	"context"
	"fmt"

	"distancedesktop/captured/pipelines"
)

type unsupportedPipeline struct{}

// New always returns the unsupported stub on non-Darwin platforms.
func New() pipelines.Pipeline { return &unsupportedPipeline{} }

func (p *unsupportedPipeline) ListDisplays(ctx context.Context) ([]pipelines.DisplayMeta, error) {
	return nil, fmt.Errorf("macos pipeline is only supported on darwin")
}

func (p *unsupportedPipeline) SupportedFormats() []pipelines.FrameFormat {
	return nil
}

func (p *unsupportedPipeline) StartStream(ctx context.Context, displayID uint32, fps int) (pipelines.FrameStream, error) {
	return nil, fmt.Errorf("macos pipeline is only supported on darwin")
}
