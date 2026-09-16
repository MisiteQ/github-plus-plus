package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ghpp/ghpp/internal/app"
	"github.com/ghpp/ghpp/internal/config"
)

// Server 是控制台的 HTTP 服务。
type Server struct {
	app    *app.App
	auth   *Auth
	logf   func(level, format string, args ...any)
	assets http.Handler

	httpSrv *http.Server
}

// NewServer 创建 API 服务。
func NewServer(a *app.App, assets http.Handler) *Server {
	s := &Server{
		app:    a,
		auth:   NewAuth(a.CurrentConfig, a.Log().Logf),
		logf:   a.Log().Logf,
		assets: assets,
	}
	return s
}

// Handler 返回配置好的路由处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// ---- 公开接口 ----
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/health", s.handleHealth)

	// ---- 需要认证的接口 ----
	mux.HandleFunc("/api/status", s.guard(s.handleStatus))
	mux.HandleFunc("/api/config", s.guard(s.handleConfig))
	mux.HandleFunc("/api/mode", s.guard(s.handleMode))
	mux.HandleFunc("/api/mirrors", s.guard(s.handleMirrors))
	mux.HandleFunc("/api/mirrors/test", s.guard(s.handleMirrorTest))
	mux.HandleFunc("/api/mirrors/test-all", s.guard(s.handleMirrorTestAll))
	mux.HandleFunc("/api/hosts", s.guard(s.handleHosts))
	mux.HandleFunc("/api/dns", s.guard(s.handleDNS))
	mux.HandleFunc("/api/logs", s.guard(s.handleLogs))
	mux.HandleFunc("/api/logs/stream", s.guard(s.handleLogStream))
	mux.HandleFunc("/api/password", s.guard(s.handlePassword))
	mux.HandleFunc("/api/service", s.guard(s.handleService))
	mux.HandleFunc("/api/cert", s.guard(s.handleCert))
	mux.HandleFunc("/api/cert/", s.guard(s.handleCert))
	mux.HandleFunc("/api/docker", s.guard(s.handleDocker))
	mux.HandleFunc("/api/about", s.guard(s.handleAbout))

	// ---- 静态资源：未匹配的路径交给前端 ----
	mux.Handle("/", s.assets)

	return s.withMiddleware(mux)
}

// withMiddleware 添加通用中间件：日志、安全头、CORS。
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// 安全响应头。
		// 注意：不设置 X-Frame-Options / CSP frame-ancestors，
		// 因为控制台需要在飞牛桌面（不同端口）的 iframe 中嵌入展示。
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")

		// 控制台自身接口不允许跨站调用，防止 CSRF。
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}

		next.ServeHTTP(w, r)

		// 只记录 API 调用，避免静态资源刷屏。
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/logs/stream" {
			s.logf("debug", "%s %s %s 耗时 %s", clientIP(r), r.Method, r.URL.Path,
				time.Since(start).Round(time.Millisecond))
		}
	})
}

// guard 包装需要认证的处理器。
func (s *Server) guard(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := TokenFromRequest(r)
		if !s.auth.Verify(token) {
			writeError(w, http.StatusUnauthorized, "未登录或会话已过期")
			return
		}
		fn(w, r)
	}
}

// handleLogin 处理登录请求。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}

	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}

	token, err := s.auth.Login(req.Username, req.Password, clientIP(r))
	if err != nil {
		writeError(w, http.StatusUnauthorized, "%v", err)
		return
	}

	// 同时下发 Cookie，便于浏览器端直接使用。
	cfg := s.app.CurrentConfig()
	http.SetCookie(w, &http.Cookie{
		Name:     "ghpp_token",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   cfg.Server.SessionTTLHours * 3600,
	})

	writeOK(w, map[string]any{"token": token, "username": req.Username})
}

// handleLogout 处理登出请求。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(TokenFromRequest(r))
	http.SetCookie(w, &http.Cookie{
		Name:   "ghpp_token",
		Value:  "",
		Path:   "/",
		MaxAge: -1,
	})
	writeOK(w, nil)
}

// handleHealth 是无需认证的健康检查，供飞牛应用中心探活。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeOK(w, map[string]any{
		"running": s.app.Running(),
		"version": Version,
	})
}

