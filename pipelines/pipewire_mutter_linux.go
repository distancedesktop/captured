//go:build linux

package pipelines

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
)

// pipewire_mutter: ask GNOME's Mutter for a ScreenCast of one monitor over its
// private D-Bus API (the portal would pop a consent dialog), then pull frames
// out of the PipeWire node with gst-launch-1.0.
//
// Frames arrive when Mutter has something new to show. Nothing pads them up to
// a fixed rate, so an idle desktop sends roughly one keepalive frame a second.
// Must run as the desktop user, inside the session (it needs the session bus).

const (
	mutterScreenCast = "org.gnome.Mutter.ScreenCast"
	mutterDisplay    = "org.gnome.Mutter.DisplayConfig"
	cursorEmbedded   = uint32(1)
)

type pipewireMutter struct{}

func init() { Register(pipewireMutter{}, 20) }

func (pipewireMutter) Name() string { return "pipewire_mutter" }

func (pipewireMutter) Probe(ctx context.Context) error {
	if _, err := exec.LookPath("gst-launch-1.0"); err != nil {
		return fmt.Errorf("gst-launch-1.0 not found (gstreamer1.0-tools + gstreamer1.0-pipewire)")
	}
	conn, err := dbus.SessionBus()
	if err != nil {
		return fmt.Errorf("no session bus: %w", err)
	}
	var owner string
	if err := conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, mutterScreenCast).Store(&owner); err != nil {
		return fmt.Errorf("%s is not on the session bus (not GNOME?)", mutterScreenCast)
	}
	return nil
}

type mutterMonitor struct {
	connector string
	w, h      int
	refresh   float64
	x, y      int
}

// mutterMonitors lists monitors that are part of the current layout. Monitors
// without a current mode are switched off and Mutter refuses to record them.
func mutterMonitors(ctx context.Context) ([]mutterMonitor, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, err
	}
	type spec struct{ Connector, Vendor, Product, Serial string }
	var serial uint32
	var monitors []struct {
		Spec  spec
		Modes []struct {
			ID            string
			Width, Height int32
			Refresh       float64
			Scale         float64
			Scales        []float64
			Props         map[string]dbus.Variant
		}
		Props map[string]dbus.Variant
	}
	var logical []struct {
		X, Y      int32
		Scale     float64
		Transform uint32
		Primary   bool
		Monitors  []spec
		Props     map[string]dbus.Variant
	}
	var props map[string]dbus.Variant
	obj := conn.Object(mutterDisplay, "/org/gnome/Mutter/DisplayConfig")
	if err := obj.CallWithContext(ctx, mutterDisplay+".GetCurrentState", 0).Store(&serial, &monitors, &logical, &props); err != nil {
		return nil, fmt.Errorf("GetCurrentState: %w", err)
	}
	pos := map[string][2]int{}
	rotated := map[string]bool{}
	for _, l := range logical {
		for _, m := range l.Monitors {
			pos[m.Connector] = [2]int{int(l.X), int(l.Y)}
			rotated[m.Connector] = l.Transform%2 == 1 // 90/270, flipped or not
		}
	}
	var out []mutterMonitor
	for _, m := range monitors {
		p, active := pos[m.Spec.Connector]
		if !active {
			continue
		}
		for _, mode := range m.Modes {
			if cur, _ := mode.Props["is-current"].Value().(bool); cur {
				w, h := int(mode.Width), int(mode.Height)
				if rotated[m.Spec.Connector] {
					w, h = h, w
				}
				out = append(out, mutterMonitor{m.Spec.Connector, w, h, mode.Refresh, p[0], p[1]})
				break
			}
		}
	}
	return out, nil
}

func mutterDisplays(ms []mutterMonitor) []Display {
	ds := make([]Display, len(ms))
	for i, m := range ms {
		ds[i] = Display{ID: uint32(i), Name: m.connector, Width: m.w, Height: m.h, X: m.x, Y: m.y, RefreshRate: m.refresh}
	}
	return ds
}

func (pipewireMutter) ListDisplays(ctx context.Context) ([]Display, error) {
	ms, err := mutterMonitors(ctx)
	if err != nil {
		return nil, err
	}
	return mutterDisplays(ms), nil
}

