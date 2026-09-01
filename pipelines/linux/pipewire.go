//go:build linux

package linux

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"distancedesktop/captured/pipelines"
)

// PipeWire capture via GNOME Mutter's private ScreenCast interface.
//
// Unlike the KMS path, this works on a Wayland compositor with no CRTC bound to
// a physical connector, and it needs no DRM permissions -- the compositor owns
// the scanout and hands us a PipeWire node.
//
// org.gnome.Mutter.ScreenCast is used rather than
// org.freedesktop.portal.Desktop.ScreenCast because the portal requires
// interactive user consent through a dialog, which a headless daemon cannot
// satisfy. Mutter's interface is available to any client in the user's session
// bus, so `captured` must run as the session user.
//
// Frames are pulled out of PipeWire with gst-launch-1.0 (pipewiresrc ->
// videoconvert -> BGRA -> fdsink) and read as raw frames off stdout. Shelling
// out to GStreamer avoids binding libpipewire's buffer negotiation by hand; the
// pw node id is the only thing the D-Bus round trip is needed for.

const (
	mutterScreenCastName = "org.gnome.Mutter.ScreenCast"
	mutterScreenCastPath = "/org/gnome/Mutter/ScreenCast"
	mutterDisplayName    = "org.gnome.Mutter.DisplayConfig"
	mutterDisplayPath    = "/org/gnome/Mutter/DisplayConfig"

	// cursorModeEmbedded draws the cursor into the frames, which is what a
	// remote viewer wants (there is no local pointer to composite).
	cursorModeEmbedded = 1
)

// screenCastStartTimeout bounds the wait for Mutter's PipeWireStreamAdded.
const screenCastStartTimeout = 10 * time.Second

type pipewirePipeline struct{}

type pwMonitor struct {
	Connector string
	Width     int
	Height    int
	Refresh   float64
}

// scanPipeWireMonitors lists logical monitors from Mutter's DisplayConfig.
// These are compositor-side monitors, so a virtual display with no physical
// connector attached still appears.
func scanPipeWireMonitors() ([]pwMonitor, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("linux/pipewire: session bus: %w (captured must run in the desktop user's session)", err)
	}
	obj := conn.Object(mutterDisplayName, dbus.ObjectPath(mutterDisplayPath))

	var serial uint32
	// GetCurrentState signature:
	//   u a((ssss)a(siiddada{sv})a{sv}) a((iiduba(ssss)a{sv})) a{sv}
	// The monitor spec is a *struct* of four strings (connector, vendor, product,
	// serial), not a string array -- decoding it as []string fails with
	// "cannot convert a value of []interface {} into []string".
	type monitorSpec struct {
		Connector string
		Vendor    string
		Product   string
		Serial    string
	}
	var monitors []struct {
		Spec  monitorSpec
		Modes []struct {
			ID      string
			Width   int32
			Height  int32
			Refresh float64
			Scale   float64
			Scales  []float64
			Props   map[string]dbus.Variant
		}
		Props map[string]dbus.Variant
	}
	var logical []struct {
		X, Y      int32
		Scale     float64
		Transform uint32
		Primary   bool
		Monitors  []monitorSpec
		Props     map[string]dbus.Variant
	}
	var props map[string]dbus.Variant

	call := obj.Call(mutterDisplayName+".GetCurrentState", 0)
	if call.Err != nil {
		return nil, fmt.Errorf("linux/pipewire: GetCurrentState: %w", call.Err)
	}
	if err := call.Store(&serial, &monitors, &logical, &props); err != nil {
		return nil, fmt.Errorf("linux/pipewire: decode monitor state: %w", err)
	}

	out := make([]pwMonitor, 0, len(monitors))
	for _, m := range monitors {
		if m.Spec.Connector == "" {
			continue
		}
		mon := pwMonitor{Connector: m.Spec.Connector, Refresh: 60}
		for _, mode := range m.Modes {
			if v, ok := mode.Props["is-current"]; ok {
				if cur, ok := v.Value().(bool); ok && cur {
					mon.Width = int(mode.Width)
					mon.Height = int(mode.Height)
					mon.Refresh = mode.Refresh
					break
				}
			}
		}
		if mon.Width == 0 && len(m.Modes) > 0 {
			mon.Width = int(m.Modes[0].Width)
			mon.Height = int(m.Modes[0].Height)
			mon.Refresh = m.Modes[0].Refresh
		}
		out = append(out, mon)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("linux/pipewire: no monitors reported by Mutter")
	}
	return out, nil
}

