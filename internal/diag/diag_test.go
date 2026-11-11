package diag

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// lifecycle 文件必须带时间戳、可追加、且每次写一行（上传后能直接读）。
func TestLoggerWritesTimestampedLines(t *testing.T) {
	var buf bytes.Buffer
	l := NewWithOptions(&buf, nil, Options{Header: "test", HeaderOnce: true})
	l.Records(PrefixTunnelLost, "link down", "reason", "peer silent", "uptime_s", "12.5")
	l.Records(PrefixTunnelLinked, "paired with relay", "attempt", 2, "dial_ms", 31)

	out := buf.String()
	if !strings.Contains(out, "# dsh connection lifecycle log v1 — test") {
		t.Fatalf("missing header: %q", out)
	}
	if n := strings.Count(out, "# dsh connection lifecycle log"); n != 1 {
		t.Fatalf("header must appear exactly once, got %d: %q", n, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 records, got %d lines: %q", len(lines), out)
	}
	for _, ln := range lines[1:] {
		if len(ln) < 24 || ln[4] != '-' || ln[10] != 'T' {
			t.Errorf("record line is not timestamped: %q", ln)
		}
	}
	if !strings.Contains(lines[1], "tunnel-lost link down reason=peer silent uptime_s=12.5") {
		t.Errorf("record line malformed: %q", lines[1])
	}
	// 奇数个参数（缺值）不能 panic，也不能吞掉整行。
	l.Records(PrefixTunnelRetry, "retrying", "attempt")
}

// 记录必须同时进 process log（journal），且重连类事件用 Warn 级别。
func TestLoggerMirrorsToStore(t *testing.T) {
	var stderr bytes.Buffer
	store := slog.New(slog.NewTextHandler(&stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	var file bytes.Buffer
	l := New(&file, store, "test")
	l.WriteHeader()
	l.Records(PrefixTunnelLost, "link down", "reason", "x")
	l.Records(PrefixTunnelLinked, "paired", "attempt", 1)

	logs := stderr.String()
	if !strings.Contains(logs, "level=WARN") || !strings.Contains(logs, "tunnel-lost") {
		t.Errorf("lost event should be mirrored at WARN: %q", logs)
	}
	if !strings.Contains(logs, "tunnel-linked") {
		t.Errorf("linked event should be mirrored: %q", logs)
	}
}

// FileWriter 必须把生命周期行落盘，并在超过上限时轮转出 .1 备份。
func TestFileWriterAppendsAndRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "lifecycle.log")
	w, err := NewFileWriter(path)
	if err != nil {
		t.Fatalf("NewFileWriter: %v", err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 冒充已写满，触发轮转。
	w.size = maxFileBytes
	if _, err := w.Write([]byte("second\n")); err != nil {
		t.Fatalf("write after rotate: %v", err)
	}
	backup, err := os.ReadFile(path + ".1")
	if err != nil || !strings.Contains(string(backup), "first") {
		t.Fatalf("rotated backup missing first line: %q err=%v", backup, err)
	}
	cur, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(cur), "second") {
		t.Fatalf("current file should hold the new line: %q err=%v", cur, err)
	}
}
