package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"distancedesktop/captured/av"
	"distancedesktop/captured/pipelines"
)

// JSON-RPC 2.0 over newline-delimited JSON.
//
// Methods:
//
//	list             pipelines (with probe result and displays) and encoders
//	get   [keys]     property values; no params means all of them
//	set   {k: v}     change properties; returns the new values
//	start {k: v}     set, then start streaming
//	stop             stop streaming
//
// Notification from captured: "stopped" {"error": "..."} when a stream ends
// on its own.
type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type server struct {
	ctx   context.Context
	media *writer

	outMu sync.Mutex
	out   *json.Encoder

	mu    sync.Mutex
	props map[string]any
	pipe  pipelines.Pipeline
	sess  pipelines.Session
	live  atomic.Pointer[pipelines.Session]
}

func newServer(ctx context.Context, out io.Writer, media *writer) *server {
	s := &server{ctx: ctx, media: media, out: json.NewEncoder(out), props: map[string]any{}}
	for k, p := range props {
		if p.def != nil {
			s.props[k] = p.def
		}
	}
	// Runs on the capture thread, so it must not take s.mu: stop holds it
	// while waiting for that thread to finish.
	media.onDrop = func() {
		if p := s.live.Load(); p != nil {
			(*p).Keyframe()
		}
	}
	return s
}

func (s *server) send(m rpcMsg) {
	m.JSONRPC = "2.0"
	s.outMu.Lock()
	defer s.outMu.Unlock()
	if err := s.out.Encode(m); err != nil {
		log.Printf("control output: %v", err)
	}
}

func (s *server) serve(in io.Reader) {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var req rpcMsg
		if err := json.Unmarshal(line, &req); err != nil {
			s.send(rpcMsg{ID: json.RawMessage("null"), Error: &rpcError{-32700, err.Error()}})
			continue
		}
		res, err := s.call(req.Method, req.Params)
		if req.ID == nil {
			continue
		}
		if err != nil {
			code := -32000
			var re *rpcError
			if errors.As(err, &re) {
				code = re.Code
			}
			s.send(rpcMsg{ID: req.ID, Error: &rpcError{code, err.Error()}})
			continue
		}
		if res == nil {
			res = struct{}{}
		}
		s.send(rpcMsg{ID: req.ID, Result: res})
	}
}

func (e *rpcError) Error() string { return e.Message }

func (s *server) call(method string, params json.RawMessage) (any, error) {
	switch method {
	case "list":
		return s.list(), nil
	case "get":
		var keys []string
		if len(params) > 0 && string(params) != "null" {
			if err := json.Unmarshal(params, &keys); err != nil {
				return nil, &rpcError{-32602, "get takes a list of keys"}
			}
		}
		return s.get(keys)
	case "set", "start":
		var kv map[string]json.RawMessage
		if len(params) > 0 && string(params) != "null" {
			if err := json.Unmarshal(params, &kv); err != nil {
				return nil, &rpcError{-32602, method + " takes an object of properties"}
			}
		}
		changed, err := s.set(kv, method == "set")
		if err != nil {
			return nil, err
		}
		if method == "start" {
			if err := s.start(); err != nil {
				return nil, err
			}
			return s.get(nil)
		}
		return s.get(changed)
	case "stop":
		s.stopSession()
		return nil, nil
	}
	return nil, &rpcError{-32601, "unknown method " + method}
}

type pipelineInfo struct {
	Name     string              `json:"name"`
	Error    string              `json:"error,omitempty"`
	Displays []pipelines.Display `json:"displays,omitempty"`
}

func (s *server) list() any {
	var ps []pipelineInfo
	for _, p := range pipelines.All() {
		pi := pipelineInfo{Name: p.Name()}
		if err := p.Probe(s.ctx); err != nil {
			pi.Error = err.Error()
		} else if ds, err := p.ListDisplays(s.ctx); err != nil {
			pi.Error = err.Error()
		} else {
			pi.Displays = ds
		}
		ps = append(ps, pi)
	}
	encs := map[string][]string{}
	for _, c := range codecs {
		encs[c] = av.Candidates(c)
	}
	return map[string]any{"pipelines": ps, "encoders": encs}
}

// property describes one key. Changing a restart property while streaming
// restarts the session with the new value (and a new format record).
type property struct {
	def      any
	readonly bool
	restart  bool
	parse    func(json.RawMessage) (any, error)
	live     func(s *server, v any) // applies to a running session
	read     func(s *server) any    // computed value
}

var codecs = []string{"h264", "hevc", "av1"}

