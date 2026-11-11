// Command forwarder-client runs on the machine behind NAT. It replaces the two
// frpc instances: one control connection registers the local ports, and each
// public connection gets its own data connection + local dial.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"dsh_mobile/internal/forwarder"
)

func main() {
	var (
		configPath = flag.String("config", "/etc/dsh-forwarder/client.json", "path to client config JSON")
		verbose    = flag.Bool("v", false, "debug logging")
		checkOnly  = flag.Bool("check", false, "validate config, then exit")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := forwarder.LoadClientConfig(*configPath)
	if err != nil {
		logger.Error("config invalid", "err", err)
		os.Exit(1)
	}
	if *checkOnly {
		logger.Info("config OK", "server", cfg.ServerAddr, "forwards", len(cfg.Forwards))
		return
	}

	client := forwarder.NewClient(cfg, logger)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger.Info("dsh-forwarder-client starting", "server", cfg.ServerAddr, "forwards", len(cfg.Forwards))
	if err := client.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("client exited", "err", err)
		os.Exit(1)
	}
	logger.Info("dsh-forwarder-client stopped")
}
