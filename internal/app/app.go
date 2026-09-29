// Package app 负责把配置、镜像池、代理引擎、DNS 优选、决策器等模块编排成一个可运行的服务。
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ghpp/ghpp/internal/config"
	"github.com/ghpp/ghpp/internal/decide"
	"github.com/ghpp/ghpp/internal/dnsopt"
	"github.com/ghpp/ghpp/internal/hostsfile"
	"github.com/ghpp/ghpp/internal/logbus"
	"github.com/ghpp/ghpp/internal/mirror"
	"github.com/ghpp/ghpp/internal/netutil"
	"github.com/ghpp/ghpp/internal/proxy"
)

// App 是应用的总控制器。
//
// 它持有全部子系统，并驱动后台任务：
//   - 定期镜像测速
//   - 定期 DNS 优选与 hosts 刷新
//   - 定期自动决策
//   - 得分衰减与会话清理
type App struct {
	cfg *config.Config
	log *logbus.Bus

	pool     *mirror.Pool
	engine   *proxy.Engine
	optimize *dnsopt.Optimizer
	hostsMgr *hostsfile.Manager
	decider  *decide.Decider

	// ca 是 HTTPS 中间人所需的根证书，初始化失败时为 nil。
	ca    *proxy.CA
	caErr error

	mu      sync.RWMutex
	started bool

	cancel context.CancelFunc
	wg     sync.WaitGroup

	// 最近一次全量测速的结果，供决策与界面展示。
	lastProbes []mirror.Result

	// 最近一次 hosts 刷新时间。
	lastHostsSync time.Time
	// 最近一次自动决策时间。
	lastDecide time.Time

	// 当前实际生效的模式（可能是自动决策的产物，而非用户设定的模式）。
	effectiveMode config.Mode
	effectiveWhy  string
}

// New 创建应用。
func New(cfg *config.Config, log *logbus.Bus) *App {
	pool := mirror.NewPool()

	// 加载或生成根证书，用于可选的 HTTPS 中间人加速。
	// 失败不阻断启动，只是 HTTPS 网页加速不可用，代理与 DNS 优选照常工作。
	caDir := config.ResolveEtcDir(cfg.DataDir)
	ca, caErr := proxy.LoadOrCreateCA(caDir)
	if caErr != nil {
		log.Logf("warn", "根证书初始化失败，HTTPS 中间人加速不可用: %v", caErr)
	}

	a := &App{
		cfg:   cfg,
		log:   log,
		pool:  pool,
		ca:    ca,
		caErr: caErr,
	}

	// 日志总线同时作为各模块的日志出口。
	a.engine = proxy.NewEngine(proxy.Options{
		Config: func() *config.Config { return a.CurrentConfig() },
		Pool:   pool,
		Logf:   log.Logf,
		CA:     ca,
	})
	if ca != nil {
		// NewEngine 通过 Options 传入 CA，这里再显式同步一次，
		// 保证 caPtr 与 a.ca 始终指向同一份证书。
		a.engine.SetCA(ca)
	}
	a.optimize = dnsopt.NewOptimizer(log.Logf)
	a.hostsMgr = hostsfile.NewManager(cfg.Hosts.FilePath, log.Logf)
	a.decider = decide.New(pool, log.Logf)
	// 让决策器能拿到优选出的 IP，从而公平比较 hosts 通路的真实表现。
	a.decider.SetBestIPSource(func(host string) (string, bool) {
		c, ok := a.optimize.Best(host)
		if !ok || !c.OK {
			return "", false
		}
		return c.IP, true
	})
	a.effectiveMode = cfg.Mode

	return a
}

// CA 返回根证书，可能为 nil（初始化失败时）。
func (a *App) CA() *proxy.CA { return a.ca }

// CAError 返回根证书初始化时的错误。
func (a *App) CAError() error { return a.caErr }

