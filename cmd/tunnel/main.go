// Command tunnel runs the dsh-side outbound connector: it pairs with the
// public relay and forwards multiplexed streams to the local dsh web server
// (127.0.0.1:3080). Pairing tokens are supplied through the environment,
// never on the command line.
package main

import (
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"dsh_mobile/internal/diag"
	"dsh_mobile/internal/tunnel"
)

func main() {
	var (
		relayURL      = flag.String("relay", "", "relay device endpoint, e.g. wss://relay.example.com/__dsh_tunnel__/device")
		relayFallback = flag.String("relay-fallback", "", "comma-separated alternative relay endpoints tried in order when a dial fails")
		deviceID      = flag.String("device-id", "", "this machine's device id")
		localAddr     = flag.String("local", "127.0.0.1:3080", "local dsh web server address")
		// RFC 8252 loopback OAuth 回调：App 把 http://127.0.0.1:<port>/callback
		// 重写为 relay 上的 /__dsh_loopback__/<port>/callback，经同一条隧道
		// 回到本机对应端口。默认关——开了等于把本机所有 loopback 端口暴露给
		// 持有 relay access token 的人（同 ssh -L 的信任模型，但显式 opt-in）。
		allowLoopbackPorts = flag.Bool("allow-loopback-ports", false, "allow /__dsh_loopback__/<port>/ callbacks to reach local loopback ports")
		// dsh web 的 GUI 闸门用「每进程启动 token」换 cookie；隧道把它注入到根请求里，
		// 于是手机端无需额外填任何 token（App 只需 relay 的访问 token）。
		guiTokenFile    = flag.String("gui-token-file", "", "file holding dsh web's launch token (empty = no injection)")
		guiJournalUnit  = flag.String("gui-journal-unit", "", "systemd unit to scrape dsh web's launch token from (in-process discovery)")
		guiJournalUser  = flag.Bool("gui-journal-user", false, "use journalctl --user for the unit")
		heartbeat       = flag.Duration("heartbeat", 30*time.Second, "application heartbeat interval")
		maxStreams      = flag.Int("max-streams", 64, "max concurrent forwarded streams")
		streamBuffer    = flag.Int("stream-buffer", 256*1024, "per-stream buffered bytes before backpressure")
		streamRecvLimit = flag.Int("stream-recv-limit", 0, "hard per-stream receive-queue cap in bytes (0 = stream-buffer*8)")
		streamRecvTotal = flag.Int("stream-recv-total", 0, "mux-wide receive-queue budget in bytes (0 = unlimited)")
		idleTimeout     = flag.Duration("idle-timeout", 5*time.Minute, "close idle streams after this (0 = off)")
		reconnectMin    = flag.Duration("reconnect-min", time.Second, "min reconnect backoff")
		reconnectMax    = flag.Duration("reconnect-max", 30*time.Second, "max reconnect backoff")
		logDir          = flag.String("log-dir", "", "directory for the connection lifecycle log (default: <binary dir>/logs)")
		pprofAddr       = flag.String("pprof", "", "serve net/http/pprof on this address, loopback only (e.g. 127.0.0.1:6062); empty = off")
		watchdogURL     = flag.String("watchdog-url", "", "public entrance probe URL (e.g. https://relay.example.com/__dsh_tunnel__/probe); empty = watchdog off")
		watchdogEvery   = flag.Duration("watchdog-interval", 60*time.Second, "watchdog probe cadence")
		watchdogCA      = flag.String("watchdog-ca", "", "private CA file for the watchdog probe (IP entrance); empty = system trust / SSL_CERT_FILE")
		verbose         = flag.Bool("v", false, "debug logging")
	)
	flag.Parse()

	logLevel := slog.LevelInfo
	if *verbose {
		logLevel = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))
	// Opt-in runtime profiling for leak investigations: RSS creeping up on a
	// long-lived tunnel is only diagnosable with a live heap/goroutine
	// profile. Loopback-only, off unless --pprof names an address.
	startPprofServer(*pprofAddr, logger)
	// Connection lifecycle log: dial attempts, pairing, losses with reason and
	// recoveries with the total time from loss to re-paired. Written to a file
	// (not only the journal) because that is what gets uploaded when a phone
	// cannot connect and the journal is not reachable.
	lifecyclePath := diag.LifecycleFromEnv("DSH_TUNNEL_LOG_DIR")
	if lifecyclePath == "" {
		dir := *logDir
		if dir == "" {
			if exe, err := os.Executable(); err == nil {
				dir = filepath.Join(filepath.Dir(exe), "logs")
			} else {
				dir = "logs"
			}
		}
		lifecyclePath = dir
	}
	lifecycleWriter, lifecycleErr := diag.NewFileWriter(filepath.Join(lifecyclePath, diag.LogFileName))
	if lifecycleErr != nil {
		logger.Warn("tunnel: lifecycle log disabled (cannot open file)",
			"dir", lifecyclePath, "err", lifecycleErr)
	}
	if lifecycleWriter != nil {
		defer lifecycleWriter.Close()
	}

	// Pairing token comes from the credentials seam: environment only.
	token := os.Getenv("DSH_TUNNEL_PAIRING_TOKEN")
	if *relayURL == "" || *deviceID == "" || token == "" {
		logger.Error("missing configuration: --relay, --device-id and DSH_TUNNEL_PAIRING_TOKEN are required")
		os.Exit(1)
	}

	var diagSink io.Writer
	if lifecycleWriter != nil {
		diagSink = lifecycleWriter
	}
	var relayURLs []string
	for _, h := range strings.Split(*relayFallback, ",") {
		if h = strings.TrimSpace(h); h != "" {
			relayURLs = append(relayURLs, h)
		}
	}

	lifecycle := diag.New(diagSink, logger,
		"tunnel "+strings.Join(append([]string{*relayURL}, relayURLs...), " | ")+" -> "+*localAddr)
	lifecycle.Records(diag.PrefixTunnelAttempt, "process started",
		"relay", *relayURL,
		"device", *deviceID,
		"local", *localAddr,
		"heartbeat_s", int(heartbeat.Seconds()),
		"idle_timeout_s", int(idleTimeout.Seconds()),
		"max_streams", *maxStreams,
		"pid", os.Getpid())

	t := tunnel.New(tunnel.Config{
		RelayURL:             *relayURL,
		RelayURLs:            relayURLs,
		DeviceID:             *deviceID,
		PairingToken:         token,
		LocalAddr:            *localAddr,
		AllowLoopbackPorts:   *allowLoopbackPorts,
		GUIAuthTokenFile:     *guiTokenFile,
		GUITokenJournalUnit:  *guiJournalUnit,
		GUITokenJournalUser:  *guiJournalUser,
		GUICookieAuthority:   *localAddr,
		HeartbeatInterval:    *heartbeat,
		MaxStreams:           *maxStreams,
		PerStreamBufferBytes: *streamBuffer,
		StreamRecvLimitBytes: *streamRecvLimit,
		TotalRecvLimitBytes:  *streamRecvTotal,
		IdleTimeout:          *idleTimeout,
		ReconnectMin:         *reconnectMin,
		ReconnectMax:         *reconnectMax,
		Watchdog: tunnel.WatchdogConfig{
			PublicProbeURL: *watchdogURL,
			Interval:       *watchdogEvery,
			CAFile:         *watchdogCA,
		},
		Diagnostic: diagSink,
		Logger:     logger,
	})

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	logger.Info("tunnel: started", "relay", *relayURL, "deviceId", *deviceID, "local", *localAddr,
		"lifecycleLog", lifecyclePath)
	<-stop
	logger.Info("tunnel: shutting down")
	lifecycle.Records(diag.PrefixTunnelLost, "process shutting down", "signal", "SIGINT/SIGTERM")
	t.Close()
}
