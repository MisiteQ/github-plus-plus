// docker.go 实现基于代理端口的 Docker Registry v2 加速。
//
// 工作原理：把 NAS 的代理地址填入 /etc/docker/daemon.json 的 registry-mirrors
// （或在飞牛 Docker 界面的镜像源里填写），Docker 拉取镜像时就会先访问本服务：
//
//	docker pull nginx
//	  -> GET http://nas:7710/v2/                     (握手，拿到 401 与认证地址)
//	  -> GET http://nas:7710/v2/token?...            (换取令牌，已改写为本机)
//	  -> GET http://nas:7710/v2/library/nginx/manifests/latest
//	  -> GET http://nas:7710/v2/library/nginx/blobs/<digest>
//	  -> (上游 302 时改写 Location 继续经本机中转)
//
// 本服务依次尝试配置的上游（公益镜像站、Docker 官方源），
// 对认证地址与重定向地址做改写，让整个拉取流程始终经过本机，
// 上游不可用时自动切换，实现自托管的镜像加速。
package proxy

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ghpp/ghpp/internal/config"
)

// Docker 官方源相关的固定地址。
const (
	dockerOfficialRegistry = "https://registry-1.docker.io"
	dockerOfficialAuth     = "https://auth.docker.io/token"
	dockerUpstreamOfficial = "official"
)

// isDockerRequest 判断请求是否属于 Docker 镜像拉取。
//
// 两种来源：
//  1. 透明反代：Docker 把 registry-mirrors 配置成本机，路径以 /v2/ 开头，
//     且 Host 不是 GitHub 域名；
//  2. 内部端点：/v2/token 与 /v2/_redirect 是本服务改写出来的回调地址。
func (e *Engine) isDockerRequest(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/v2/") && r.URL.Path != "/v2" {
		return false
	}
	if r.URL.IsAbs() && r.URL.Host != "" {
		return isDockerRegistryHost(stripPort(r.URL.Host))
	}
	host := stripPort(r.Host)
	// GitHub 域名上不存在 /v2/ 路径，不会误伤正常加速。
	if _, ok := e.hosts[host]; ok {
		return false
	}
	return !isGitHubInfra(host)
}

// isDockerRegistryHost 判断域名是否属于 Docker 官方的仓库与认证基础设施。
func isDockerRegistryHost(host string) bool {
	switch host {
	case "registry-1.docker.io", "index.docker.io", "registry.docker.io", "auth.docker.io":
		return true
	}
	return strings.HasSuffix(host, ".docker.com") || strings.HasSuffix(host, ".docker.io")
}

// serveDocker 处理一次 Docker Registry v2 请求。
func (e *Engine) serveDocker(w http.ResponseWriter, r *http.Request) {
	cfg := e.cfg()
	if !cfg.Docker.Enabled {
		http.Error(w, "Docker 加速未启用", http.StatusNotFound)
		return
	}

	e.metrics.CategoryDocker.Add(1)

	switch {
	case r.URL.Path == "/v2/token":
		e.serveDockerToken(w, r)
	case r.URL.Path == "/v2/_redirect":
		e.serveDockerRedirect(w, r, cfg)
	default:
		e.serveDockerRegistry(w, r, cfg)
	}
}

// serveDockerRegistry 把 /v2/ 请求转发给按序尝试的上游。
func (e *Engine) serveDockerRegistry(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	upstreams := e.dockerUpstreams(cfg)

	var lastErr error
	for _, up := range upstreams {
		done := e.tryDockerUpstream(w, r, cfg, up)
		if done {
			return
		}
		lastErr = fmt.Errorf("上游 %s 不可用", up)
		e.logf("warn", "Docker 上游 %s 请求失败，尝试下一个: %v", up, lastErr)
	}

	e.metrics.Failed.Add(1)
	if lastErr == nil {
		lastErr = errors.New("没有配置 Docker 上游")
	}
	http.Error(w, "Docker 加速失败: "+lastErr.Error(), http.StatusBadGateway)
}

// dockerUpstreams 返回去重且保证 "official" 兜底在末尾的上游列表。
func (e *Engine) dockerUpstreams(cfg *config.Config) []string {
	list := cfg.Docker.Upstreams
	out := make([]string, 0, len(list)+1)
	hasOfficial := false
	for _, u := range list {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if u == dockerUpstreamOfficial {
			hasOfficial = true
			continue
		}
		if !strings.Contains(u, "://") {
			u = "https://" + u
		}
		out = append(out, strings.TrimSuffix(u, "/"))
	}
	// 官方源始终排在最后兜底，保证镜像站全部失效时仍可拉取。
	if hasOfficial || len(out) > 0 {
		out = append(out, dockerOfficialRegistry)
	}
	return out
}

