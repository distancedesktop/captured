package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"

	"distancedesktop/captured/pipelines"
	"distancedesktop/captured/pipelines/macos"
)

var pipeline pipelines.Pipeline

func _getPipeline() pipelines.Pipeline {
	switch runtime.GOOS {
	  case "darwin":
		  return macos.New()
	  default:
		  log.Fatalf("unsupported platform: %s", runtime.GOOS)
	}
}

func main() {
	// platform check

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	os.Remove("captured.socket")
	l, err := net.Listen("unix", "captured.socket")
	if err != nil {
		log.Fatal(err)
	}
	log.Println("listening on captured.socket")

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
				os.Remove("captured-media.socket")
				ml, err := net.Listen("unix", "captured-media.socket")
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
					// accept one client and stream frames to it
					mc, err := ml.Accept()
					ml.Close()
					if err != nil {
						return
					}
					defer mc.Close()
					for f := range s.Frames() {
						header := make([]byte, 4)
						binary.BigEndian.PutUint32(header, uint32(len(f.Data)))
						if _, err := mc.Write(header); err != nil {
							return
						}
						if _, err := mc.Write(f.Data); err != nil {
							return
						}
					}
				}()
				streamMu.Unlock()
				enc.Encode(map[string]string{"type": "stream-started", "socket": "captured-media.socket"})

			case "stop-stream":
				streamMu.Lock()
				if stream != nil {
					stream.Close()
					stream = nil
				}
				os.Remove("captured-media.socket")
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

	go func() {
		<-ctx.Done()
		l.Close()
	}()

	for {
		conn, err := l.Accept()
		if err != nil {
			break
		}
		go serveConn(conn)
	}

	streamMu.Lock()
	if stream != nil {
		stream.Close()
	}
	streamMu.Unlock()
	os.Remove("captured-media.socket")
}
