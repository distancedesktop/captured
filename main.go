// captured: a dumb capture daemon. It grabs a display and hands out raw BGRA
// frames; encoding and everything else is the agent's job.
//
// Control: JSON lines on a unix socket (and optionally TCP).
// Media:   [u32 BE width][u32 BE height][width*height*4 BGRA] per frame, on a
//
//	unix socket that exists while a stream is running.
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"distancedesktop/captured/pipelines"
)

const (
	version           = "0.2.0"
	firstFrameTimeout = 5 * time.Second
)

var (
	controlPath = flag.String("socket", "/tmp/captured.socket", "control socket path")
	mediaPath   = flag.String("media-socket", "/tmp/captured-media.socket", "media socket path")
	listenTCP   = flag.String("listen", "", "also accept control connections on this TCP address (e.g. :9090)")
	pipeName    = flag.String("pipeline", "auto", "capture pipeline, or auto")
	listOnly    = flag.Bool("list", false, "print pipelines and their displays, then exit")
)

type request struct {
	Type      string `json:"type"`
	DisplayID uint32 `json:"display_id"`
	FPS       int    `json:"fps"`
}

type server struct {
	ctx  context.Context
	pipe pipelines.Pipeline

	mu     sync.Mutex
	stream pipelines.Stream
	media  net.Listener
}

func main() {
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *listOnly {
		list(ctx)
		return
	}

	pipe, err := choose(ctx)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("pipeline: %s", pipe.Name())

	s := &server{ctx: ctx, pipe: pipe}
	os.Remove(*controlPath)
	ul, err := net.Listen("unix", *controlPath)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("control: %s", *controlPath)
	go s.accept(ul)
	if *listenTCP != "" {
		tl, err := net.Listen("tcp", *listenTCP)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("control: tcp %s", tl.Addr())
		go s.accept(tl)
	}

	<-ctx.Done()
	ul.Close()
	s.stop()
	os.Remove(*controlPath)
	os.Remove(*mediaPath)
}

func choose(ctx context.Context) (pipelines.Pipeline, error) {
	if *pipeName != "auto" {
		p, err := pipelines.Get(*pipeName)
		if err != nil {
			return nil, err
		}
		if err := p.Probe(ctx); err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name(), err)
		}
		return p, nil
	}
	p, skipped := pipelines.Auto(ctx)
	for _, err := range skipped {
		log.Printf("skipped %v", err)
	}
	if p == nil {
		return nil, pipelines.ErrNoPipeline
	}
	return p, nil
}

func list(ctx context.Context) {
	for _, p := range pipelines.All() {
		if err := p.Probe(ctx); err != nil {
			fmt.Printf("%s: unavailable: %v\n", p.Name(), err)
			continue
		}
		ds, err := p.ListDisplays(ctx)
		if err != nil {
			fmt.Printf("%s: %v\n", p.Name(), err)
			continue
		}
		fmt.Printf("%s:\n", p.Name())
		for _, d := range ds {
			fmt.Printf("  %d %s %dx%d@%.2f +%d+%d\n", d.ID, d.Name, d.Width, d.Height, d.RefreshRate, d.X, d.Y)
		}
	}
}

func (s *server) accept(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go s.serve(c)
	}
}

func (s *server) serve(c net.Conn) {
	defer c.Close()
	dec := json.NewDecoder(c)
	enc := json.NewEncoder(c)
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			return
		}
		resp, err := s.handle(req)
		if err != nil {
			resp = map[string]any{"type": "error", "error": err.Error()}
		}
		if enc.Encode(resp) != nil {
			return
		}
	}
}

func (s *server) handle(req request) (any, error) {
	switch req.Type {
	case "list-displays":
		ds, err := s.pipe.ListDisplays(s.ctx)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "displays", "displays": ds}, nil
	case "start-stream":
		return s.start(req.DisplayID, req.FPS)
	case "stop-stream":
		s.stop()
		return map[string]any{"type": "stream-stopped"}, nil
	case "stats":
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.stream == nil {
			return map[string]any{"type": "stats", "streaming": false}, nil
		}
		return map[string]any{"type": "stats", "streaming": true, "stats": s.stream.Stats()}, nil
	case "info":
		names := []string{}
		for _, p := range pipelines.All() {
			names = append(names, p.Name())
		}
		return map[string]any{
			"type":      "info",
			"version":   version,
			"formats":   []pipelines.Format{pipelines.FormatBGRA},
			"pipeline":  s.pipe.Name(),
			"pipelines": names,
		}, nil
	}
	return nil, fmt.Errorf("unknown type: %s", req.Type)
}

func (s *server) start(display uint32, fps int) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()

	st, err := s.pipe.Start(s.ctx, display, fps)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.pipe.Name(), err)
	}
	os.Remove(*mediaPath)
	ml, err := net.Listen("unix", *mediaPath)
	if err != nil {
		st.Close()
		return nil, err
	}
	s.stream, s.media = st, ml

	// Grab the size from the first frame so the reply can carry it.
	var first *pipelines.Frame
	ok := true
	select {
	case first, ok = <-st.Frames():
	case <-time.After(firstFrameTimeout):
		s.stopLocked()
		return nil, fmt.Errorf("no frame within %v", firstFrameTimeout)
	}
	if !ok {
		s.stopLocked()
		if e := st.Stats().Err; e != "" {
			return nil, errors.New(e)
		}
		return nil, errors.New("stream ended before the first frame")
	}
	go s.pump(st, ml, first)
	return map[string]any{
		"type":     "stream-started",
		"socket":   *mediaPath,
		"format":   pipelines.FormatBGRA,
		"pipeline": s.pipe.Name(),
		"width":    first.Width,
		"height":   first.Height,
	}, nil
}

// pump serves one media client. When it goes away the stream stops, so
// nothing keeps capturing for nobody.
func (s *server) pump(st pipelines.Stream, ml net.Listener, first *pipelines.Frame) {
	defer func() {
		s.mu.Lock()
		if s.stream == st {
			s.stopLocked()
		}
		s.mu.Unlock()
	}()
	c, err := ml.Accept()
	ml.Close()
	if err != nil {
		first.Release()
		return
	}
	defer c.Close()
	hdr := make([]byte, 8)
	write := func(f *pipelines.Frame) error {
		defer f.Release()
		binary.BigEndian.PutUint32(hdr[0:4], uint32(f.Width))
		binary.BigEndian.PutUint32(hdr[4:8], uint32(f.Height))
		bufs := net.Buffers{hdr, f.Data}
		_, err := bufs.WriteTo(c)
		return err
	}
	if write(first) != nil {
		return
	}
	for f := range st.Frames() {
		if write(f) != nil {
			return
		}
	}
	if e := st.Stats().Err; e != "" {
		log.Printf("stream ended: %s", e)
	}
}

func (s *server) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

func (s *server) stopLocked() {
	if s.media != nil {
		s.media.Close()
		s.media = nil
	}
	if s.stream != nil {
		s.stream.Close()
		s.stream = nil
	}
}