func (p *pipewirePipeline) SupportedFormats() []pipelines.FrameFormat {
	return []pipelines.FrameFormat{pipelines.FormatBGRA}
}

func (p *pipewirePipeline) ListDisplays(ctx context.Context) ([]pipelines.DisplayMeta, error) {
	mons, err := scanPipeWireMonitors()
	if err != nil {
		return nil, err
	}
	out := make([]pipelines.DisplayMeta, len(mons))
	for i, m := range mons {
		out[i] = pipelines.DisplayMeta{
			ID:          uint32(i),
			Width:       m.Width,
			Height:      m.Height,
			RefreshRate: m.Refresh,
		}
	}
	return out, nil
}

// pwSession holds the Mutter screencast session for one stream.
type pwSession struct {
	conn        *dbus.Conn
	sessionPath dbus.ObjectPath
	nodeID      uint32
}

// startMutterScreenCast creates a session, records the given connector, starts
// it, and waits for the PipeWireStreamAdded signal carrying the node id.
//
// Every step is bounded by screenCastStartTimeout: if Mutter accepts the session
// but never emits the signal (a compositor restart mid-handshake, for instance),
// this must not block StartStream forever, since ctx is the daemon's long-lived
// context. The D-Bus calls use CallWithContext for the same reason -- a
// synchronous Call would ignore the deadline entirely and could block past it
// before the timeout select is ever reached.
func startMutterScreenCast(ctx context.Context, connector string) (*pwSession, error) {
	ctx, cancel := context.WithTimeout(ctx, screenCastStartTimeout)
	defer cancel()

	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, fmt.Errorf("linux/pipewire: session bus: %w", err)
	}
	sc := conn.Object(mutterScreenCastName, dbus.ObjectPath(mutterScreenCastPath))

	var sessionPath dbus.ObjectPath
	if err := sc.CallWithContext(ctx, mutterScreenCastName+".CreateSession", 0, map[string]dbus.Variant{}).Store(&sessionPath); err != nil {
		return nil, fmt.Errorf("linux/pipewire: CreateSession: %w", err)
	}
	sess := conn.Object(mutterScreenCastName, sessionPath)

	// stopSession tears down a session created above. It deliberately does not
	// use ctx: on the timeout path ctx is already expired, and the session must
	// still be released or Mutter keeps recording.
	stopSession := func() {
		_ = sess.Call(mutterScreenCastName+".Session.Stop", 0).Err
	}

	var streamPath dbus.ObjectPath
	opts := map[string]dbus.Variant{
		"cursor-mode": dbus.MakeVariant(uint32(cursorModeEmbedded)),
	}
	if err := sess.CallWithContext(ctx, mutterScreenCastName+".Session.RecordMonitor", 0, connector, opts).Store(&streamPath); err != nil {
		stopSession()
		return nil, fmt.Errorf("linux/pipewire: RecordMonitor(%s): %w", connector, err)
	}

	// Subscribe before Start so the signal cannot be missed.
	matchOpts := []dbus.MatchOption{
		dbus.WithMatchObjectPath(streamPath),
		dbus.WithMatchInterface(mutterScreenCastName + ".Stream"),
		dbus.WithMatchMember("PipeWireStreamAdded"),
	}
	if err := conn.AddMatchSignalContext(ctx, matchOpts...); err != nil {
		stopSession()
		return nil, fmt.Errorf("linux/pipewire: AddMatchSignal: %w", err)
	}
	sigCh := make(chan *dbus.Signal, 4)
	conn.Signal(sigCh)
	// The match rule and channel are only needed for this handshake; pwSession
	// does not retain them, so they must not outlive this function.
	defer func() {
		conn.RemoveSignal(sigCh)
		_ = conn.RemoveMatchSignal(matchOpts...)
	}()

	if err := sess.CallWithContext(ctx, mutterScreenCastName+".Session.Start", 0).Err; err != nil {
		stopSession()
		return nil, fmt.Errorf("linux/pipewire: Session.Start: %w", err)
	}

	for {
		select {
		case sig, ok := <-sigCh:
			// A closed connection closes sigCh, yielding a nil signal.
			if !ok || sig == nil {
				stopSession()
				return nil, fmt.Errorf("linux/pipewire: session bus closed while waiting for PipeWireStreamAdded")
			}
			if sig.Path != streamPath || !strings.HasSuffix(sig.Name, "PipeWireStreamAdded") {
				continue
			}
			if len(sig.Body) == 0 {
				continue
			}
			id, ok := sig.Body[0].(uint32)
			if !ok {
				continue
			}
			return &pwSession{conn: conn, sessionPath: sessionPath, nodeID: id}, nil
		case <-ctx.Done():
			stopSession()
			return nil, fmt.Errorf("linux/pipewire: timed out waiting for PipeWireStreamAdded: %w", ctx.Err())
		}
	}
}

