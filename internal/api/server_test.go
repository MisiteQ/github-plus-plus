package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ghpp/ghpp/internal/config"
)

// newRouteTestServer 构造一个只用于验证路由表的最小 Server。
//
// 未登录请求会在 guard 阶段被 401 拦下，因此无需初始化 app；
// assets 返回 HTML 模拟真实的前端兜底，用来断言接口请求没有落到它身上。
func newRouteTestServer() *Server {
	cfg := config.Default()
	return &Server{
		auth: NewAuth(func() *config.Config { return cfg }, nil),
		logf: func(string, string, ...any) {},
		assets: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = io.WriteString(w, "<!DOCTYPE html>")
		}),
	}
}

// TestDockerSubRoutesAreRegistered 防止 issue #1 回归：
// "/api/docker" 只匹配精确路径，子路径必须额外注册前缀模式。
func TestDockerSubRoutesAreRegistered(t *testing.T) {
	srv := newRouteTestServer()
	handler := srv.Handler()

	cases := []struct{ method, path string }{
		{http.MethodPost, "/api/docker/apply"},
		{http.MethodPost, "/api/docker/unapply"},
		{http.MethodGet, "/api/docker"},
	}

	for _, c := range cases {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(c.method, c.path, nil))

		if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
			t.Fatalf("%s %s 返回了非 JSON 内容（Content-Type=%q），说明请求落到了前端兜底",
				c.method, c.path, got)
		}
		if body := rec.Body.String(); strings.Contains(body, "<!DOCTYPE") {
			t.Fatalf("%s %s 返回了前端首页 HTML，说明路由未注册", c.method, c.path)
		}
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s 期望 401（未登录），实际 %d", c.method, c.path, rec.Code)
		}
	}
}

// TestAPIUnmatchedPathReturnsJSON404 未注册的 /api/ 路径必须返回 JSON 404，
// 否则任何路由遗漏都会被 200 + index.html 掩盖。
func TestAPIUnmatchedPathReturnsJSON404(t *testing.T) {
	srv := newRouteTestServer()
	handler := srv.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/definitely-not-a-route", nil))

	if rec.Code != http.StatusNotFound {
		t.Fatalf("期望 404，实际 %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Fatalf("期望 JSON 响应，实际 Content-Type=%q", ct)
	}
	if !strings.Contains(rec.Body.String(), "接口不存在") {
		t.Fatalf("响应体缺少可诊断提示：%s", rec.Body.String())
	}
}

// TestMockRouteFallsBackToAssets 确认非 API 路径仍然回落到前端，
// 避免为了修 404 把前端路由打进 JSON 404。
func TestMockRouteFallsBackToAssets(t *testing.T) {
	srv := newRouteTestServer()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))

	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<!DOCTYPE") {
		t.Fatalf("前端路由应回落到 index.html，实际 %d %q", rec.Code, rec.Body.String())
	}
}
