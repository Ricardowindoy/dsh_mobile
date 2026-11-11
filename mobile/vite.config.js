import { defineConfig } from "vite";

// Tauri 移动端壳：把 src 打包到 dist（解析 @tauri-apps/* 裸模块导入，
// WebView 内无打包器时动态 import 这些包会失败——getVersion/openUrl 静默
// 失效导致"永远提示更新""下载按钮没反应"）。
export default defineConfig({
  root: "src",
  base: "./",
  build: {
    outDir: "../dist",
    emptyOutDir: true,
    target: "chrome100",
    rollupOptions: {
      input: "src/index.html",
    },
  },
  server: {
    port: 1420,
    strictPort: true,
  },
});
