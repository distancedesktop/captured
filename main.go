package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"

	"distancedesktop/captured/pipelines"
	"distancedesktop/captured/pipelines/linux"
	"distancedesktop/captured/pipelines/macos"
)

var pipeline pipelines.Pipeline

func main() {
	listen := flag.String("listen", "", "TCP address for remote control (e.g. :9090)")
	source := flag.String("source", "kms", "capture source on linux: kms|x11")
	flag.Parse()

	switch runtime.GOOS {
	case "darwin":
		pipeline = macos.New()
	case "linux":
		pipeline = linux.New(*source)
	default:
		log.Fatalf("unsupported platform: %s", runtime.GOOS)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	os.Remove("/tmp/captured.socket")
	ul, err := net.Listen("unix", "/tmp/captured.socket")
	if err != nil {
		log.Fatal(err)
	}
	log.Println("unix: /tmp/captured.socket")

	var tl net.Listener
	if *listen != "" {
		tl, err = net.Listen("tcp", *listen)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("tcp: %s", *listen)
	}

	var stream pipelines.FrameStream
	var streamMu sync.Mutex

	serveConn := func(conn net.Conn) {
		defer conn.Close()
		dec := json.NewDecoder(conn)
		enc := json.NewEncoder(conn)
		for {
			var req struct {
				Type      string `json:"type"`
				DisplayID uint32 `json:"display_id,omitempty"`
				FPS       int    `json:"fps,omitempty"`
			}
			if err := dec.Decode(&req); err != nil {
				return
			}
			switch req.Type {
			case "list-displays":
				displays, err := pipeline.ListDisplays(ctx)
				if err != nil {
					enc.Encode(map[string]string{"error": err.Error()})
					continue
				}
				enc.Encode(map[string]any{"type": "displays", "displays": displays})

			case "start-stream":
				if req.FPS == 0 {
					req.FPS = 60
				}
				streamMu.Lock()
				if stream != nil {
					stream.Close()
				}
				s, err := pipeline.StartStream(ctx, req.DisplayID, req.FPS)
				if err != nil {
					streamMu.Unlock()
					enc.Encode(map[string]string{"error": err.Error()})
					continue
				}
				stream = s
				os.Remove("/tmp/captured-media.socket")
				ml, err := net.Listen("unix", "/tmp/captured-media.socket")
				if err != nil {
					stream.Close()
					stream = nil
					streamMu.Unlock()
					enc.Encode(map[string]string{"error": err.Error()})
					continue
				}
				go func() {
					<-ctx.Done()
					ml.Close()
				}()
				go func() {
					mc, err := ml.Accept()
					ml.Close()
					if err != nil {
						return
					}
					defer mc.Close()
					for f := range s.Frames() {
						buf := make([]byte, 8+len(f.Data))
						binary.BigEndian.PutUint32(buf[0:4], uint32(f.Width))
						binary.BigEndian.PutUint32(buf[4:8], uint32(f.Height))
						copy(buf[8:], f.Data)
						if _, err := mc.Write(buf); err != nil {
							return
						}
					}
				}()
				streamMu.Unlock()
				enc.Encode(map[string]any{
					"type":   "stream-started",
					"socket": "/tmp/captured-media.socket",
					"format": "bgra",
				})

			case "stop-stream":
				streamMu.Lock()
				if stream != nil {
					stream.Close()
					stream = nil
				}
				// os.Remove("/tmp/captured-media.socket") - do NOT fucking do this
				streamMu.Unlock()
				enc.Encode(map[string]string{"type": "stream-stopped"})

			case "info":
				formats := pipeline.SupportedFormats()
				s := make([]string, len(formats))
				for i, f := range formats {
					s[i] = string(f)
				}
				enc.Encode(map[string]any{
					"type":    "info",
					"formats": s,
					"version": "0.1.0",
				})

			default:
				enc.Encode(map[string]string{"error": fmt.Sprintf("unknown type: %s", req.Type)})
			}
		}
	}

	accept := func(l net.Listener) {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go serveConn(conn)
		}
	}

	go accept(ul)
	if tl != nil {
		go accept(tl)
	}

	<-ctx.Done()
	ul.Close()
	if tl != nil {
		tl.Close()
	}

	streamMu.Lock()
	if stream != nil {
		stream.Close()
	}
	streamMu.Unlock()
	os.Remove("/tmp/captured-media.socket")
}