// tryDockerUpstream 用指定上游转发一次 Registry 请求。
//
// 返回 true 表示响应已写给客户端（无论成功还是业务性失败）；
// 返回 false 表示该上游连接层失败或服务端错误，应换下一个上游。
func (e *Engine) tryDockerUpstream(w http.ResponseWriter, r *http.Request, cfg *config.Config, upstream string) bool {
	target := upstream + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(cfg.Proxy.ReadTimeoutMS)*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, r.Method, target, r.Body)
	if err != nil {
		return false
	}
	copyDockerHeaders(req.Header, r.Header)
	req.ContentLength = r.ContentLength
	req.Host = hostOf(upstream)

	start := time.Now()
	resp, err := e.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	// 上游 5xx 视为该源不稳定，换下一个上游重试。
	if resp.StatusCode >= 500 {
		return false
	}

	copyResponseHeaders(w.Header(), resp, hostOf(upstream))

	// 认证质询与重定向都需要改写，让后续请求继续经过本机。
	if resp.StatusCode == http.StatusUnauthorized {
		if realm := resp.Header.Get("WWW-Authenticate"); realm != "" {
			resp.Header.Set("WWW-Authenticate", rewriteDockerRealm(realm, r.Host))
		}
	}
	if isRedirect(resp.StatusCode) {
		if loc := resp.Header.Get("Location"); loc != "" {
			resp.Header.Set("Location", rewriteDockerLocation(loc, r.Host))
		}
	}

	w.WriteHeader(resp.StatusCode)
	counter := &countingWriter{w: w}
	n, _ := io.Copy(counter, resp.Body)
	e.metrics.BytesOut.Add(n)
	e.metrics.Accelerated.Add(1)

	latency := time.Since(start)
	e.logf("debug", "Docker %s %s via %s -> %d (%dms, %d bytes)",
		r.Method, r.URL.Path, upstream, resp.StatusCode, latency.Milliseconds(), n)
	return true
}