// ReplaceCA 用新证书替换本地 CA 并热生效。
// mode 为 "import" 时使用用户提供的 PEM；为 "regenerate" 时重新生成。
func (a *App) ReplaceCA(mode string, certPEM, keyPEM []byte) error {
	caDir := config.ResolveEtcDir(a.CurrentConfig().DataDir)

	var (
		ca  *proxy.CA
		err error
	)
	switch mode {
	case "import":
		ca, err = proxy.ImportCA(caDir, certPEM, keyPEM)
	case "regenerate":
		ca, err = proxy.RegenerateCA(caDir)
	default:
		err = fmt.Errorf("不支持的操作: %s", mode)
	}
	if err != nil {
		return err
	}

	a.mu.Lock()
	a.ca = ca
	a.caErr = nil
	a.mu.Unlock()

	// 热替换，新连接立即使用新证书。
	a.engine.SetCA(ca)
	a.log.Logf("info", "根证书已更新（%s），客户端需重新安装并信任新证书", mode)
	return nil
}

// CurrentConfig 返回当前配置快照，供各模块读取。
func (a *App) CurrentConfig() *config.Config {
	return a.cfg.Snapshot()
}

// Config 返回底层配置对象，供 API 层执行写操作。
func (a *App) Config() *config.Config { return a.cfg }

// Log 返回日志总线。
func (a *App) Log() *logbus.Bus { return a.log }

// Pool 返回镜像源池。
func (a *App) Pool() *mirror.Pool { return a.pool }

// Engine 返回代理引擎。
func (a *App) Engine() *proxy.Engine { return a.engine }

// Optimizer 返回 DNS 优选器。
func (a *App) Optimizer() *dnsopt.Optimizer { return a.optimize }

// HostsManager 返回 hosts 管理器。
func (a *App) HostsManager() *hostsfile.Manager { return a.hostsMgr }

// Decider 返回决策器。
func (a *App) Decider() *decide.Decider { return a.decider }

// Start 启动全部子系统与后台任务。
func (a *App) Start() error {
	a.mu.Lock()
	if a.started {
		a.mu.Unlock()
		return errors.New("服务已在运行")
	}
	a.started = true
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.mu.Unlock()

	// 重置启动时刻，保证重启后运行时长从本次启动算起。
	startedAt = time.Now()

	cfg := a.CurrentConfig()

	// 启动代理监听。
	if err := a.engine.Start(cfg.Proxy.Listen); err != nil {
		return err
	}

	// 首轮测速与决策放到后台，避免阻塞启动。
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		// 稍作延迟，等待网络栈就绪。
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
		a.RefreshAll(ctx)
		a.loop(ctx)
	}()

	// 定期清理会话与陈旧统计。
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				a.pool.ScoreHalfLifeDecay(now)
			}
		}
	}()

	a.log.Logf("info", "GitHub++ 已启动：代理 %s，控制台 %s，模式 %s",
		cfg.Proxy.Listen, cfg.Server.Listen, cfg.Mode)
	return nil
}

// Stop 停止全部子系统。
func (a *App) Stop(ctx context.Context) error {
	a.mu.Lock()
	if !a.started {
		a.mu.Unlock()
		return nil
	}
	a.started = false
	cancel := a.cancel
	a.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	// 等待后台任务退出，但不超过上下文期限。
	done := make(chan struct{})
	go func() {
		a.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}

	// 停止时清理 hosts 托管区，避免留下失效的解析记录。
	if a.CurrentConfig().Hosts.Enabled {
		if err := a.hostsMgr.Clear(); err != nil {
			a.log.Logf("warn", "停止时清理 hosts 失败: %v", err)
		}
	}

	err := a.engine.Stop(ctx)
	a.log.Logf("info", "GitHub++ 已停止")
	return err
}

// Running 返回服务是否在运行。
func (a *App) Running() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.started
}

// loop 驱动周期性的后台任务。
func (a *App) loop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cfg := a.CurrentConfig()
			now := time.Now()

			// 镜像测速：按配置的评估间隔执行。
			interval := time.Duration(cfg.Auto.ProbeIntervalMinutes) * time.Minute
			if now.Sub(a.lastDecide) >= interval {
				a.RefreshAll(ctx)
			}

			// hosts 刷新。
			if cfg.Hosts.Enabled {
				hostsInterval := time.Duration(cfg.Hosts.RefreshMinutes) * time.Minute
				if now.Sub(a.lastHostsSync) >= hostsInterval {
					a.SyncHosts(ctx)
				}
			}
		}
	}
}

