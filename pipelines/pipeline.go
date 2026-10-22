// Package pipelines holds every capture pipeline captured knows about.
//
// One pipeline per file, named <source>_<capturer>_<os>.go (kms_nvidia_linux.go,
// pipewire_mutter_linux.go, ...). Each file registers itself from init() behind
// a build tag, so a binary only ever contains the pipelines its OS can run.
package pipelines

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

type Display struct {
	ID          uint32  `json:"id"`
	Name        string  `json:"name,omitempty"`
	Width       int     `json:"width"`
	Height      int     `json:"height"`
	X           int     `json:"x"`
	Y           int     `json:"y"`
	RefreshRate float64 `json:"refresh_rate"`
}

type Format string

const FormatBGRA Format = "bgra"

// Frame is one tightly packed BGRA image (len(Data) == Width*Height*4).
// Call Release once the bytes have been written out so the buffer is reused.
type Frame struct {
	Data     []byte
	Width    int
	Height   int
	Seq      uint64
	Captured time.Time
	pool     *bufPool
}

func (f *Frame) Release() {
	if f.pool != nil {
		f.pool.put(f.Data)
		f.pool = nil
	}
}

type Stream interface {
	Frames() <-chan *Frame
	Stats() Stats
	Close() error
}

type Pipeline interface {
	Name() string
	// Probe returns nil when the pipeline can run on this machine right now.
	Probe(ctx context.Context) error
	ListDisplays(ctx context.Context) ([]Display, error)
	Start(ctx context.Context, displayID uint32, fps int) (Stream, error)
}

var ErrNoPipeline = errors.New("no capture pipeline available on this machine")

type entry struct {
	p        Pipeline
	priority int
}

var registry []entry

// Register adds a pipeline. Higher priority wins when picking automatically.
func Register(p Pipeline, priority int) {
	registry = append(registry, entry{p, priority})
	sort.SliceStable(registry, func(i, j int) bool { return registry[i].priority > registry[j].priority })
}

func All() []Pipeline {
	out := make([]Pipeline, len(registry))
	for i, e := range registry {
		out[i] = e.p
	}
	return out
}

func Get(name string) (Pipeline, error) {
	for _, e := range registry {
		if e.p.Name() == name {
			return e.p, nil
		}
	}
	return nil, fmt.Errorf("unknown pipeline %q", name)
}

// Auto returns the highest-priority pipeline whose probe passes, plus the
// probe errors of everything it skipped.
func Auto(ctx context.Context) (Pipeline, []error) {
	var skipped []error
	for _, e := range registry {
		if err := e.p.Probe(ctx); err != nil {
			skipped = append(skipped, fmt.Errorf("%s: %w", e.p.Name(), err))
			continue
		}
		return e.p, skipped
	}
	return nil, append(skipped, ErrNoPipeline)
}

func pickDisplay(ds []Display, id uint32) (int, error) {
	for i, d := range ds {
		if d.ID == id {
			return i, nil
		}
	}
	return 0, fmt.Errorf("display %d not found (%d available)", id, len(ds))
}