// handleStatus 返回完整运行状态。
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	st := s.app.Status()
	verdict := s.app.Decider().Verdict()

	writeOK(w, map[string]any{
		"status":  st,
		"verdict": verdict,
		"version": Version,
	})
}

// handleConfig 读取或更新完整配置。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.app.CurrentConfig()
		writeOK(w, cfg)

	case http.MethodPut, http.MethodPost:
		var incoming config.Config
		if err := decodeBody(r, &incoming); err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}
		if !incoming.Mode.IsValid() {
			writeError(w, http.StatusBadRequest, "无效的模式: %s", incoming.Mode)
			return
		}

		err := s.app.Config().Update(func(c *config.Config) error {
			// 只允许修改可安全热更新的字段。
			c.Mode = incoming.Mode
			c.Auto = incoming.Auto
			// 监听地址修改后需重启代理服务才能生效。
			if incoming.Proxy.Listen != "" {
				c.Proxy.Listen = incoming.Proxy.Listen
			}
			c.Proxy.ConnectTimeoutMS = incoming.Proxy.ConnectTimeoutMS
			c.Proxy.ReadTimeoutMS = incoming.Proxy.ReadTimeoutMS
			c.Proxy.FailoverThreshold = incoming.Proxy.FailoverThreshold
			c.Proxy.CooldownSeconds = incoming.Proxy.CooldownSeconds
			c.Hosts.RefreshMinutes = incoming.Hosts.RefreshMinutes
			c.Hosts.CandidateLimit = incoming.Hosts.CandidateLimit
			c.Hosts.ProbeTimeoutMS = incoming.Hosts.ProbeTimeoutMS
			return nil
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "保存配置失败: %v", err)
			return
		}

		s.logf("info", "配置已更新，模式=%s", incoming.Mode)
		writeOK(w, s.app.CurrentConfig())

	default:
		writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

