// Command relay runs the public relay service: it pairs dsh-tunnel devices
// over wss and routes phone HTTP requests to them through multiplexed
// streams. See docs/deployment.md for TLS and systemd setup.
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
	"strings"
	"syscall"
	"time"

	"dsh_mobile/internal/diag"
	"dsh_mobile/internal/relay"
)

func main() {
	var (
		listen          = flag.String("listen", ":8080", "public listen address")
		tlsCert         = flag.String("tls-cert", "", "TLS certificate file (enables TLS)")
		tlsKey          = flag.String("tls-key", "", "TLS key file")
		pairingsFile    = flag.String("pairings", "pairings.json", "JSON file mapping pairing tokens to device ids: {\"token\":\"deviceId\"}")
		pairingsInline  = flag.String("pairing", "", "inline pairing token=deviceId (repeatable, comma separated)")
		accessToken     = flag.String("access-token", "", "static access token gate for phone requests (empty = off)")
		accessTokenFile = flag.String("access-token-file", "", "read the access token from a file (docker secrets friendly)")
		heartbeat       = flag.Duration("heartbeat", 30*time.Second, "application heartbeat interval")
		maxStreams      = flag.Int("max-streams", 64, "max concurrent streams per device")
		streamBuffer    = flag.Int("stream-buffer", 256*1024, "per-stream buffered bytes before backpressure")
		streamRecvLimit = flag.Int("stream-recv-limit", 0, "hard per-stream receive-queue cap in bytes (0 = stream-buffer*8)")
		streamRecvTotal = flag.Int("stream-recv-total", 0, "mux-wide receive-queue budget in bytes (0 = unlimited)")
		idleTimeout     = flag.Duration("idle-timeout", 5*time.Minute, "close idle streams after this (0 = off)")
		bandwidth       = flag.Int64("bandwidth", 0, "per-device bandwidth cap bytes/sec (0 = unlimited)")
		maxConn         = flag.Duration("max-conn-duration", 0, "kick devices after this long (0 = forever)")
		bodyLimit       = flag.Int64("body-limit", 32<<20, "forwarded request body limit")
		updatesDir      = flag.String("updates-dir", "updates", "app update manifest/package directory")
		phoneDevices    = flag.String("phone-devices", "", "phone-device roster file: enables QR pairing + per-device credentials (empty = legacy static token only)")
		defaultNodeFile = flag.String("default-node-file", "", "file holding the default device id for root-path routing and the multi-node shell (empty = single-device heuristic only)")
		notifyEnabled   = flag.Bool("notify", false, "enable the approval/question notification plane (requires --phone-devices and an access token)")
		extraListen     = flag.String("extra-listen", "", "additional comma-separated listen addresses served as plain HTTP (reverse-proxy upstreams)")
		logFile         = flag.String("log-file", "", "also append the full relay log to this file (rotated at 8MB, one .1 backup)")
		verbose         = flag.Bool("v", false, "debug logging")
	)
	flag.Parse()

	logLevel := slog.LevelInfo
	if *verbose {
		logLevel = slog.LevelDebug
	}
	// The full relay log (requests, slow stages, device lifecycle) is written
	// to stderr and, when requested, to a file: when the phone cannot connect
	// at all, the journal on the relay host is often not reachable from the
	// device side, but a file can be fetched.
	var logSink io.Writer = os.Stderr
	var logWriter *diag.FileWriter
	if *logFile != "" {
		w, err := diag.NewFileWriter(*logFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "relay: cannot open log file %s: %v\n", *logFile, err)
		} else {
			logWriter = w
			defer logWriter.Close()
			logSink = io.MultiWriter(os.Stderr, w)
		}
	}
	logger := slog.New(slog.NewTextHandler(logSink, &slog.HandlerOptions{Level: logLevel}))

	var extra []string
	for _, addr := range strings.Split(*extraListen, ",") {
		if addr = strings.TrimSpace(addr); addr != "" {
			extra = append(extra, addr)
		}
	}

	pairings, err := loadPairings(*pairingsFile, *pairingsInline)
	if err != nil {
		logger.Error("load pairings", "err", err)
		os.Exit(1)
	}
	if len(pairings) == 0 {
		logger.Error("no pairings configured: provide --pairings or --pairing token=deviceId")
		os.Exit(1)
	}

	token := *accessToken
	if *accessTokenFile != "" {
		data, err := os.ReadFile(*accessTokenFile)
		if err != nil {
			logger.Error("read access token file", "err", err)
			os.Exit(1)
		}
		token = strings.TrimSpace(string(data))
	}

	cfg := relay.Config{
		ListenAddr:           *listen,
		ExtraListenAddrs:     extra,
		TLSCertFile:          *tlsCert,
		TLSKeyFile:           *tlsKey,
		Pairings:             pairings,
		AccessToken:          token,
		HeartbeatInterval:    *heartbeat,
		MaxStreamsPerDevice:  *maxStreams,
		PerStreamBufferBytes: *streamBuffer,
		StreamRecvLimitBytes: *streamRecvLimit,
		TotalRecvLimitBytes:  *streamRecvTotal,
		IdleTimeout:          *idleTimeout,
		BandwidthBytesPerSec: *bandwidth,
		MaxConnDuration:      *maxConn,
		RequestBodyLimit:     *bodyLimit,
		UpdatesDir:           *updatesDir,
		PhoneDevicesFile:     *phoneDevices,
		DefaultNodeFile:      *defaultNodeFile,
		NotifyEnabled:        *notifyEnabled,
		Diagnostic:           logSink,
		Logger:               logger,
	}

	// 文件头只写一次（relay 生命周期行与 mux 行共用同一个 sink）。
	if logWriter != nil {
		_, _ = fmt.Fprintf(logWriter, "# dsh connection lifecycle log v%d — relay listen=%s tls=%v extra=%v\n",
			diag.Version, *listen, *tlsCert != "", extra)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := relay.New(cfg).ListenAndServe(ctx); err != nil && ctx.Err() == nil {
		logger.Error("relay exited", "err", err)
		os.Exit(1)
	}
}

// loadPairings merges the pairings file with inline --pairing flags.
func loadPairings(file, inline string) (map[string]string, error) {
	pairings := make(map[string]string)
	if file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read pairings file: %w", err)
		}
		if err := json.Unmarshal(data, &pairings); err != nil {
			return nil, fmt.Errorf("parse pairings file: %w", err)
		}
	}
	if inline != "" {
		token, deviceID, ok := cutPair(inline)
		if !ok {
			return nil, fmt.Errorf("invalid --pairing %q: want token=deviceId", inline)
		}
		pairings[token] = deviceID
	}
	return pairings, nil
}

func cutPair(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}
