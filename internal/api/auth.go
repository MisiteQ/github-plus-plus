// Package api 提供 Web 控制台的 HTTP 接口与认证。
package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ghpp/ghpp/internal/config"
)

// session 描述一个已登录的会话。
type session struct {
	token     string
	expiresAt time.Time
	remote    string
}

// Auth 实现基于签名令牌的登录认证。
//
// 设计取舍：不引入数据库或外部依赖，会话保存在内存中，
// 进程重启后需要重新登录。对于 NAS 场景这足够安全且实现简单。
type Auth struct {
	cfg    func() *config.Config
	logf   func(level, format string, args ...any)
	secret []byte

	mu       sync.RWMutex
	sessions map[string]session

	// 登录失败节流，防止暴力破解。
	fails    map[string]*failRecord
	failsMu  sync.Mutex
}

// failRecord 记录某个来源的登录失败情况。
type failRecord struct {
	count     int
	lockUntil time.Time
}

// NewAuth 创建认证器。
func NewAuth(cfg func() *config.Config, logf func(level, format string, args ...any)) *Auth {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// 熵源异常时退化到时间派生密钥，仍能保证会话不可跨进程复用。
		copy(secret, []byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	}
	if logf == nil {
		logf = func(string, string, ...any) {}
	}
	return &Auth{
		cfg:      cfg,
		logf:     logf,
		secret:   secret,
		sessions: make(map[string]session),
		fails:    make(map[string]*failRecord),
	}
}

// Login 校验用户名密码并签发会话令牌。
func (a *Auth) Login(username, password, remote string) (string, error) {
	if locked, remain := a.isLocked(remote); locked {
		return "", fmt.Errorf("登录失败次数过多，请等待 %s 后重试", remain.Round(time.Second))
	}

	cfg := a.cfg()
	if username != cfg.Server.Username || password != cfg.Server.Password {
		a.recordFail(remote)
		a.logf("warn", "来自 %s 的登录失败（用户名 %q）", remote, username)
		return "", fmt.Errorf("用户名或密码错误")
	}

	a.clearFail(remote)

	token := a.signToken(username, remote)
	ttl := time.Duration(cfg.Server.SessionTTLHours) * time.Hour

	a.mu.Lock()
	a.sessions[token] = session{
		token:     token,
		expiresAt: time.Now().Add(ttl),
		remote:    remote,
	}
	a.mu.Unlock()

	a.logf("info", "用户 %s 从 %s 登录成功", username, remote)
	return token, nil
}

// Logout 注销指定令牌。
func (a *Auth) Logout(token string) {
	a.mu.Lock()
	delete(a.sessions, token)
	a.mu.Unlock()
}

// Verify 校验令牌是否有效。
func (a *Auth) Verify(token string) bool {
	if token == "" {
		return false
	}
	a.mu.RLock()
	s, ok := a.sessions[token]
	a.mu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().After(s.expiresAt) {
		a.mu.Lock()
		delete(a.sessions, token)
		a.mu.Unlock()
		return false
	}
	return true
}

// signToken 生成带时间戳的签名令牌。
func (a *Auth) signToken(username, remote string) string {
	ts := fmt.Sprintf("%d", time.Now().UnixNano())
	raw := username + "|" + remote + "|" + ts
	mac := hmac.New(sha256.New, a.secret)
	mac.Write([]byte(raw))
	sig := hex.EncodeToString(mac.Sum(nil))[:32]
	return base64.RawURLEncoding.EncodeToString([]byte(raw + "|" + sig))
}

// TokenFromRequest 从请求中提取令牌，支持 Header 与 Cookie 两种方式。
func TokenFromRequest(r *http.Request) string {
	if v := r.Header.Get("Authorization"); v != "" {
		if strings.HasPrefix(strings.ToLower(v), "bearer ") {
			return strings.TrimSpace(v[7:])
		}
		return strings.TrimSpace(v)
	}
	if c, err := r.Cookie("ghpp_token"); err == nil {
		return c.Value
	}
	return r.URL.Query().Get("token")
}

// isLocked 判断某来源是否处于登录锁定状态。
func (a *Auth) isLocked(remote string) (bool, time.Duration) {
	a.failsMu.Lock()
	defer a.failsMu.Unlock()
	rec := a.fails[remote]
	if rec == nil {
		return false, 0
	}
	if time.Now().Before(rec.lockUntil) {
		return true, time.Until(rec.lockUntil)
	}
	return false, 0
}

// recordFail 记录一次失败，达到阈值后锁定一段时间。
func (a *Auth) recordFail(remote string) {
	a.failsMu.Lock()
	defer a.failsMu.Unlock()

	rec := a.fails[remote]
	if rec == nil {
		rec = &failRecord{}
		a.fails[remote] = rec
	}
	rec.count++
	// 锁定策略：5 次失败锁 1 分钟，10 次锁 5 分钟，15 次以上锁 15 分钟。
	switch {
	case rec.count >= 15:
		rec.lockUntil = time.Now().Add(15 * time.Minute)
	case rec.count >= 10:
		rec.lockUntil = time.Now().Add(5 * time.Minute)
	case rec.count >= 5:
		rec.lockUntil = time.Now().Add(time.Minute)
	}
}

// clearFail 登录成功后清除失败记录。
func (a *Auth) clearFail(remote string) {
	a.failsMu.Lock()
	defer a.failsMu.Unlock()
	delete(a.fails, remote)
}

// Cleanup 定期清理过期会话与陈旧失败记录。
func (a *Auth) Cleanup() {
	now := time.Now()

	a.mu.Lock()
	for k, s := range a.sessions {
		if now.After(s.expiresAt) {
			delete(a.sessions, k)
		}
	}
	a.mu.Unlock()

	a.failsMu.Lock()
	for k, rec := range a.fails {
		// 锁定已过期且计数超过 1 小时后清零，避免长期累积误锁。
		if rec.count > 0 && now.After(rec.lockUntil) && now.Sub(rec.lockUntil) > time.Hour {
			delete(a.fails, k)
		}
	}
	a.failsMu.Unlock()
}

// writeJSON 以 JSON 形式返回响应。
// writeJSON 写出 JSON 响应。
//
// 这里先完整序列化再写，而不是直接把 Encoder 挂到 ResponseWriter 上：
// 一旦序列化中途失败（例如出现 Inf/NaN 这类非法浮点值），
// 状态行和响应头早已发出，客户端会收到一个 200 + 空 body 的"半成品"响应，
// 排查起来非常困难。先序列化就能在写头之前发现问题并给出可诊断的错误响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	buf, err := marshalJSON(v)
	if err != nil {
		// 序列化失败：此时还未写任何响应头，可以安全地改成 500。
		// 用标准库日志输出，因为 writeJSON 是包级函数，拿不到注入的 logger。
		log.Printf("api: 响应序列化失败: %v", err)
		status = http.StatusInternalServerError
		buf = []byte(`{"ok":false,"error":"服务器内部错误：响应序列化失败"}`)
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

// writeError 返回统一的错误响应。
func writeError(w http.ResponseWriter, status int, format string, args ...any) {
	writeJSON(w, status, map[string]any{
		"ok":    false,
		"error": fmt.Sprintf(format, args...),
	})
}

// writeOK 返回统一的成功响应。
func writeOK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"data": data,
	})
}

// decodeBody 解析请求体 JSON，带体积限制。
func decodeBody(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("请求体格式错误: %w", err)
	}
	return nil
}

// clientIP 提取客户端 IP，优先使用反向代理传递的头部。
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		return strings.TrimSpace(strings.Split(v, ",")[0])
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}
