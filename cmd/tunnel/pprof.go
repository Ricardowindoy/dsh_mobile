package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// startPprofServer serves net/http/pprof on addr. Deliberately opt-in and
// loopback-only: profiles expose process internals (goroutine stacks carry
// request paths; cmdline/heap views can surface secrets), so a non-loopback
// bind is refused rather than silently exposing them on the LAN.
//
// A listen failure (port already taken) is logged and skipped — a profiling
// sidecar must never take the tunnel itself down.
func startPprofServer(addr string, logger *slog.Logger) {
	if addr == "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		logger.Error("pprof: bad address, profiling disabled", "addr", addr, "err", err)
		return
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		logger.Error("pprof: refusing non-loopback address, profiling disabled", "addr", addr)
		return
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("pprof: listen failed, profiling disabled", "addr", addr, "err", err)
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("pprof: server stopped", "err", err)
		}
	}()
	logger.Info("pprof: listening", "addr", ln.Addr().String(),
		"url", fmt.Sprintf("http://%s/debug/pprof/", ln.Addr().String()))
}