func str(allowed ...string) func(json.RawMessage) (any, error) {
	return func(r json.RawMessage) (any, error) {
		var v string
		if err := json.Unmarshal(r, &v); err != nil {
			return nil, errors.New("want a string")
		}
		if len(allowed) > 0 && !slices.Contains(allowed, v) {
			return nil, fmt.Errorf("want one of %s", strings.Join(allowed, ", "))
		}
		return v, nil
	}
}

func num(min, max int64) func(json.RawMessage) (any, error) {
	return func(r json.RawMessage) (any, error) {
		var v int64
		if err := json.Unmarshal(r, &v); err != nil {
			return nil, errors.New("want an integer")
		}
		if v < min || v > max {
			return nil, fmt.Errorf("want %d..%d", min, max)
		}
		return v, nil
	}
}

func strMap(r json.RawMessage) (any, error) {
	var v map[string]string
	if err := json.Unmarshal(r, &v); err != nil {
		return nil, errors.New("want an object of strings")
	}
	return v, nil
}

func (s *server) stats() pipelines.Stats {
	if s.sess == nil {
		return pipelines.Stats{}
	}
	return s.sess.Stats()
}

var props = map[string]property{
	"version": {readonly: true, read: func(*server) any { return version }},
	"state": {readonly: true, read: func(s *server) any {
		if s.sess != nil {
			return "streaming"
		}
		return "idle"
	}},
	"pipeline":      {def: "auto", restart: true, parse: str()},
	"display":       {def: int64(0), restart: true, parse: num(0, 1<<32-1)},
	"video.codec":   {def: "h264", restart: true, parse: str(codecs...)},
	"video.encoder": {def: "auto", restart: true, parse: str()},
	"video.fps":     {def: int64(60), restart: true, parse: num(1, 1000)},
	"video.width":   {def: int64(0), restart: true, parse: num(0, 16384)},
	"video.height":  {def: int64(0), restart: true, parse: num(0, 16384)},
	"video.gop":     {def: int64(0), restart: true, parse: num(0, 1<<30)},
	"video.preset":  {def: "", restart: true, parse: str()},
	"video.options": {def: map[string]string{}, restart: true, parse: strMap},
	"video.bitrate": {def: int64(10_000_000), parse: num(100_000, 1_000_000_000),
		live: func(s *server, v any) { s.sess.SetBitrate(v.(int64)) }},
	// Write-only trigger: set it to true to get an IDR on the next frame.
	"video.keyframe": {parse: func(raw json.RawMessage) (any, error) {
		var b bool
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, errors.New("want true")
		}
		return b, nil
	}, live: func(s *server, v any) {
		if v.(bool) {
			s.sess.Keyframe()
		}
	}},

	"video.active_pipeline": {readonly: true, read: func(s *server) any {
		if s.pipe == nil {
			return nil
		}
		return s.pipe.Name()
	}},
	"video.active_encoder": {readonly: true, read: func(s *server) any { return sessInfo(s).Encoder }},
	"video.hw":             {readonly: true, read: func(s *server) any { return sessInfo(s).HW }},
	"video.size": {readonly: true, read: func(s *server) any {
		i := sessInfo(s)
		return []int{i.Width, i.Height}
	}},
	"stats.fps":            {readonly: true, read: func(s *server) any { return s.stats().FPS }},
	"stats.frames":         {readonly: true, read: func(s *server) any { return s.stats().Frames }},
	"stats.bitrate":        {readonly: true, read: func(s *server) any { return s.stats().Bitrate }},
	"stats.capture_avg_ms": {readonly: true, read: func(s *server) any { return s.stats().CaptureAvgMs }},
	"stats.encode_avg_ms":  {readonly: true, read: func(s *server) any { return s.stats().EncodeAvgMs }},
	"stats.encode_max_ms":  {readonly: true, read: func(s *server) any { return s.stats().EncodeMaxMs }},
	"stats.dropped":        {readonly: true, read: func(s *server) any { return s.media.dropped.Load() }},
	"stats.error":          {readonly: true, read: func(s *server) any { return s.stats().Err }},
}

func sessInfo(s *server) pipelines.Info {
	if s.sess == nil {
		return pipelines.Info{}
	}
	return s.sess.Info()
}