// handleMode 快速切换加速模式。
func (s *Server) handleMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}

	var req struct {
		Mode config.Mode `json:"mode"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if !req.Mode.IsValid() {
		writeError(w, http.StatusBadRequest, "无效的模式: %s", req.Mode)
		return
	}

	if err := s.app.Config().Update(func(c *config.Config) error {
		c.Mode = req.Mode
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "切换模式失败: %v", err)
		return
	}

	// 切换到 host 模式时立即同步一次，让效果立即可见。
	if req.Mode == config.ModeHosts {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			s.app.SyncHosts(ctx)
		}()
	}

	s.logf("info", "加速模式已切换为 %s", req.Mode)
	writeOK(w, map[string]any{"mode": req.Mode})
}

// handleMirrors 管理镜像源列表。
func (s *Server) handleMirrors(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.app.CurrentConfig()
		stats := s.app.Pool().Stats()

		type item struct {
			config.Mirror
			Stat any `json:"stat,omitempty"`
		}
		items := make([]item, 0, len(cfg.Mirrors))
		for _, m := range cfg.Mirrors {
			it := item{Mirror: m}
			if st, ok := stats[m.ID]; ok {
				it.Stat = st
			}
			items = append(items, it)
		}
		writeOK(w, items)

	case http.MethodPost:
		// 新增镜像源。
		var m config.Mirror
		if err := decodeBody(r, &m); err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}
		if m.URL == "" {
			writeError(w, http.StatusBadRequest, "镜像源地址不能为空")
			return
		}
		if !strings.HasPrefix(m.URL, "http://") && !strings.HasPrefix(m.URL, "https://") {
			writeError(w, http.StatusBadRequest, "镜像源地址必须以 http:// 或 https:// 开头")
			return
		}
		if m.ID == "" {
			m.ID = "custom-" + config.RandomToken(4)
		}
		if m.Kind == "" {
			m.Kind = config.KindPrefix
		}
		if m.Name == "" {
			m.Name = m.URL
		}
		m.Enabled = true
		if m.Weight == 0 {
			m.Weight = 1
		}

		if err := s.app.Config().Update(func(c *config.Config) error {
			for _, existing := range c.Mirrors {
				if existing.ID == m.ID {
					return fmt.Errorf("镜像源 ID %s 已存在", m.ID)
				}
			}
			c.Mirrors = append(c.Mirrors, m)
			return nil
		}); err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}

		s.logf("info", "新增镜像源 %s (%s)", m.Name, m.URL)
		writeOK(w, m)

	case http.MethodPut:
		// 修改镜像源。
		var m config.Mirror
		if err := decodeBody(r, &m); err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}
		if m.ID == "" {
			writeError(w, http.StatusBadRequest, "缺少镜像源 ID")
			return
		}

		if err := s.app.Config().Update(func(c *config.Config) error {
			for i := range c.Mirrors {
				if c.Mirrors[i].ID != m.ID {
					continue
				}
				if m.Name != "" {
					c.Mirrors[i].Name = m.Name
				}
				if m.URL != "" {
					c.Mirrors[i].URL = m.URL
				}
				if m.Kind != "" {
					c.Mirrors[i].Kind = m.Kind
				}
				if m.Note != "" {
					c.Mirrors[i].Note = m.Note
				}
				c.Mirrors[i].Enabled = m.Enabled
				if m.Weight > 0 {
					c.Mirrors[i].Weight = m.Weight
				}
				return nil
			}
			return fmt.Errorf("未找到镜像源 %s", m.ID)
		}); err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}
		writeOK(w, m)

	case http.MethodDelete:
		id := r.URL.Query().Get("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "缺少 id 参数")
			return
		}

		if err := s.app.Config().Update(func(c *config.Config) error {
			out := c.Mirrors[:0]
			found := false
			for _, m := range c.Mirrors {
				if m.ID == id {
					found = true
					continue
				}
				out = append(out, m)
			}
			if !found {
				return fmt.Errorf("未找到镜像源 %s", id)
			}
			c.Mirrors = out
			return nil
		}); err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}

		s.logf("info", "已删除镜像源 %s", id)
		writeOK(w, map[string]string{"id": id})

	default:
		writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

// handleMirrorTest 对单个镜像源测速。
func (s *Server) handleMirrorTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "缺少 id 参数")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	results, err := s.app.SweepOne(ctx, id)
	if err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	writeOK(w, results)
}

// handleMirrorTestAll 对所有镜像源测速。
func (s *Server) handleMirrorTestAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	// 全量测速比较耗时，放到后台执行，接口立即返回。
	go func() {
		s.app.RefreshAll(ctx)
	}()

	writeOK(w, map[string]string{"message": "测速已在后台开始，稍后刷新查看结果"})
}

// handleHosts 管理 hosts 相关操作。
func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		entries, err := s.app.HostsManager().Current()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "%v", err)
			return
		}
		best := s.app.Optimizer().BestAll()
		writeOK(w, map[string]any{
			"enabled":    s.app.CurrentConfig().Hosts.Enabled,
			"file":       s.app.HostsManager().Path(),
			"writable":   s.app.HostsManager().Writable(),
			"has_backup": s.app.HostsManager().HasBackup(),
			"entries":    entries,
			"best":       best,
		})

	case http.MethodPost:
		// 立即执行一次 DNS 优选并写入 hosts。
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
		defer cancel()

		go func() {
			s.app.SyncHosts(ctx)
		}()
		writeOK(w, map[string]string{"message": "DNS 优选已在后台开始"})

	case http.MethodDelete:
		if err := s.app.ClearHosts(); err != nil {
			writeError(w, http.StatusInternalServerError, "清理 hosts 失败: %v", err)
			return
		}
		writeOK(w, nil)

	case http.MethodPut:
		// 开关 hosts 加速。
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := decodeBody(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}

		if err := s.app.Config().Update(func(c *config.Config) error {
			c.Hosts.Enabled = req.Enabled
			// 标记为用户显式设置，避免后续被默认值迁移覆盖。
			c.Hosts.EnabledSet = true
			return nil
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "%v", err)
			return
		}

		if !req.Enabled {
			// 关闭时清理托管区，避免残留无效解析。
			if err := s.app.ClearHosts(); err != nil {
				s.logf("warn", "清理 hosts 失败: %v", err)
			}
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			go s.app.SyncHosts(ctx)
		}

		writeOK(w, map[string]bool{"enabled": req.Enabled})

	default:
		writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

// handleDNS 返回 DNS 优选的候选与结果。
func (s *Server) handleDNS(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	if host != "" {
		best, ok := s.app.Optimizer().Best(host)
		writeOK(w, map[string]any{
			"host":       host,
			"best":       best,
			"has_best":   ok,
			"candidates": s.app.Optimizer().Candidates(host),
		})
		return
	}

	writeOK(w, map[string]any{
		"best":  s.app.Optimizer().BestAll(),
		"hosts": config.GitHubHosts(),
	})
}

// handleLogs 返回历史日志。
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}
	level := r.URL.Query().Get("level")
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			after = n
		}
	}

	entries := s.app.Log().Recent(limit, level, after)
	writeOK(w, entries)
}

// handleLogStream 以 Server-Sent Events 推送实时日志。
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "当前服务器不支持流式响应")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// 先补发最近 30 条，让用户一进页面就能看到上下文。
	for _, e := range s.app.Log().Recent(30, "", 0) {
		writeSSE(w, e)
	}
	flusher.Flush()

	ch, cancel := s.app.Log().Subscribe()
	defer cancel()

	// 心跳定时器，避免中间设备因长时间无数据而断开连接。
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		case e, ok := <-ch:
			if !ok {
				return
			}
			writeSSE(w, e)
			flusher.Flush()
		}
	}
}

// handlePassword 修改控制台密码。
func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}

	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}
	if len(req.NewPassword) < 6 {
		writeError(w, http.StatusBadRequest, "新密码长度至少 6 位")
		return
	}

	cfg := s.app.CurrentConfig()
	if req.OldPassword != cfg.Server.Password {
		writeError(w, http.StatusUnauthorized, "原密码错误")
		return
	}

	if err := s.app.Config().Update(func(c *config.Config) error {
		c.Server.Password = req.NewPassword
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "保存密码失败: %v", err)
		return
	}

	s.logf("info", "控制台密码已修改")
	writeOK(w, nil)
}

// handleService 控制代理服务的启停。
func (s *Server) handleService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "仅支持 POST")
		return
	}

	var req struct {
		Action string `json:"action"`
	}
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "%v", err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	switch req.Action {
	case "start":
		if s.app.Running() {
			writeOK(w, map[string]string{"state": "running"})
			return
		}
		if err := s.app.Start(); err != nil {
			writeError(w, http.StatusInternalServerError, "启动失败: %v", err)
			return
		}
		writeOK(w, map[string]string{"state": "running"})

	case "stop":
		if !s.app.Running() {
			writeOK(w, map[string]string{"state": "stopped"})
			return
		}
		if err := s.app.Stop(ctx); err != nil && !errors.Is(err, context.Canceled) {
			writeError(w, http.StatusInternalServerError, "停止失败: %v", err)
			return
		}
		writeOK(w, map[string]string{"state": "stopped"})

	case "restart":
		if s.app.Running() {
			_ = s.app.Stop(ctx)
		}
		if err := s.app.Start(); err != nil {
			writeError(w, http.StatusInternalServerError, "重启失败: %v", err)
			return
		}
		writeOK(w, map[string]string{"state": "running"})

	case "refresh":
		go func() {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			s.app.RefreshAll(c)
		}()
		writeOK(w, map[string]string{"state": "refreshing"})

	default:
		writeError(w, http.StatusBadRequest, "未知操作: %s", req.Action)
	}
}

// writeSSE 以 SSE 格式写出一条日志。
func writeSSE(w http.ResponseWriter, e any) {
	data, err := marshalJSON(e)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
}

// Start 启动控制台监听。
func (s *Server) Start(addr string) error {
	addr = config.JoinHostPort(addr)
	s.httpSrv = &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// 会话清理任务。
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			s.auth.Cleanup()
		}
	}()

	return s.httpSrv.ListenAndServe()
}

// Stop 关闭控制台。
func (s *Server) Stop(ctx context.Context) error {
	if s.httpSrv == nil {
		return nil
	}
	return s.httpSrv.Shutdown(ctx)
}