func (pipewireMutter) Start(ctx context.Context, displayID uint32, fps int) (Stream, error) {
	ms, err := mutterMonitors(ctx)
	if err != nil {
		return nil, err
	}
	i, err := pickDisplay(mutterDisplays(ms), displayID)
	if err != nil {
		return nil, err
	}
	m := ms[i]
	return startStream(ctx, func() (grabber, error) { return openPipewireMutter(ctx, m, fps) }, fps, false)
}

type pipewireGrabber struct {
	conn    *dbus.Conn
	session dbus.ObjectPath
	cmd     *exec.Cmd
	out     io.ReadCloser
	w, h    int
	once    sync.Once
}

func openPipewireMutter(ctx context.Context, m mutterMonitor, fps int) (grabber, error) {
	conn, err := dbus.SessionBus()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	g := &pipewireGrabber{conn: conn, w: m.w, h: m.h}
	sc := conn.Object(mutterScreenCast, "/org/gnome/Mutter/ScreenCast")
	if err := sc.CallWithContext(ctx, mutterScreenCast+".CreateSession", 0, map[string]dbus.Variant{}).Store(&g.session); err != nil {
		return nil, fmt.Errorf("CreateSession: %w", err)
	}
	sess := conn.Object(mutterScreenCast, g.session)
	var streamPath dbus.ObjectPath
	opts := map[string]dbus.Variant{"cursor-mode": dbus.MakeVariant(cursorEmbedded)}
	if err := sess.CallWithContext(ctx, mutterScreenCast+".Session.RecordMonitor", 0, m.connector, opts).Store(&streamPath); err != nil {
		g.close()
		return nil, fmt.Errorf("RecordMonitor(%s): %w", m.connector, err)
	}
	match := []dbus.MatchOption{
		dbus.WithMatchObjectPath(streamPath),
		dbus.WithMatchInterface(mutterScreenCast + ".Stream"),
		dbus.WithMatchMember("PipeWireStreamAdded"),
	}
	if err := conn.AddMatchSignalContext(ctx, match...); err != nil {
		g.close()
		return nil, err
	}
	sigs := make(chan *dbus.Signal, 8)
	conn.Signal(sigs)
	defer func() {
		conn.RemoveSignal(sigs)
		conn.RemoveMatchSignal(match...)
	}()
	if err := sess.CallWithContext(ctx, mutterScreenCast+".Session.Start", 0).Err; err != nil {
		g.close()
		return nil, fmt.Errorf("Session.Start: %w", err)
	}
	var node uint32
	for node == 0 {
		select {
		case s := <-sigs:
			if s != nil && s.Path == streamPath && len(s.Body) > 0 {
				node, _ = s.Body[0].(uint32)
			}
		case <-ctx.Done():
			g.close()
			return nil, fmt.Errorf("no PipeWire stream from Mutter: %w", ctx.Err())
		}
	}

	g.cmd = exec.Command("gst-launch-1.0", "-q",
		"pipewiresrc", "path="+strconv.FormatUint(uint64(node), 10), "keepalive-time=1000",
		"!", "videoconvert", "chroma-mode=none", "dither=none",
		"!", fmt.Sprintf("video/x-raw,format=BGRA,width=%d,height=%d", m.w, m.h),
		"!", "fdsink", "fd=1", "sync=false",
	)
	g.cmd.Stderr = os.Stderr
	if g.out, err = g.cmd.StdoutPipe(); err != nil {
		g.close()
		return nil, err
	}
	if err := g.cmd.Start(); err != nil {
		g.close()
		return nil, fmt.Errorf("gst-launch-1.0: %w", err)
	}
	return g, nil
}

func (g *pipewireGrabber) size() (int, int) { return g.w, g.h }

func (g *pipewireGrabber) grab(dst []byte) error {
	_, err := io.ReadFull(g.out, dst)
	return err
}

func (g *pipewireGrabber) interrupt() {
	if g.cmd != nil && g.cmd.Process != nil {
		g.cmd.Process.Kill()
	}
}

func (g *pipewireGrabber) close() error {
	g.once.Do(func() {
		g.interrupt()
		if g.cmd != nil && g.cmd.Process != nil {
			g.cmd.Wait()
		}
		if g.session != "" {
			g.conn.Object(mutterScreenCast, g.session).Call(mutterScreenCast+".Session.Stop", 0)
		}
	})
	return nil
}
