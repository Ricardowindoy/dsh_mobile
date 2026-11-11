// Package boundaries 用 import 白名单强制模块能力边界：
// 任何模块引用白名单之外的内部包都会让测试失败。
// 边界约定见 kb/架构/模块职责边界.md（本文件是其机器可执行形式）。
package boundaries

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// allowed: 包 → 允许 import 的内部包（dsh_mobile/ 前缀）。
var allowed = map[string][]string{
	"internal/protocol": {}, // L0 叶子：零内部依赖
	"internal/update":   {}, // L0 叶子：零内部依赖
	"internal/diag":     {}, // L0 叶子：零内部依赖（连接生命周期日志）
	"internal/mux": {
		"internal/protocol",
		"internal/diag",
	},
	"internal/relay": {
		"internal/mux",
		"internal/protocol",
		"internal/update",
		"internal/diag",
	},
	"internal/tunnel": {
		"internal/mux",
		"internal/protocol",
		"internal/diag",
	},
	"cmd/relay":  {"internal/relay", "internal/diag"},
	"cmd/tunnel": {"internal/tunnel", "internal/diag"},
	// e2e 是集成测试层，允许引用全部业务包（但不得反向被引用）。
	"e2e": {
		"internal/relay",
		"internal/tunnel",
	},
}

func TestBoundaries(t *testing.T) {
	// 用 runtime.Caller 定位仓库根（与测试运行 cwd 无关）。
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	for pkg, allowedList := range allowed {
		imports := collectInternalImports(t, filepath.Join(root, pkg))
		allowedSet := make(map[string]bool, len(allowedList))
		for _, a := range allowedList {
			allowedSet[a] = true
		}
		for _, imp := range imports {
			if !allowedSet[imp] {
				t.Errorf("%s 越界 import %q（允许: %v）", pkg, imp, allowedList)
			}
		}
	}
}

// collectInternalImports 收集包内所有非测试 Go 文件的 dsh_mobile/* import。
func collectInternalImports(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.HasPrefix(path, "dsh_mobile/") {
				seen[strings.TrimPrefix(path, "dsh_mobile/")] = true
			}
		}
	}
	var out []string
	for p := range seen {
		out = append(out, p)
	}
	return out
}