// RefreshAll 执行一轮完整的刷新：镜像测速 -> DNS 优选 -> 自动决策。
func (a *App) RefreshAll(ctx context.Context) {
	cfg := a.CurrentConfig()

	a.log.Logf("info", "开始全量刷新：镜像测速中…")
	probes := a.SweepMirrors(ctx)

	a.mu.Lock()
	a.lastProbes = probes
	a.mu.Unlock()

	// hosts 优选与代理测速相互独立，始终执行一次，
	// 这样决策器才能公平比较三条通路的真实表现。
	// 是否真正写入 hosts 由 Hosts.Enabled 决定。
	a.SyncHosts(ctx)

	// 仅在自动模式下才做通路评估，手动模式尊重用户选择。
	if cfg.Mode == config.ModeAuto {
		verdict := a.decider.Decide(ctx, cfg, probes)
		a.mu.Lock()
		a.effectiveMode = verdict.EffectiveMode
		a.effectiveWhy = verdict.Reason
		a.lastDecide = time.Now()
		a.mu.Unlock()
	} else {
		a.mu.Lock()
		a.effectiveMode = cfg.Mode
		a.effectiveWhy = "用户手动指定模式"
		a.lastDecide = time.Now()
		a.mu.Unlock()
	}
}

// SweepMirrors 对所有启用的镜像源执行一轮测速。
func (a *App) SweepMirrors(ctx context.Context) []mirror.Result {
	cfg := a.CurrentConfig()
	mirrors := cfg.EnabledMirrors()

	// 全量测速前清除冷却状态，让被摘除的源有机会回归。
	a.pool.ResetCooldown()

	targets := config.DomainProbeTargets()
	// 每个源只探测一个代表性目标即可，全量组合测速过慢。
	const maxBytes = 256 * 1024

	// Sweep 内部会根据源类型与目标分类自动匹配，无需在此预组合。
	results := a.pool.Sweep(ctx, mirrors, targets, maxBytes)

	okCount := 0
	for _, r := range results {
		if r.OK {
			okCount++
		}
	}
	a.log.Logf("info", "镜像测速完成：%d/%d 个源可用", okCount, len(results))
	return results
}