// serveDockerToken 转发令牌请求到改写前记录的认证源。
//
// _u 参数保存了原始 realm 的完整地址（base64url 编码），
// 例如 https://auth.docker.io/token。
func (e *Engine) serveDockerToken(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("_u")
	if raw == "" {
		http.Error(w, "缺少 _u 参数", http.StatusBadRequest)
		return
	}
	decoded, err := b64decode(raw)
	if err != nil || !strings.Contains(decoded, "://") {
		http.Error(w, "_u 参数无效", http.StatusBadRequest)
		return
	}
	// 仅允许 Docker 官方与已配置上游的认证地址，防止被滥用为开放代理。
	u, err := url.Parse(decoded)
	if err != nil || !isDockerAuthHost(stripPort(u.Host)) {
		http.Error(w, "认证地址不受信任", http.StatusForbidden)
		return
	}

	q := r.URL.Query()
	q.Del("_u")
	target := decoded
	if eq := q.Encode(); eq != "" {
		target += "?" + eq
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, target, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	copyDockerHeaders(req.Header, r.Header)
	req.Host = u.Host

	resp, err := e.client.Do(req)
	if err != nil {
		http.Error(w, "获取令牌失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp, u.Host)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// serveDockerRedirect 中转上游的重定向目标（通常是镜像 blob 的 CDN 地址），
// 让大文件下载也走本机的加速与统计。
func (e *Engine) serveDockerRedirect(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	raw := r.URL.Query().Get("to")
	if raw == "" {
		http.Error(w, "缺少 to 参数", http.StatusBadRequest)
		return
	}
	decoded, err := b64decode(raw)
	if err != nil || !strings.Contains(decoded, "://") {
		http.Error(w, "to 参数无效", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(decoded)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		http.Error(w, "to 参数无效", http.StatusBadRequest)
		return
	}
	// SSRF 防护：只允许中转 Docker 官方与已配置上游的地址。
	if !isDockerCDNHost(stripPort(u.Host), e.dockerUpstreams(cfg)) {
		http.Error(w, "重定向目标不受信任", http.StatusForbidden)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(cfg.Proxy.ReadTimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, r.Method, decoded, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	copyDockerHeaders(req.Header, r.Header)
	req.ContentLength = r.ContentLength
	req.Host = u.Host

	resp, err := e.client.Do(req)
	if err != nil {
		http.Error(w, "中转下载失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyResponseHeaders(w.Header(), resp, u.Host)
	if isRedirect(resp.StatusCode) {
		if loc := resp.Header.Get("Location"); loc != "" {
			resp.Header.Set("Location", rewriteDockerLocation(loc, r.Host))
		}
	}
	w.WriteHeader(resp.StatusCode)
	counter := &countingWriter{w: w}
	n, _ := io.Copy(counter, resp.Body)
	e.metrics.BytesOut.Add(n)
}

// rewriteDockerRealm 把认证质询里的 realm 改写为本机地址。
//
// 例如 Bearer realm="https://auth.docker.io/token",service="registry.docker.io"
// 变为   Bearer realm="http://nas:7710/v2/token?_u=aHR0cHM6...",service="registry.docker.io"
func rewriteDockerRealm(realm, selfHost string) string {
	marker := "realm=\""
	i := strings.Index(realm, marker)
	if i < 0 {
		return realm
	}
	rest := realm[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return realm
	}
	origin := rest[:j]
	if strings.HasPrefix(origin, "http://"+selfHost) || strings.HasPrefix(origin, "https://"+selfHost) {
		return realm // 已经指向本机
	}
	encoded := b64encode(origin)
	tail := realm[i+len(marker)+j:]
	return realm[:i] + marker + "http://" + selfHost + "/v2/token?_u=" + encoded + tail
}

// rewriteDockerLocation 把重定向地址改写为本机中转端点。
//
// 只改写指向 Docker 相关域名的绝对地址；其他地址保持原样，
// 由客户端直接访问（例如镜像站自有的对象存储）。
func rewriteDockerLocation(loc, selfHost string) string {
	if loc == "" || strings.HasPrefix(loc, "/") {
		return loc
	}
	u, err := url.Parse(loc)
	if err != nil {
		return loc
	}
	host := stripPort(u.Host)
	if !isDockerCDNHost(host, nil) {
		return loc
	}
	if strings.HasPrefix(loc, "http://"+selfHost) || strings.HasPrefix(loc, "https://"+selfHost) {
		return loc // 已经指向本机
	}
	return "http://" + selfHost + "/v2/_redirect?to=" + b64encode(loc)
}

// isDockerAuthHost 判断是否为允许中转的认证地址。
func isDockerAuthHost(host string) bool {
	if host == "auth.docker.io" {
		return true
	}
	// 部分镜像站使用自己的认证端点。
	return strings.HasSuffix(host, ".daocloud.io") ||
		strings.HasSuffix(host, ".1ms.run") ||
		strings.HasSuffix(host, ".xuanyuan.me") ||
		strings.HasSuffix(host, ".docker.com")
}

// isDockerCDNHost 判断是否为允许中转的 blob 存储地址。
// extra 传入额外允许的上游地址列表（通常来自配置）。
func isDockerCDNHost(host string, extra []string) bool {
	if isDockerRegistryHost(host) {
		return true
	}
	for _, suffix := range []string{".daocloud.io", ".1ms.run", ".xuanyuan.me", ".cloudflarestorage.com"} {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	for _, up := range extra {
		if up == dockerUpstreamOfficial {
			continue
		}
		if h := hostOf(up); h == host {
			return true
		}
	}
	return false
}

// copyDockerHeaders 复制 Docker 客户端请求头。
// Registry 协议依赖 Accept（manifest 类型协商）与 Range（断点续传），
// 必须完整保留；Authorization 是仓库令牌，同样保留。
func copyDockerHeaders(dst, src http.Header) {
	for k, vv := range src {
		lk := strings.ToLower(k)
		switch lk {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
			"proxy-connection", "te", "trailer", "transfer-encoding", "upgrade",
			// Host 由上游请求单独设置，Host 头交给 Go 传输层管理。
			"host", "accept-encoding":
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	if dst.Get("User-Agent") == "" {
		dst.Set("User-Agent", "docker/ghpp")
	}
}

// b64encode 用 base64url 编码，供 query 参数携带 URL。
func b64encode(s string) string {
	return base64.URLEncoding.EncodeToString([]byte(s))
}

// b64decode 解码 base64url，兼容未带 padding 的变体。
func b64decode(s string) (string, error) {
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return string(b), nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return string(b), nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return string(b), nil
	}
	return "", errors.New("base64 解码失败")
}
