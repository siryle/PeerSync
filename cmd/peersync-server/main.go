package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/netcraft/peersync/internal/discover"
)

func main() {
	listen := flag.String("listen", ":8080", "http listen address")
	stun := flag.String("stun", ":3478", "udp address reflector")
	flag.Parse()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	srv := &discover.Server{Listen: *listen, Stun: *stun, Log: slog.Default()}
	if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "peersync-server:", err)
		os.Exit(1)
	}
}
