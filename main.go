// captured captures a display and encodes it, in one process, for the agent
// that spawned it.
//
// Control: JSON-RPC 2.0, one message per line, on stdin/stdout. Logs go to
// stderr. captured exits when stdin closes.
// Media:   encoded records on fd 3 (--media), see writer.go.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"distancedesktop/captured/pipelines"
)

const version = "0.3.0"

var (
	mediaOut = flag.String("media", "fd:3", "where encoded records go: fd:N (an inherited fd or handle) or a file path")
	listOnly = flag.Bool("list", false, "print pipelines and their displays, then exit")
)

func main() {
	flag.Parse()
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *listOnly {
		list(ctx)
		return
	}
	media, err := openMedia(*mediaOut)
	if err != nil {
		log.Fatalf("media output: %v", err)
	}
	s := newServer(ctx, os.Stdout, newWriter(media))
	go func() {
		s.serve(os.Stdin)
		stop()
	}()
	<-ctx.Done()
	s.stopSession()
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
