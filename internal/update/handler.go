// Package update 封装应用版本检查与安装包下载接口（Relay 侧）。
// 对外提供 GET /__dsh_tunnel__/update/check 与
// GET /__dsh_tunnel__/update/download 两条路由，从 updates/manifest.json
// 读取版本清单，按 platform/arch 匹配后返回元信息或文件流。
package update

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Manifest 描述单个平台/架构的发布条目。
type Manifest struct {
	Platform    string `json:"platform"`
	Arch        string `json:"arch"`
	Version     string `json:"version"`
	Filename    string `json:"filename"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	ReleaseNote string `json:"releaseNote"`
	ReleaseAt   string `json:"releaseAt"`
	Force       bool   `json:"force"`
}

// ManifestFile 是 manifest.json 的顶层结构。
type ManifestFile struct {
	Items []Manifest `json:"items"`
}

// Handler 是 update 接口的 HTTP handler 集合。
type Handler struct {
	// Dir 是 updates 目录路径（manifest.json 与包文件所在目录）。
	Dir string
	Log *slog.Logger
}

// NewHandler 创建 update Handler 实例。
func NewHandler(dir string, log *slog.Logger) *Handler {
	if dir == "" {
		dir = "updates"
	}
	if log == nil {
		log = slog.Default()
	}
	return &Handler{Dir: dir, Log: log}
}

// loadManifest 读取 manifest.json，文件不存在时返回空清单（不视为错误）。
func (h *Handler) loadManifest() (*ManifestFile, error) {
	data, err := os.ReadFile(filepath.Join(h.Dir, "manifest.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return &ManifestFile{}, nil
		}
		return nil, err
	}
	var manifest ManifestFile
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, err
	}
	return &manifest, nil
}

// findUpdate 在清单中按 platform/arch 查找条目（大小写不敏感）。
func findUpdate(manifest *ManifestFile, platform, arch string) *Manifest {
	for i := range manifest.Items {
		item := &manifest.Items[i]
		if strings.EqualFold(item.Platform, platform) && strings.EqualFold(item.Arch, arch) {
			return item
		}
	}
	return nil
}

// Check 返回指定平台/架构的最新版本信息；若版本不大于 appVersion 则返回 204。
// GET /__dsh_tunnel__/update/check?platform=&arch=&appVersion=
func (h *Handler) Check(w http.ResponseWriter, r *http.Request) {
	platform := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("platform")))
	arch := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("arch")))
	appVersion := strings.TrimSpace(r.URL.Query().Get("appVersion"))
	if platform == "" || arch == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 400, "msg": "missing platform or arch"})
		return
	}

	manifest, err := h.loadManifest()
	if err != nil {
		h.Log.Error("update: load manifest failed", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 500, "msg": "manifest load failed"})
		return
	}
	item := findUpdate(manifest, platform, arch)
	if item == nil || (appVersion != "" && !isVersionGreater(item.Version, appVersion)) {
		writeJSON(w, http.StatusOK, map[string]any{"code": 204, "msg": "already up to date"})
		return
	}

	base := requestScheme(r) + "://" + r.Host
	writeJSON(w, http.StatusOK, map[string]any{
		"code":        200,
		"version":     item.Version,
		"url":         fmt.Sprintf("%s/__dsh_tunnel__/update/download?platform=%s&arch=%s", base, platform, arch),
		"size":        item.Size,
		"sha256":      item.SHA256,
		"releaseNote": item.ReleaseNote,
		"releaseAt":   item.ReleaseAt,
		"force":       item.Force,
	})
}

// Download 流式返回安装包文件；Android 平台使用专用 MIME。
// GET /__dsh_tunnel__/update/download?platform=&arch=
func (h *Handler) Download(w http.ResponseWriter, r *http.Request) {
	platform := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("platform")))
	arch := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("arch")))
	if platform == "" || arch == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 400, "msg": "missing platform or arch"})
		return
	}
	manifest, err := h.loadManifest()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 500, "msg": "manifest load failed"})
		return
	}
	item := findUpdate(manifest, platform, arch)
	if item == nil || item.Filename == "" || filepath.Base(item.Filename) != item.Filename {
		writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "msg": "release not found"})
		return
	}
	file, err := os.Open(filepath.Join(h.Dir, platform, arch, item.Filename))
	if err != nil {
		h.Log.Error("update: open package failed", "err", err)
		writeJSON(w, http.StatusNotFound, map[string]any{"code": 404, "msg": "package file not found"})
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"code": 500, "msg": "package stat failed"})
		return
	}
	contentType := "application/octet-stream"
	if platform == "android" {
		contentType = "application/vnd.android.package-archive"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", item.Filename))
	// http.ServeContent 自行写入 Content-Length / 状态码并支持 Range。
	http.ServeContent(w, r, item.Filename, info.ModTime(), file)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// isVersionGreater 判断 a 是否严格大于 b，支持 "v" 前缀与多段点分版本号。
func isVersionGreater(a, b string) bool {
	parse := func(s string) []int {
		parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(s), "v"), ".")
		values := make([]int, len(parts))
		for i, part := range parts {
			for _, char := range part {
				if char < '0' || char > '9' {
					break
				}
				values[i] = values[i]*10 + int(char-'0')
			}
		}
		return values
	}
	av, bv := parse(a), parse(b)
	length := len(av)
	if len(bv) > length {
		length = len(bv)
	}
	for i := 0; i < length; i++ {
		var x, y int
		if i < len(av) {
			x = av[i]
		}
		if i < len(bv) {
			y = bv[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// requestScheme 推断请求使用的协议，优先 X-Forwarded-Proto，其次 TLS，默认 http。
func requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if forwarded := r.Header.Get("X-Forwarded-Proto"); forwarded != "" {
		return strings.ToLower(strings.TrimSpace(strings.Split(forwarded, ",")[0]))
	}
	return "http"
}