func (s *server) get(keys []string) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(keys) == 0 {
		for k, p := range props {
			if p.readonly || p.def != nil {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
	}
	out := map[string]any{}
	for _, k := range keys {
		p, ok := props[k]
		switch {
		case !ok && strings.HasPrefix(k, "audio."):
			return nil, &rpcError{-32602, k + ": audio is not supported yet"}
		case !ok:
			return nil, &rpcError{-32602, "unknown property " + k}
		case p.read != nil:
			out[k] = p.read(s)
		default:
			out[k] = s.props[k]
		}
	}
	return out, nil
}

// set validates everything before changing anything.
// set applies kv. With restart, changing a restart property while streaming
// restarts the stream.
func (s *server) set(kv map[string]json.RawMessage, restart bool) ([]string, error) {
	vals := map[string]any{}
	for k, raw := range kv {
		p, ok := props[k]
		switch {
		case !ok && strings.HasPrefix(k, "audio."):
			return nil, &rpcError{-32602, k + ": audio is not supported yet"}
		case !ok:
			return nil, &rpcError{-32602, "unknown property " + k}
		case p.readonly:
			return nil, &rpcError{-32602, k + " is read only"}
		}
		v, err := p.parse(raw)
		if err != nil {
			return nil, &rpcError{-32602, k + ": " + err.Error()}
		}
		vals[k] = v
	}
	s.mu.Lock()
	needRestart := false
	var changed []string
	for k, v := range vals {
		p := props[k]
		if p.def != nil {
			s.props[k] = v
			changed = append(changed, k)
		}
		if s.sess != nil {
			if p.live != nil {
				p.live(s, v)
			}
			needRestart = needRestart || p.restart
		}
	}
	s.mu.Unlock()
	sort.Strings(changed)
	if restart && needRestart {
		return changed, s.start()
	}
	return changed, nil
}

func (s *server) choose(name string) (pipelines.Pipeline, error) {
	if name != "auto" {
		p, err := pipelines.Get(name)
		if err != nil {
			return nil, err
		}
		if err := p.Probe(s.ctx); err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name(), err)
		}
		return p, nil
	}
	p, skipped := pipelines.Auto(s.ctx)
	for _, err := range skipped {
		log.Printf("skipped %v", err)
	}
	if p == nil {
		return nil, pipelines.ErrNoPipeline
	}
	return p, nil
}

func (s *server) start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()

	p, err := s.choose(s.props["pipeline"].(string))
	if err != nil {
		return err
	}
	display := uint32(s.props["display"].(int64))
	if display == 0 {
		ds, err := p.ListDisplays(s.ctx)
		if err != nil {
			return err
		}
		if len(ds) == 0 {
			return errors.New("no displays")
		}
		display = ds[0].ID
	}
	cfg := pipelines.Config{
		FPS:     int(s.props["video.fps"].(int64)),
		Width:   int(s.props["video.width"].(int64)),
		Height:  int(s.props["video.height"].(int64)),
		Codec:   s.props["video.codec"].(string),
		Encoder: s.props["video.encoder"].(string),
		Bitrate: s.props["video.bitrate"].(int64),
		GOP:     int(s.props["video.gop"].(int64)),
		Preset:  s.props["video.preset"].(string),
		Options: s.props["video.options"].(map[string]string),
	}
	// The format record has to go out before the first packet, but the
	// encoder (and so the format) is only known once Start returns, so packets
	// wait on a gate until then.
	gate := make(chan struct{})
	cfg.Sink = func(pk av.Packet) error {
		<-gate
		return s.media.packet(pk)
	}
	sess, err := p.Start(s.ctx, display, cfg)
	if err != nil {
		return fmt.Errorf("%s: %w", p.Name(), err)
	}
	info := sess.Info()
	js, _ := json.Marshal(map[string]any{
		"codec": cfg.Codec, "encoder": info.Encoder, "hw": info.HW, "pipeline": p.Name(),
		"display": display, "width": info.Width, "height": info.Height, "fps": cfg.FPS,
	})
	s.media.format(js)
	close(gate)
	s.pipe, s.sess = p, sess
	s.live.Store(&sess)
	go s.watch(sess)
	return nil
}

func (s *server) watch(sess pipelines.Session) {
	<-sess.Done()
	s.mu.Lock()
	mine := s.sess == sess
	if mine {
		s.sess, s.pipe = nil, nil
		s.live.Store(nil)
	}
	s.mu.Unlock()
	if mine {
		msg := sess.Stats().Err
		if msg == "" {
			msg = "stream ended"
		}
		log.Printf("stream stopped: %s", msg)
		params, _ := json.Marshal(map[string]any{"error": msg})
		s.send(rpcMsg{Method: "stopped", Params: params})
	}
}

func (s *server) stopSession() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopLocked()
}

func (s *server) stopLocked() {
	if s.sess != nil {
		sess := s.sess
		s.sess, s.pipe = nil, nil
		s.live.Store(nil)
		sess.Close()
	}
}
