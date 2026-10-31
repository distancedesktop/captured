package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"distancedesktop/captured/av"
)

// Media records, back to back:
//
//	u32 BE  payload length
//	u8      type: 0 video packet, 2 stream format (JSON)
//	u8      flags: bit 0 keyframe
//	u16     reserved
//	u64 BE  pts in microseconds
//	payload
//
// A format record comes before the first packet of every session and again
// whenever the stream is reconfigured. Video payloads are Annex B for
// h264/hevc and OBU (low overhead) for av1, one access unit per record.
const (
	recVideo  = 0
	recFormat = 2

	flagKey = 1

	headerLen = 16
	queueLen  = 64
)

func openMedia(spec string) (*os.File, error) {
	if n, ok := strings.CutPrefix(spec, "fd:"); ok {
		fd, err := strconv.ParseUint(n, 10, 64)
		if err != nil {
			return nil, err
		}
		f := os.NewFile(uintptr(fd), "media")
		if f == nil {
			return nil, fmt.Errorf("fd %d is not open", fd)
		}
		return f, nil
	}
	return os.Create(spec)
}

// writer moves records to the media output off the capture thread. When the
// reader falls behind, the queue fills and packets are dropped up to the next
// keyframe, which gets requested right away, so a slow reader costs a
// stutter instead of building latency.
type writer struct {
	q       chan []byte
	waitKey atomic.Bool
	dropped atomic.Uint64
	written atomic.Uint64
	err     atomic.Value
	onDrop  func()
	pool    sync.Pool
}

func newWriter(f *os.File) *writer {
	w := &writer{q: make(chan []byte, queueLen)}
	go w.run(f)
	return w
}

func (w *writer) run(f *os.File) {
	for b := range w.q {
		if _, err := f.Write(b); err != nil {
			w.err.Store(err.Error())
			log.Printf("media output: %v", err)
			for range w.q {
			}
			return
		}
		w.written.Add(1)
		w.pool.Put(b[:0])
	}
}

func (w *writer) record(typ, flags byte, pts int64, payload []byte) []byte {
	b, _ := w.pool.Get().([]byte)
	b = append(b[:0], make([]byte, headerLen)...)
	binary.BigEndian.PutUint32(b[0:], uint32(len(payload)))
	b[4], b[5] = typ, flags
	binary.BigEndian.PutUint64(b[8:], uint64(pts))
	return append(b, payload...)
}

// format queues a format record. It never gets dropped and never blocks: if
// the reader is stuck, older packets make room, since the new stream starts
// on a keyframe anyway.
func (w *writer) format(js []byte) {
	rec := w.record(recFormat, 0, 0, js)
	for {
		select {
		case w.q <- rec:
			w.waitKey.Store(false)
			return
		default:
		}
		select {
		case <-w.q:
			w.dropped.Add(1)
		default:
		}
	}
}

// packet copies p into the queue. Called on the capture thread.
func (w *writer) packet(p av.Packet) error {
	if w.waitKey.Load() {
		if !p.Key {
			w.dropped.Add(1)
			return nil
		}
		w.waitKey.Store(false)
	}
	var flags byte
	if p.Key {
		flags |= flagKey
	}
	select {
	case w.q <- w.record(recVideo, flags, p.PTS, p.Data):
	default:
		w.dropped.Add(1)
		w.waitKey.Store(true)
		if w.onDrop != nil {
			w.onDrop()
		}
	}
	return nil
}