func (s *pwSession) stop() {
	if s == nil || s.conn == nil {
		return
	}
	_ = s.conn.Object(mutterScreenCastName, s.sessionPath).
		Call(mutterScreenCastName+".Session.Stop", 0).Err
}

// pipewireGrabber reads raw BGRA frames from a gst-launch-1.0 pipeline attached
// to the screencast's PipeWire node.
type pipewireGrabber struct {
	sess   *pwSession
	cmd    *exec.Cmd
	stdout io.ReadCloser
	width  int
	height int
	frame  []byte
	once   sync.Once
}

// interrupt unblocks a grab() that is waiting on a partial frame, so a stream
// can be closed while the compositor is idle and producing nothing. Killing the
// child and closing the pipe both make the pending read return an error.
// Safe to call more than once, and safe to call concurrently with grab().
func (g *pipewireGrabber) interrupt() {
	if g.cmd != nil && g.cmd.Process != nil {
		_ = g.cmd.Process.Kill()
	}
	_ = g.stdout.Close()
}

func newPipeWireCapture(ctx context.Context, connector string, w, h, fps int) (grabber, error) {
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		return nil, fmt.Errorf("linux/pipewire: gst-launch-1.0 not found (install gstreamer1.0-tools + gstreamer1.0-pipewire): %w", err)
	}
	sess, err := startMutterScreenCast(ctx, connector)
	if err != nil {
		return nil, err
	}

	// Force BGRA at a fixed size so frame length is predictable; the agent
	// expects tightly packed BGRA with no stride padding.
	caps := fmt.Sprintf("video/x-raw,format=BGRA,width=%d,height=%d", w, h)
	args := []string{
		"-q",
		"pipewiresrc", "path=" + strconv.FormatUint(uint64(sess.nodeID), 10),
		"do-timestamp=true", "keepalive-time=1000",
		"!", "videorate",
		"!", fmt.Sprintf("video/x-raw,framerate=%d/1", fps),
		"!", "videoscale",
		"!", "videoconvert", "chroma-mode=none", "dither=none",
		"!", caps,
		"!", "fdsink", "fd=1", "sync=false",
	}
	cmd := exec.Command("gst-launch-1.0", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		sess.stop()
		return nil, fmt.Errorf("linux/pipewire: stdout pipe: %w", err)
	}
	// Surface GStreamer's diagnostics: a caps negotiation failure otherwise looks
	// like an unexplained EOF on the first frame read.
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		sess.stop()
		return nil, fmt.Errorf("linux/pipewire: start gst-launch-1.0: %w", err)
	}

	return &pipewireGrabber{
		sess:   sess,
		cmd:    cmd,
		stdout: stdout,
		width:  w,
		height: h,
		frame:  make([]byte, w*h*4),
	}, nil
}

func (g *pipewireGrabber) grab() ([]byte, int, int, error) {
	if _, err := io.ReadFull(g.stdout, g.frame); err != nil {
		return nil, 0, 0, fmt.Errorf("linux/pipewire: read frame: %w", err)
	}
	out := make([]byte, len(g.frame))
	copy(out, g.frame)
	return out, g.width, g.height, nil
}

func (g *pipewireGrabber) close() error {
	g.once.Do(func() {
		g.interrupt()
		if g.cmd != nil && g.cmd.Process != nil {
			_, _ = g.cmd.Process.Wait()
		}
		g.sess.stop()
	})
	return nil
}

func (p *pipewirePipeline) StartStream(ctx context.Context, displayID uint32, fps int) (pipelines.FrameStream, error) {
	if fps <= 0 {
		fps = 60
	}
	mons, err := scanPipeWireMonitors()
	if err != nil {
		return nil, err
	}
	if int(displayID) >= len(mons) {
		return nil, fmt.Errorf("linux/pipewire: display %d not found (%d available)", displayID, len(mons))
	}
	m := mons[displayID]

	g, err := newPipeWireCapture(ctx, m.Connector, m.Width, m.Height, fps)
	if err != nil {
		return nil, err
	}
	// PipeWire delivers frames at its own cadence; the frame stream must not
	// re-tick, so it is driven by reads returning.
	return newPulledFrameStream(ctx, g), nil
}
