//go:build !windows

// Package windows provides the Windows capture pipeline. On non-Windows hosts
// it compiles to a stub so the package remains importable; the real
// implementation lives behind the `windows` build tag.
package windows

import (
	"context"
	"fmt"

	"distancedesktop/captured/pipelines"
)

type unsupportedPipeline struct{}

// New always returns the unsupported stub on non-Windows platforms.
func New() pipelines.Pipeline { return &unsupportedPipeline{} }

func (p *unsupportedPipeline) ListDisplays(ctx context.Context) ([]pipelines.DisplayMeta, error) {
	return nil, fmt.Errorf("windows pipeline is only supported on windows")
}

func (p *unsupportedPipeline) SupportedFormats() []pipelines.FrameFormat {
	return nil
}

func (p *unsupportedPipeline) StartStream(ctx context.Context, displayID uint32, fps int) (pipelines.FrameStream, error) {
	return nil, fmt.Errorf("windows pipeline is only supported on windows")
}
