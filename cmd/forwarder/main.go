// Command forwarder runs the unified ECS-side service: the existing dsh relay
// (HTTP reverse tunnel) behind one or two self-terminated TLS entrances, plus
// a self-contained reverse TCP port forwarder. It replaces nginx (TLS
// termination + dsh vhost) and frps (control + data ports) with ONE binary.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dsh_mobile/internal/diag"
	"dsh_mobile/internal/forwarder"
	"dsh_mobile/internal/relay"
)

func main() {
	var (
		configPath = flag.String("config", "/etc/dsh-forwarder/server.json", "path to server config JSON")
		verbose    = flag.Bool("v", false, "debug logging")
		checkOnly  = flag.Bool("check", false, "validate config and certificate files, then exit")
	)
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := forwarder.LoadServerConfig(*configPath)
	if err != nil {
		logger.Error("config invalid", "err", err)
		os.Exit(1)
	}

	if *checkOnly {
		if err := checkFiles(cfg); err != nil {
			logger.Error("check failed", "err", err)
			os.Exit(1)
		}
		fmt.Println("config OK: all files present and certificates parse")
		return
	}

	// Log sink: stderr + optional rotated relay.log (same as cmd/relay).
	var sink io.Writer = os.Stderr
	var fw *diag.FileWriter
	if cfg.Relay.LogFile != "" {
		w, err := diag.NewFileWriter(cfg.Relay.LogFile)
		if err != nil {
			logger.Error("cannot open relay log file", "err", err)
			os.Exit(1)
		}
		fw = w
		defer fw.Close()
		sink = io.MultiWriter(os.Stderr, w)
	}
	relayLogger := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: level}))

	// Pairings + access token are read exactly like cmd/relay did; keeping the
	// file formats unchanged means the existing relay deployment files work as-is.
	pairings, err := loadPairings(cfg.Relay.PairingsFile)
	if err != nil {
		logger.Error("load pairings", "err", err)
		os.Exit(1)
	}
	if len(pairings) == 0 {
		logger.Error("no pairings configured")
		os.Exit(1)
	}
	accessToken := ""
	if cfg.Relay.AccessTokenFile != "" {
		b, err := os.ReadFile(cfg.Relay.AccessTokenFile)
		if err != nil {
			logger.Error("read access token file", "err", err)
			os.Exit(1)
		}
		accessToken = string(b)
	}

	r := relay.New(relay.Config{
		ListenAddr:           cfg.RelayTLS.Listen, // only used for its own handler diagnostics
		TLSCertFile:          cfg.RelayTLS.CertFile,
		TLSKeyFile:           cfg.RelayTLS.KeyFile,
		Pairings:             pairings,
		DefaultNodeFile:      cfg.Relay.DefaultNodeFile,
		AccessToken:          accessToken,
		PhoneDevicesFile:     cfg.Relay.PhoneDevicesFile,
		NotifyEnabled:        cfg.Relay.NotifyEnabled,
		HeartbeatInterval:    cfg.Relay.Heartbeat.D(),
		MaxStreamsPerDevice:  cfg.Relay.MaxStreams,
		PerStreamBufferBytes: cfg.Relay.StreamBuffer,
		StreamRecvLimitBytes: cfg.Relay.StreamRecvLimit,
		TotalRecvLimitBytes:  cfg.Relay.StreamRecvTotal,
		IdleTimeout:          cfg.Relay.IdleTimeout.D(),
		UpdatesDir:           cfg.Relay.UpdatesDir,
		Diagnostic:           sink,
		Logger:               relayLogger,
	})

	srv := forwarder.NewServer(cfg, r.Handler(), logger)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger.Info("dsh-forwarder starting", "relayTLS", cfg.RelayTLS.Listen,
		"publicTLS", publicTLSLabel(cfg), "portForward", cfg.PortForward.ControlListen)
	if err := srv.ListenAndServe(ctx); err != nil && ctx.Err() == nil {
		logger.Error("forwarder exited", "err", err)
		os.Exit(1)
	}
	// The relay's device registry is owned by the injected handler; drain it.
	r.CloseDevices()
	logger.Info("dsh-forwarder stopped")
}

func publicTLSLabel(cfg forwarder.ServerConfig) string {
	if cfg.PublicTLS == nil {
		return "(disabled)"
	}
	return cfg.PublicTLS.Listen
}

func loadPairings(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pairings file: %w", err)
	}
	var p map[string]string
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse pairings file: %w", err)
	}
	return p, nil
}

func checkFiles(cfg forwarder.ServerConfig) error {
	// Reuse the same certificate parser the server uses; a bad pair fails here
	// instead of at the first handshake.
	for _, t := range []forwarder.TLSConfig{cfg.RelayTLS} {
		cs, err := forwarder.NewCertSource(t.CertFile, t.KeyFile, time.Hour, slog.Default())
		if err != nil {
			return err
		}
		cs.Close()
	}
	if cfg.PublicTLS != nil {
		cs, err := forwarder.NewCertSource(cfg.PublicTLS.CertFile, cfg.PublicTLS.KeyFile, time.Hour, slog.Default())
		if err != nil {
			return err
		}
		cs.Close()
	}
	for _, p := range []string{cfg.Relay.PairingsFile, cfg.Relay.AccessTokenFile} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
	}
	return nil
}
