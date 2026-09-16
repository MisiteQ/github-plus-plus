// Package web 内嵌控制台前端资源。
//
// 前端是零依赖的单页应用，全部资源编译进二进制，
// 因此 NAS 上无需额外部署静态服务器，也不依赖任何外部 CDN。
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var staticFS embed.FS

// Handler 返回托管前端静态资源的处理器。
//
// 对未命中静态文件的路径统一回落到 index.html，
// 这样前端路由（如 /dashboard）刷新时不会 404。
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// embed 路径写错属于编译期问题，这里退化为 404 提示。
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "前端资源未正确打包", http.StatusInternalServerError)
		})
	}

	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		// 命中真实文件则直接返回。
		if f, err := sub.Open(path); err == nil {
			_ = f.Close()
			setCacheHeaders(w, path)
			fileServer.ServeHTTP(w, r)
			return
		}

		// 其余路径回落到首页。
		setCacheHeaders(w, "index.html")
		serveIndex(w, r, sub)
	})
}

// serveIndex 返回首页内容。
func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "找不到首页文件", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(data)
}

// setCacheHeaders 依据文件类型设置缓存策略。
//
// HTML/CSS/JS 一律不强缓存（每次带 ETag 协商即可），保证版本更新后浏览器立即拿到新资源；
// 图片等资源可长缓存，需要更新时由 index.html 里的 ?v= 版本号破缓存。
func setCacheHeaders(w http.ResponseWriter, path string) {
	switch {
	case strings.HasSuffix(path, ".html"),
		strings.HasSuffix(path, ".css"),
		strings.HasSuffix(path, ".js"):
		w.Header().Set("Cache-Control", "no-cache")
	default:
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}
}