// SweepOne 只对指定源测速，用于界面上的单源测试按钮。
func (a *App) SweepOne(ctx context.Context, id string) ([]mirror.Result, error) {
	cfg := a.CurrentConfig()
	var target config.Mirror
	found := false
	for _, m := range cfg.Mirrors {
		if m.ID == id {
			target = m
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("未找到镜像源 %s", id)
	}

	targets := config.DomainProbeTargets()
	filtered := make([]config.ProbeTarget, 0, len(targets))
	for _, t := range targets {
		if mirror.Supports(target, t.Category) {
			filtered = append(filtered, t)
		}
	}
	if len(filtered) == 0 {
		filtered = targets
	}

	results := a.pool.Sweep(ctx, []config.Mirror{target}, filtered, 256*1024)
	return results, nil
}

// SyncHosts 执行 DNS 优选并把结果写入 hosts。
//
// 当用户尚未显式关闭 hosts 能力时（Enabled 为 true），本方法会完成
// 优选并写入系统 hosts 文件；若用户关闭了该开关，则跳过。
func (a *App) SyncHosts(ctx context.Context) {
	cfg := a.CurrentConfig()
	if !cfg.Hosts.Enabled {
		a.log.Logf("debug", "hosts 加速未启用，跳过同步")
		return
	}

	if err := ctx.Err(); err != nil {
		return
	}

	a.log.Logf("info", "开始 DNS 优选…")
	timeout := time.Duration(cfg.Hosts.ProbeTimeoutMS) * time.Millisecond
	best := a.optimize.OptimizeAll(ctx, cfg.Hosts.CandidateLimit, timeout)

	entries := make([]hostsfile.Entry, 0, len(best))
	for host, candidates := range best {
		if len(candidates) == 0 || !candidates[0].OK {
			a.log.Logf("debug", "域名 %s 无可用候选 IP", host)
			continue
		}
		top := candidates[0]
		entries = append(entries, hostsfile.Entry{
			IP:   top.IP,
			Host: host,
			Note: fmt.Sprintf("%s 延迟 %dms", host, top.TotalMS),
		})
	}

	if len(entries) == 0 {
		a.log.Logf("warn", "DNS 优选未获得可用结果，hosts 保持不变")
		return
	}

	// 检查写入权限，提前给出明确提示而不是等写入失败。
	if !a.hostsMgr.Writable() {
		a.log.Logf("warn", "没有权限写入 %s，DNS 优选结果仅用于展示。"+
			"请以 root 身份运行，或手动把结果添加到 hosts 文件。", a.hostsMgr.Path())
	}

	if err := a.hostsMgr.Apply(entries); err != nil {
		a.log.Logf("error", "写入 hosts 失败: %v", err)
		return
	}

	a.mu.Lock()
	a.lastHostsSync = time.Now()
	a.mu.Unlock()

	a.hostsMgr.FlushDNS()
}

// SyncDNSOnly 只执行 DNS 优选而不写入 hosts。
//
// 用于用户没有 hosts 写入权限的场景：结果仍可在界面上查看，
// 用户可手动采纳，或改用代理模式。
func (a *App) SyncDNSOnly(ctx context.Context) map[string][]dnsopt.Candidate {
	cfg := a.CurrentConfig()
	a.log.Logf("info", "开始 DNS 优选（仅探测，不写 hosts）…")
	timeout := time.Duration(cfg.Hosts.ProbeTimeoutMS) * time.Millisecond
	return a.optimize.OptimizeAll(ctx, cfg.Hosts.CandidateLimit, timeout)
}

// ClearHosts 清除 hosts 托管区。
func (a *App) ClearHosts() error {
	return a.hostsMgr.Clear()
}

// SetWatchdogAutoRestart 切换看门狗自动重启开关并立即同步标记文件。
//
// 配置写入 config.yaml 后，同步把 ${DataDir}/watchdog_enabled 改为 "1"/"0"，
// 这样看门狗在下次进程异常退出时立即按新值判断，无需重启 Go 进程。
func (a *App) SetWatchdogAutoRestart(enabled bool) error {
	if err := a.cfg.Update(func(c *config.Config) error {
		c.Watchdog.AutoRestart = enabled
		c.Watchdog.AutoRestartSet = true
		return nil
	}); err != nil {
		return err
	}
	return config.SyncWatchdogFlag(a.cfg.DataDir, enabled)
}

// Status 汇总当前运行状态，供 API 返回给前端。
type Status struct {
	Running       bool          `json:"running"`
	EffectiveMode config.Mode   `json:"effective_mode"`
	ConfigMode    config.Mode   `json:"config_mode"`
	Reason        string        `json:"reason"`
	ProxyAddr     string        `json:"proxy_addr"`
	Uptime        string        `json:"uptime"`
	LastSweep     time.Time     `json:"last_sweep"`
	LastHostsSync time.Time     `json:"last_hosts_sync"`
	LastDecide    time.Time     `json:"last_decide"`
	HostsFile     string        `json:"hosts_file"`
	HostsWritable bool          `json:"hosts_writable"`
	Metrics       MetricsStatus `json:"metrics"`
	Mirrors       MirrorBrief   `json:"mirrors"`
	// LocalIPs 是本机所有可用于局域网通信的 IPv4，供前端「接入方式」切换 LAN 地址。
	LocalIPs []string `json:"local_ips"`
	// ExternalHost 是用户保存的外网接入地址（域名或 IP，可带端口），空表示未配置。
	ExternalHost string `json:"external_host"`
	// SystemProxyEnabled 表示是否已开启系统级 HTTP 代理（写 /etc/profile.d/ghpp-proxy.sh）。
	// 开启后飞牛本机新 login shell 启动的软件默认走加速器，无需软件自身配置。
	SystemProxyEnabled bool `json:"system_proxy_enabled"`
	// HostsEnabled 表示 hosts 加速是否已开启（cfg.Hosts.Enabled），
	// 前端「应用加速」页据此展示状态徽章。
	HostsEnabled bool `json:"hosts_enabled"`
	// DockerEnabled 表示 Docker 加速是否已开启（cfg.Docker.Enabled），
	// 前端「应用加速」页据此展示状态徽章。
	DockerEnabled bool `json:"docker_enabled"`
}

// MetricsStatus 是流量与命中指标。
type MetricsStatus struct {
	TotalRequests int64 `json:"total_requests"`
	Accelerated   int64 `json:"accelerated"`
	Failed        int64 `json:"failed"`
	Failovers     int64 `json:"failovers"`
	BytesIn       int64 `json:"bytes_in"`
	BytesOut      int64 `json:"bytes_out"`
	SavedMS       int64 `json:"saved_ms"`
	WebCount      int64 `json:"web_count"`
	RawCount      int64 `json:"raw_count"`
	CloneCount    int64 `json:"clone_count"`
	DockerCount   int64 `json:"docker_count"`
	// SavedMB 是节省流量的可读形式，单位 MB。
	SavedMB float64 `json:"saved_mb"`
}

// MirrorBrief 是镜像源的汇总信息。
type MirrorBrief struct {
	Total     int    `json:"total"`
	Enabled   int    `json:"enabled"`
	Healthy   int    `json:"healthy"`
	BestID    string `json:"best_id"`
	BestName  string `json:"best_name"`
	BestLatMS int64  `json:"best_latency_ms"`
}

// startedAt 记录启动时刻，用于计算运行时长。
var startedAt = time.Now()

// Status 构造当前状态快照。
func (a *App) Status() Status {
	cfg := a.CurrentConfig()
	m := a.engine.Metrics()

	a.mu.RLock()
	effMode := a.effectiveMode
	reason := a.effectiveWhy
	lastSweep := time.Time{}
	lastHosts := a.lastHostsSync
	lastDecide := a.lastDecide
	probes := a.lastProbes
	a.mu.RUnlock()

	if len(probes) > 0 {
		lastSweep = a.pool.LastSweep()
	}

	stats := a.pool.Stats()

	brief := MirrorBrief{Total: len(cfg.Mirrors)}
	var bestLat int64
	for _, m := range cfg.Mirrors {
		if !m.Enabled {
			continue
		}
		brief.Enabled++
		st, ok := stats[m.ID]
		if ok && st.OK {
			brief.Healthy++
			if bestLat == 0 || (st.LatencyMS > 0 && st.LatencyMS < bestLat) {
				bestLat = st.LatencyMS
				brief.BestID = m.ID
				brief.BestName = m.Name
			}
		}
	}
	brief.BestLatMS = bestLat

	if effMode == "" {
		effMode = cfg.Mode
	}

	acc := m.Accelerated.Load()
	total := m.TotalRequests.Load()

	return Status{
		Running:       a.Running(),
		EffectiveMode: effMode,
		ConfigMode:    cfg.Mode,
		Reason:        reason,
		ProxyAddr:     a.engine.Addr(),
		Uptime:        time.Since(startedAt).Round(time.Second).String(),
		LastSweep:     lastSweep,
		LastHostsSync: lastHosts,
		LastDecide:    lastDecide,
		HostsFile:     a.hostsMgr.Path(),
		HostsWritable: a.hostsMgr.Writable(),
		Metrics: MetricsStatus{
			TotalRequests: total,
			Accelerated:   acc,
			Failed:        m.Failed.Load(),
			Failovers:     m.Failovers.Load(),
			BytesIn:       m.BytesIn.Load(),
			BytesOut:      m.BytesOut.Load(),
			SavedMS:       m.SavedMS.Load(),
			WebCount:      m.CategoryWeb.Load(),
			RawCount:      m.CategoryRaw.Load(),
			CloneCount:    m.CategoryClone.Load(),
			DockerCount:   m.CategoryDocker.Load(),
			SavedMB:       float64(m.BytesOut.Load()) / 1024 / 1024,
		},
		Mirrors:      brief,
		LocalIPs:     netutil.LocalIPs(),
		ExternalHost: a.cfg.External.Host,
		// 系统代理实际状态以文件是否存在为准（不依赖 config 字段，避免配置与文件不一致时误报）。
		SystemProxyEnabled: systemProxyFileExists(),
		HostsEnabled:       a.cfg.Hosts.Enabled,
		DockerEnabled:      a.cfg.Docker.Enabled,
	}
}

// SetExternalHost 保存用户配置的外网接入地址（域名或 IP，可带端口）。
//
// 写入 config.yaml 的 external.host 字段，前端「接入方式」切换到「外网」时
// 会用此地址生成接入示例。空串表示清空，下次切换外网模式前端会引导重新输入。
func (a *App) SetExternalHost(host string) error {
	return a.cfg.Update(func(c *config.Config) error {
		c.External.Host = strings.TrimSpace(host)
		return nil
	})
}

// SetSystemProxy 开启或关闭系统级 HTTP 代理。
//
// 开启：往 /etc/profile.d/ghpp-proxy.sh 写入 http_proxy / https_proxy 指向
// 127.0.0.1:<代理端口>，让飞牛本机所有新 login shell 启动的软件默认走加速器，
// 无需软件自身支持配置代理。关闭：删除该文件。
//
// 同步把状态写入 config.yaml 的 system_proxy.enabled，便于启动期与前端展示。
// 已运行的进程不受影响，需重启对应软件才生效——UI 必须给出提示。
func (a *App) SetSystemProxy(enabled bool) error {
	if err := a.cfg.Update(func(c *config.Config) error {
		c.SystemProxy.Enabled = enabled
		c.SystemProxy.EnabledSet = true
		return nil
	}); err != nil {
		return err
	}
	return syncSystemProxyFile(enabled, a.cfg.Proxy.Listen)
}

// systemProxyFilePath 是系统级 HTTP 代理环境变量注入文件的固定路径。
//
// 选 /etc/profile.d/ 是因为飞牛基于 Debian，所有 login shell（含 SSH 登录、
// systemd 服务里 source 了 profile 的）都会自动 source 该目录下 .sh 文件，
// 不需要改 ~/.bashrc 或单个软件配置。
const systemProxyFilePath = "/etc/profile.d/ghpp-proxy.sh"

// syncSystemProxyFile 按开关状态创建或删除 /etc/profile.d/ghpp-proxy.sh。
//
// 文件内容导出 http_proxy/https_proxy/HTTP_PROXY/HTTPS_PROXY 与 no_proxy，
// no_proxy 排除本机与私网段，避免 NAS 内部通信也走代理。
// 写入失败（权限不足或 /etc/profile.d 不存在）时返回错误，由 API 层提示用户。
func syncSystemProxyFile(enabled bool, proxyListen string) error {
	if !enabled {
		// 关闭：删除文件。文件不存在视为成功，避免首次关闭报错。
		err := os.Remove(systemProxyFilePath)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除系统代理文件失败：%w（请确认加速器以 root 运行）", err)
		}
		return nil
	}

	// 端口取自代理服务的实际监听地址；异常时回退 7710。
	port := "7710"
	if _, p, err := net.SplitHostPort(proxyListen); err == nil && p != "" {
		port = p
	}

	// NAS 本机 shell 走 loopback，地址固定 127.0.0.1。
	proxyURL := "http://127.0.0.1:" + port
	// no_proxy 排除本机与私网段，避免 NAS 内部通信（emby/jellyfin 等本地服务）也走代理。
	noProxy := "localhost,127.0.0.1,::1,192.168.0.0/16,10.0.0.0/8,172.16.0.0/12,*.local"

	content := "# GitHub++ 加速器系统代理（自动生成，请勿手动编辑）\n" +
		"# 开启后所有新 login shell 默认走加速器；仅对 GitHub 相关域名加速，\n" +
		"# 其他流量透明转发不影响正常联网。关闭请到 GitHub++ 控制台「应用加速」页。\n" +
		"export http_proxy=" + proxyURL + "\n" +
		"export https_proxy=" + proxyURL + "\n" +
		"export HTTP_PROXY=" + proxyURL + "\n" +
		"export HTTPS_PROXY=" + proxyURL + "\n" +
		"export no_proxy=" + noProxy + "\n" +
		"export NO_PROXY=" + noProxy + "\n"

	// 写入临时文件再原子重命名，避免半写入态被 shell 读到。
	tmp := systemProxyFilePath + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0644); err != nil {
		return fmt.Errorf("写入系统代理文件失败：%w（请确认加速器以 root 运行且 /etc/profile.d 可写）", err)
	}
	return os.Rename(tmp, systemProxyFilePath)
}

// systemProxyFileExists 判断系统级 HTTP 代理是否已开启（文件存在即视为开启）。
//
// 用文件存在性而非 config.SystemProxy.Enabled，避免配置与实际文件不一致时误报。
func systemProxyFileExists() bool {
	_, err := os.Stat(systemProxyFilePath)
	return err == nil
}

// Metrics 返回指标状态，供界面轮询。
func (a *App) Metrics() MetricsStatus {
	return a.Status().Metrics
}
