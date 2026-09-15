// Package proxy 实现 GitHub 加速的核心反向代理引擎。
//
// 工作流程：
//
//	客户端请求 -> 识别 Host 是否为 GitHub 域名 -> 判定请求类别
//	  -> 由调度器选出最优镜像源 -> 改写目标 URL -> 流式转发
//	  -> 记录真实性能数据反馈给调度器 -> 失败则换源重试
//
// 引擎同时支持两种接入形态：
//   - 显式代理：客户端把本服务设为 HTTP 代理，请求行中带完整 URL；
//   - 透明反代：客户端通过 hosts 把 GitHub 域名指向本机，Host 头即域名。
package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ghpp/ghpp/internal/config"
	"github.com/ghpp/ghpp/internal/mirror"
)

// Metrics 汇总代理服务的运行指标。
type Metrics struct {
	// TotalRequests 是累计处理请求数。
	TotalRequests atomic.Int64
	// Accelerated 是其中走了加速通道的请求数。
	Accelerated atomic.Int64
	// Failed 是最终失败的请求数。
	Failed atomic.Int64
	// BytesIn 是从客户端接收的字节数。
	BytesIn atomic.Int64
	// BytesOut 是发往客户端的字节数。
	BytesOut atomic.Int64
	// SavedMS 估算的累计节省耗时毫秒数。
	SavedMS atomic.Int64
	// Failovers 是触发换源的次数。
	Failovers atomic.Int64

	// 分类计数。
	CategoryWeb    atomic.Int64
	CategoryRaw    atomic.Int64
	CategoryClone  atomic.Int64
	CategoryDocker atomic.Int64
}

// Engine 是反向代理引擎。
type Engine struct {
	cfg   func() *config.Config
	pool  *mirror.Pool
	hosts map[string]config.GHHost

	// client 用于镜像中转转发，镜像回源偶尔较慢，响应头超时给得较宽松。
	client *http.Client
	// directClient 专用于"直连优先"域名的试探性直连。
	// 它把"等待响应头"限制在 directHeaderTimeout 内（不限制响应体流式传输），
	// 以便在 SNI 被 QoS 限速的网络里快速识别"通但极慢"的直连并回退镜像。
	directClient *http.Client
	transport    *http.Transport

	// ca 用于 HTTPS 中间人解密。为 nil 时退化为纯 TCP 透传。
	// 通过 caPtr 原子持有，支持运行期热替换（导入自定义证书后无需重启）。
	caPtr atomic.Pointer[CA]

	metrics Metrics

	// logf 用于输出结构化日志，由上层注入。
	logf func(level, format string, args ...any)

	// sem 限制并发在途连接数，防止 NAS 资源被耗尽。
	sem chan struct{}

	// directCB 记录每个"直连优先"域名的直连熔断状态。
	//
	// 背景：部分网络对 GitHub 做 SNI 阻断，TCP 能连上但 TLS/HTTP 被掐，
	// 表现为直连必然拨号/握手超时。若每个请求都先试直连再回退镜像，
	// 用户每次都要白等约一个拨号超时。熔断后在冷却期内直接走镜像，
	// 冷却到期再放试探请求，网络恢复即可自动回切。
	directCB   map[string]*directBreaker
	directCBmu sync.Mutex

	server   *http.Server
	listener net.Listener

	mu      sync.RWMutex
	running bool
}

// 直连熔断参数。
const (
	// directFailThreshold 是连续失败多少次后判定直连不可用。
	directFailThreshold = 2
	// directCooldown 是直连被判不可用后跳过直连的时长。
	directCooldown = 60 * time.Second
	// directHeaderTimeout 是直连试探时等待上游响应头的最长时间。
	//
	// 在对 GitHub 做 SNI 阻断/QoS 限速的网络里，TCP 与 TLS 往往能勉强建立，
	// 但首字节被拖到十几秒。用该超时从传输层强制判定：超过它还没拿到响应头，
	// 这次直连即判失败并回退镜像（响应体的流式传输不受此值限制）。
	// 取值兼顾正常但偏重的 API 计算（通常 <2s）与阻断网络的快速失败。
	directHeaderTimeout = 5 * time.Second

	// probeBytes / probeTimeout 是写响应给客户端前的"链路探针"。
	//
	// 仅看响应头或前几 KB 无法识别 QoS 限速：被限速的链路往往先放行一个突发
	// 小包（响应头、前几 KB 秒到），随后把带宽掐到十几 KB/s，使整个下载拖到几十秒。
	// 因此在向客户端写出任何字节前，先限时读满 probeBytes（不足则读到 EOF），
	// 读不满即判定链路劣质并换通道/换源；达标后再无超时地流式转发剩余体（支持大文件）。
	// 取值 512KB（大于常见 git advertisement 的 ~348KB）：小响应必须在限时内
	// "给完整响应"才算通过，大响应则隐含约 100KB/s 的突发吞吐门槛，
	// 正常镜像（数百 KB/s 起）仍在亚秒级通过。
	probeBytes   = 512 << 10
	probeTimeout = 5 * time.Second

	// directSlowMinDuration / directMinThroughput 是直连"慢成功"的兜底判定。
	//
	// 探针可能只换来一段突发带宽（前 512KB 快、响应提交后剩余部分被掐），此时已
	// 无法换源，于是用端到端平均吞吐给本次尝试定论：耗时足够长且平均吞吐低于门槛
	// 即判劣质，立即熔断让后续请求改走镜像。用吞吐而非总耗时，可避免误伤大文件的
	// 健康直连（其吞吐通常在 MB/s 级）。
	directSlowMinDuration = 2 * time.Second
	directMinThroughput   = 100 << 10 // 100 KB/s

	// warmupDelay 是服务启动后到发起直连预热探测之间的等待，
	// 让监听与网络栈先就绪，避免开机自启瞬间网络未就绪造成误判。
	warmupDelay = 5 * time.Second
)

// errProbeTimeout 表示上游在限定时间内未能读出足够的探针数据。
var errProbeTimeout = errors.New("上游链路探针超时（响应体疑似被限速）")

// readProbe 限时读满 probeBytes（或读到 EOF），用于在写客户端前验证上游真实吞吐。
//
// 返回已读到的字节（小响应可能不足 probeBytes）。超时后调用方必须立即关闭
// resp.Body 以中断内部阻塞的 goroutine（channel 带缓冲，不会泄漏）。
func readProbe(r io.Reader, timeout time.Duration) (buf []byte, err error) {
	buf = make([]byte, probeBytes)
	type result struct {
		n int
	}
	ch := make(chan result, 1)
	go func() {
		// ReadFull 读满返回；遇 EOF 提前返回（io.ErrUnexpectedEOF 或 EOF），
		// 两种情况都表示"在限时内拿到了上游愿意给出的全部/足够数据"。
		n, _ := io.ReadFull(r, buf)
		ch <- result{n: n}
	}()
	select {
	case x := <-ch:
		return buf[:x.n], nil
	case <-time.After(timeout):
		return nil, errProbeTimeout
	}
}

// directBreaker 是单个域名的直连熔断状态。
type directBreaker struct {
	fails int
	until time.Time
	// halfOpen 表示后台探测已通过、等待一次真实请求做最终验证。
	// 该令牌在锁内被首个请求原子取走，保证并发下最多只有一个请求走直连试探。
	halfOpen bool
	// timer 是冷却到期后发起"后台直连探测"的定时器。
	// 恢复探测放在后台而非让下一个用户请求承担，可避免冷却到期后
	// 第一个真实请求被拖慢一个拨号超时。
	timer *time.Timer
}

// Options 是构造引擎时的可选项。
type Options struct {
	Config    func() *config.Config
	Pool      *mirror.Pool
	Logf      func(level, format string, args ...any)
	CA        *CA
	MaxFlight int
}

// NewEngine 创建反向代理引擎。
func NewEngine(opt Options) *Engine {
	if opt.MaxFlight <= 0 {
		opt.MaxFlight = 512
	}
	if opt.Logf == nil {
		opt.Logf = func(string, string, ...any) {}
	}

	transport := &http.Transport{
		// 上游请求不走系统代理，避免与代理自身形成环路。
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			// 拨号超时跟随用户配置，默认 8 秒。
			// 在直连被阻断的网络里，更短的超时能让失败更快暴露给熔断机制。
			timeout := 8 * time.Second
			if c := opt.Config(); c != nil && c.Proxy.ConnectTimeoutMS > 0 {
				timeout = time.Duration(c.Proxy.ConnectTimeoutMS) * time.Millisecond
			}
			d := &net.Dialer{Timeout: timeout, KeepAlive: 60 * time.Second}
			return d.DialContext(ctx, network, addr)
		},
		MaxIdleConns:        400,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     120 * time.Second,
		// 正常 TLS 握手通常在 1 秒内完成；5 秒足以判定一个卡死/被阻断的源，
		// 避免单个坏源拖满 10 秒才换下一个镜像。
		TLSHandshakeTimeout: 5 * time.Second,
		// 仅限制"请求发出后等待响应头"的时间，不包含读取响应体，
		// 因此大文件/Release 资产的流式下载不会被该超时切断。
		ResponseHeaderTimeout: 12 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// 关闭自动解压，保持字节流原样透传，避免破坏 Content-Length 与 Range 语义。
		DisableCompression: true,
		ForceAttemptHTTP2:  true,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	// 直连试探专用传输：复用连接池与拨号/TLS 参数，仅把响应头等待压到 5s，
	// 使"通但极慢"的直连能快速失败。Clone 共享底层连接池，不额外占用资源。
	directTransport := transport.Clone()
	directTransport.ResponseHeaderTimeout = directHeaderTimeout
	noRedirect := func(req *http.Request, via []*http.Request) error {
		// 交由上层处理重定向改写，这里不做自动跟随。
		return http.ErrUseLastResponse
	}

	eng := &Engine{
		cfg:          opt.Config,
		pool:         opt.Pool,
		hosts:        config.HostSet(),
		transport:    transport,
		directClient: &http.Client{Transport: directTransport, CheckRedirect: noRedirect},
		client: &http.Client{
			Transport: transport,
			// 不设置整体超时：大文件下载可能持续很久，改用分阶段超时控制。
			CheckRedirect: noRedirect,
		},
		logf:     opt.Logf,
		sem:      make(chan struct{}, opt.MaxFlight),
		directCB: make(map[string]*directBreaker),
	}
	eng.caPtr.Store(opt.CA)
	return eng
}

// CurrentCA 返回当前生效的根证书，可能为 nil。
func (e *Engine) CurrentCA() *CA { return e.caPtr.Load() }

// SetCA 热替换根证书，立即对新的 TLS 连接生效。
func (e *Engine) SetCA(ca *CA) { e.caPtr.Store(ca) }

// directAvailable 判断某域名当前是否应在请求路径内联尝试直连。
//
// 只有从未失败（健康）或已被后台探测确认恢复时才返回 true。
// 熔断冷却期间、以及冷却到期等待后台探测结论期间都返回 false，
// 因此用户请求永远不会承担"试探直连是否恢复"的拨号超时。
func (e *Engine) directAvailable(host string) bool {
	e.directCBmu.Lock()
	defer e.directCBmu.Unlock()
	cb := e.directCB[host]
	if cb == nil {
		return true
	}
	// 半开：首个请求在锁内原子取走唯一的试探令牌；在它得出结论前，
	// 其它并发请求因令牌已空且 fails 仍达阈值而继续走镜像。
	if cb.halfOpen {
		cb.halfOpen = false
		return true
	}
	// 未达熔断阈值时仍内联尝试直连，以容忍偶发的单次抖动；
	// 达到阈值后改由后台探测决定恢复时机，用户请求一律走镜像。
	return cb.fails < directFailThreshold
}

// recordDirect 记录某域名一次直连尝试的结果。
//
// ok 为 true（成功完成响应，含业务级 4xx）时清零失败计数并恢复直连；
// ok 为 false（连接/超时/5xx/限流）时累加失败计数，达到阈值进入冷却，
// 并安排一次冷却到期后的后台探测。
func (e *Engine) recordDirect(host string, ok bool) {
	e.directCBmu.Lock()
	defer e.directCBmu.Unlock()
	cb := e.directCB[host]
	if cb == nil {
		cb = &directBreaker{}
		e.directCB[host] = cb
	}
	// 无论结论如何，到达这里说明半开的那次试探（或普通尝试）已结束。
	cb.halfOpen = false
	if ok {
		if cb.fails > 0 || !cb.until.IsZero() {
			e.logf("info", "%s 直连已恢复，重新启用直连优先", host)
		}
		if cb.timer != nil {
			cb.timer.Stop()
			cb.timer = nil
		}
		cb.fails = 0
		cb.until = time.Time{}
		return
	}
	cb.fails++
	// 未达阈值视为偶发抖动：不熔断、不安排探测，保留一次内联直连机会。
	if cb.fails < directFailThreshold {
		return
	}
	cb.until = time.Now().Add(directCooldown)
	e.logf("warn", "%s 直连连续失败 %d 次，%s 内直接走镜像通道", host, cb.fails, directCooldown)
	e.scheduleProbeLocked(host, cb)
}

// scheduleProbeLocked 安排一次冷却到期后的后台直连探测（调用方持锁）。
func (e *Engine) scheduleProbeLocked(host string, cb *directBreaker) {
	if cb.timer != nil {
		cb.timer.Stop()
	}
	cb.timer = time.AfterFunc(directCooldown, func() { e.probeDirect(host) })
}

// directProbePath 返回某直连优先域名用于后台探活的代表性路径。
//
// 不能用轻量根路径：在 QoS 限速网络里根路径响应头/小响应会被突发带宽快速放行，
// 探测据此误判恢复，真实的大响应却依旧极慢。因此 github.com 采用响应稳定在
// 数百 KB 的 git advertisement 端点，配合 readProbe 实测下载方向吞吐；
// api 域名为节省有限的未认证速率配额，改用极轻量的 /zen。
func directProbePath(host string) string {
	switch host {
	case "github.com", "www.github.com":
		return "/git/git/info/refs?service=git-upload-pack"
	case "api.github.com":
		return "/zen"
	default:
		return "/"
	}
}

// probeDirectOnce 对单个直连域名执行一次真实探测并返回是否通过。
//
// 判定标准与真实转发完全一致：directClient 限响应头、readProbe 限下载吞吐。
// 不含任何熔断状态迁移，便于冷却复检与启动预热两种场景复用同一把尺子。
func (e *Engine) probeDirectOnce(host string) bool {
	// 总预算需覆盖 拨号 + 等待响应头 + 探针读取 三个阶段。
	ctx, cancel := context.WithTimeout(context.Background(), directHeaderTimeout+probeTimeout+3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+host+directProbePath(host), nil)
	if err != nil {
		return false
	}
	req.Host = host

	resp, err := e.directClient.Do(req)
	if err != nil {
		e.logf("debug", "%s 后台直连探测失败，继续走镜像: %v", host, err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		e.logf("debug", "%s 后台直连探测收到 %d，继续走镜像", host, resp.StatusCode)
		return false
	}

	// 与真实转发同一把尺子实测下载吞吐，挡掉"头快体慢"的假活链路。
	if _, err := readProbe(resp.Body, probeTimeout); err != nil {
		e.logf("debug", "%s 后台直连探测吞吐不达标，继续走镜像: %v", host, err)
		return false
	}
	return true
}

// probeDirect 是冷却到期后的后台复检：通过则进入半开，失败则续期冷却并再次排队。
//
// 探测通过后只进入"半开"（markDirectHalfOpen）——放行下一次真实请求做最终
// 验证，而不是立刻全面恢复，避免探测被 QoS 突发带宽欺骗后连累一批真实请求。
func (e *Engine) probeDirect(host string) {
	if !e.Running() {
		return
	}
	if e.probeDirectOnce(host) {
		e.markDirectHalfOpen(host)
		return
	}
	e.recordDirect(host, false)
}

// warmupDirect 是服务启动后对直连优先域名的一次性预热。
//
// 在直连被阻断的网络里，若等首个真实请求去触发熔断，重启后前两个请求要各承担
// 一次探测超时。改为启动后后台先探一次：失败立即预置熔断并安排复检，使首个
// 真实请求也直接走镜像；探测通过也不直接判健康——QoS 可能只放行探针这段
// 突发流量而拖慢完整响应，因此同样进入半开，由首个真实请求做最终裁决。
func (e *Engine) warmupDirect(host string) {
	if !e.Running() {
		return
	}
	if e.probeDirectOnce(host) {
		e.markDirectHalfOpen(host)
		return
	}
	e.directCBmu.Lock()
	cb := e.directCB[host]
	if cb == nil {
		cb = &directBreaker{}
		e.directCB[host] = cb
	}
	cb.halfOpen = false
	cb.fails = directFailThreshold
	cb.until = time.Now().Add(directCooldown)
	e.scheduleProbeLocked(host, cb)
	e.directCBmu.Unlock()
	e.logf("warn", "%s 启动预热探测失败，已直接走镜像通道，%s 后后台复检", host, directCooldown)
}

// markDirectHalfOpen 在后台探测通过后把域名置于"半开"状态。
//
// 半开只放行下一次真实用户请求走直连做最终验证：成功（recordDirect(true)）
// 才完全清零恢复，失败则立即重新熔断。这样即便探测端点意外被 QoS 突发带宽
// 欺骗，最多也只影响一个真实请求，而不是连续两个。
func (e *Engine) markDirectHalfOpen(host string) {
	e.directCBmu.Lock()
	defer e.directCBmu.Unlock()
	cb := e.directCB[host]
	if cb == nil {
		cb = &directBreaker{}
		e.directCB[host] = cb
	}
	if cb.timer != nil {
		cb.timer.Stop()
		cb.timer = nil
	}
	// fails 维持熔断阈值：普通判定仍不可用，仅靠 halfOpen 令牌放行唯一一次试探。
	cb.fails = directFailThreshold
	cb.until = time.Time{}
	cb.halfOpen = true
	e.logf("info", "%s 后台直连探测通过，放行一次真实请求验证恢复", host)
}

// resetDirectBreakers 清空全部直连熔断状态，用于代理重启后重新探测。
func (e *Engine) resetDirectBreakers() {
	e.directCBmu.Lock()
	for _, cb := range e.directCB {
		if cb.timer != nil {
			cb.timer.Stop()
		}
	}
	e.directCB = make(map[string]*directBreaker)
	e.directCBmu.Unlock()
}

// Metrics 返回指标引用，供 API 层读取。
func (e *Engine) Metrics() *Metrics { return &e.metrics }

// Start 在指定地址启动代理监听。
func (e *Engine) Start(addr string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return errors.New("代理服务已在运行")
	}

	addr = config.JoinHostPort(addr)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("监听 %s 失败: %w", addr, err)
	}
	e.listener = ln

	e.server = &http.Server{
		Handler:           e,
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
		// 关闭 Go 默认的请求体大小限制，GitHub 推送包可能很大。
		MaxHeaderBytes: 1 << 20,
		ErrorLog:       nil,
	}

	e.running = true

	// 重启后重新探测直连可用性，清空历史熔断状态。
	e.resetDirectBreakers()

	// 启动预热：稍等网络栈就绪后在后台探测直连优先域名。在直连被阻断的网络里
	// 可提前熔断，使重启后的首个真实请求也直接走镜像、不承担探测超时。
	for host := range config.DirectPreferredHosts() {
		h := host
		time.AfterFunc(warmupDelay, func() { e.warmupDirect(h) })
	}

	// Go 的 http.Server 无法处理 CONNECT 方法，而 HTTPS 代理请求恰恰依赖它。
	// 因此在裸 TCP 层做一次分流：CONNECT 请求自行处理，其余交给 http.Server。
	go e.dispatchConnections(ln)

	e.logf("info", "代理服务已启动，监听 %s", ln.Addr().String())
	return nil
}

// dispatchConnections 在 TCP 层分流连接。
//
// 实现方式：先派出一个窥探连接，读出首个请求行，
// 若是 CONNECT 则建立隧道，否则把窥探到的字节重新拼回连接交给 http.Server。
//
// 由于 Go 的 net.Listener 不支持"放回"已读取的数据，
// 这里采用更直接的做法：对每个连接做一次协议嗅探，
// 把非 CONNECT 的连接整体转交给一个自定义的单连接 HTTP 服务处理。
func (e *Engine) dispatchConnections(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			// 监听器关闭导致的错误属于正常退出路径。
			if errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "use of closed") {
				return
			}
			e.logf("warn", "接受连接失败: %v", err)
			continue
		}

		select {
		case e.sem <- struct{}{}:
		default:
			// 并发已满，直接拒绝新连接以保护 NAS 资源。
			_ = conn.Close()
			continue
		}

		go func(c net.Conn) {
			defer func() { <-e.sem }()
			e.handleConn(c)
		}(conn)
	}
}

// Stop 优雅关闭代理服务。
//
// 由于连接分发由自定义监听循环负责，这里直接关闭监听器即可让
// Accept 返回错误并退出循环；随后等待在途请求自然结束。
func (e *Engine) Stop(ctx context.Context) error {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return nil
	}
	e.running = false
	ln := e.listener
	srv := e.server
	e.mu.Unlock()

	e.transport.CloseIdleConnections()

	if ln != nil {
		_ = ln.Close()
	}
	if srv != nil {
		return srv.Shutdown(ctx)
	}
	return nil
}

// Running 返回服务是否在运行。
func (e *Engine) Running() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.running
}

// Addr 返回实际监听地址。
func (e *Engine) Addr() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.listener == nil {
		return ""
	}
	return e.listener.Addr().String()
}

// ServeHTTP 实现 http.Handler，是全部请求的入口。
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.metrics.TotalRequests.Add(1)

	cfg := e.cfg()

	// Docker Registry 加速：registry-mirrors 指向本机时，/v2/ 请求在此处理。
	if e.isDockerRequest(r) {
		e.serveDocker(w, r)
		return
	}

	// URL 前缀形态：/https://github.com/owner/repo/...
	// 让 git、wget、curl 无需配置代理即可使用本加速器，
	// 例如 git config --global url."http://nas:7710/https://github.com/".insteadOf "https://github.com/"
	if host, rest, ok := extractEmbeddedURL(r.URL.Path); ok {
		if _, managed := e.hosts[host]; managed || isGitHubInfra(host) {
			// 保留查询参数，否则嵌入式 URL 请求会丢失 query string。
			path := rest
			if r.URL.RawQuery != "" {
				path += "?" + r.URL.RawQuery
			}
			t := &target{host: host, path: path, category: e.categorize(host)}
			e.serveAccelerated(w, r, cfg, t)
			return
		}
	}

	if cfg.Mode == config.ModeDirect {
		e.forwardDirect(w, r, cfg)
		return
	}

	target := e.resolveTarget(r)
	if target == nil {
		// 非 GitHub 域名：如果是显式代理请求则拒绝，否则返回简单提示页。
		if r.URL.Host != "" && r.URL.IsAbs() {
			http.Error(w, "GitHub++ 仅加速 GitHub 相关域名", http.StatusForbidden)
			return
		}
		e.serveInfo(w, r)
		return
	}

	e.serveAccelerated(w, r, cfg, target)
}

// extractEmbeddedURL 从路径中解析 "前缀+完整URL" 形态的请求。
//
// 例如 /https://github.com/owner/repo 返回 host=github.com、rest=/owner/repo。
// 兼容部分客户端把 // 折叠成 / 的情况（/https:/github.com）。
func extractEmbeddedURL(p string) (host, rest string, ok bool) {
	for _, prefix := range []string{"/https://", "/http://", "/https:/", "/http:/"} {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		remaining := p[len(prefix):]
		// 折叠形态补回双斜杠。
		if strings.HasSuffix(prefix, ":/") && !strings.HasPrefix(remaining, "/") {
			remaining = "/" + remaining
		}
		i := strings.IndexByte(remaining, '/')
		if i <= 0 {
			return "", "", false
		}
		host = strings.ToLower(remaining[:i])
		if host == "" || !strings.Contains(host, ".") {
			return "", "", false
		}
		return host, remaining[i:], true
	}
	return "", "", false
}

// categorize 返回域名所属的加速分类。
func (e *Engine) categorize(host string) string {
	if gh, ok := e.hosts[host]; ok {
		return gh.Category
	}
	return config.CatRaw
}

// target 描述一次经过解析的加速目标。
type target struct {
	host     string
	path     string
	category string
}

// resolveTarget 判断本次请求应加速哪个域名。
//
// 两种来源：
//  1. 显式代理：请求行里是绝对 URL，直接取其 Host；
//  2. 透明反代：请求行是路径，从 Host 头（或 X-Forwarded-Host）取域名。
func (e *Engine) resolveTarget(r *http.Request) *target {
	// 显式代理形态。
	if r.URL.IsAbs() && r.URL.Host != "" {
		host := stripPort(r.URL.Host)
		if _, ok := e.hosts[host]; !ok {
			if !isGitHubInfra(host) {
				return nil
			}
			return &target{host: host, path: pathOf(r), category: config.CatRaw}
		}
		return &target{host: host, path: pathOf(r), category: e.hosts[host].Category}
	}

	// 透明反代形态，优先信任 X-Forwarded-Host 以支持反向代理前置。
	host := stripPort(r.Host)
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = stripPort(strings.TrimSpace(strings.Split(fh, ",")[0]))
	}
	if host == "" {
		return nil
	}

	gh, ok := e.hosts[host]
	if !ok {
		// 兼容没有列入但确实属于 GitHub 基础设施的域名，
		// 例如 githubusercontent.com 下的动态子域。
		if isGitHubInfra(host) {
			return &target{host: host, path: pathOf(r), category: config.CatRaw}
		}
		return nil
	}
	return &target{host: host, path: pathOf(r), category: gh.Category}
}

// pathOf 提取请求的路径与查询串。
//
// 关键点：显式代理请求的 r.URL 里带有完整地址（scheme://host/path），
// 此时 RequestURI 会返回整串内容，直接拼接会得到错误的上游地址。
// 因此这里只取 Path 与 RawQuery，并在缺省时补上斜杠。
func pathOf(r *http.Request) string {
	p := r.URL.Path
	if p == "" {
		p = "/"
	}
	if r.URL.RawQuery != "" {
		p += "?" + r.URL.RawQuery
	}
	return p
}

// isGitHubInfra 用后缀匹配兜住未显式列出的 GitHub 基础设施域名。
func isGitHubInfra(host string) bool {
	suffixes := []string{
		".githubusercontent.com",
		".githubassets.com",
		".github.io",
	}
	for _, s := range suffixes {
		if strings.HasSuffix(host, s) {
			return true
		}
	}
	return host == "github.com" || strings.HasSuffix(host, ".github.com")
}

// stripPort 去掉主机名中的端口部分。
func stripPort(h string) string {
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		if _, err := strconv.Atoi(h[i+1:]); err == nil {
			return h[:i]
		}
	}
	return strings.ToLower(strings.TrimSpace(h))
}

// serveAccelerated 执行带加速通道的转发。
func (e *Engine) serveAccelerated(w http.ResponseWriter, r *http.Request, cfg *config.Config, t *target) {
	e.bumpCategory(t.category)

	mirrors := cfg.EnabledMirrors()
	if len(mirrors) == 0 {
		e.forwardDirect(w, r, cfg)
		return
	}

	// 对来源敏感的域名优先直连：经镜像中转容易被判定为异常流量而返回 403。
	// 直连失败时仍会回落到镜像通道，因此不会牺牲可用性。
	//
	// 但若该域名近期直连连续失败（例如网络对 GitHub 做 SNI 阻断），
	// 熔断冷却期内不再每次都白等一个拨号超时，直接走镜像；
	// 冷却到期由后台探测异步确认链路是否恢复，恢复后自动回切直连，
	// 试探成本不会落到真实用户请求上。
	if config.HostIsDirectPreferred(t.host) && r.Method != http.MethodPost {
		if e.directAvailable(t.host) {
			// 带 body 的方法（PUT/DELETE/PATCH 等）需要先缓冲 body，
			// 否则直连消耗 body 后镜像重试会拿到空 body，导致请求不完整。
			var bodyBytes []byte
			if r.Body != nil && r.Body != http.NoBody {
				if b, err := io.ReadAll(r.Body); err == nil {
					bodyBytes = b
					r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
					r.ContentLength = int64(len(bodyBytes))
				}
			}
			if done, healthy, err := e.tryDirect(w, r, cfg, t); done {
				// 慢成功也会被记为不健康，立即熔断保护后续请求。
				e.recordDirect(t.host, healthy)
				return
			} else {
				e.recordDirect(t.host, false)
				e.logf("debug", "%s 直连未成功，改用镜像通道: %v", t.host, err)
				// 直连失败后重置 body，供镜像重试使用。
				if bodyBytes != nil {
					r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
					r.ContentLength = int64(len(bodyBytes))
				}
			}
		} else {
			e.logf("debug", "%s 直连熔断冷却中，本次直接走镜像通道", t.host)
		}
	}

	// 最多尝试 3 个源，逐个降级。
	const maxAttempts = 3
	var lastErr error

	for attempt := 0; attempt < maxAttempts; attempt++ {
		m, ok := e.pool.Pick(mirrors, t.category)
		if !ok {
			// 所有源都在冷却中，先直连再重置冷却，避免服务不可用。
			e.pool.ResetCooldown()
			if attempt == 0 {
				e.logf("warn", "全部加速源处于冷却状态，本次直连 %s", t.host)
				e.forwardDirect(w, r, cfg)
				return
			}
			break
		}

		done, err := e.tryMirror(w, r, cfg, t, m)
		if done {
			return
		}
		lastErr = err

		if attempt > 0 {
			e.metrics.Failovers.Add(1)
		}
		e.logf("warn", "源 %s 转发 %s 失败，尝试下一个源: %v", m.Name, t.host, err)

		if shouldEvict, cooldown := e.pool.ShouldEvict(m.ID, cfg.Proxy.FailoverThreshold, time.Duration(cfg.Proxy.CooldownSeconds)*time.Second); shouldEvict {
			e.logf("warn", "源 %s 连续失败已达阈值，冷却 %s", m.Name, cooldown)
		}
	}

	e.metrics.Failed.Add(1)
	if lastErr == nil {
		lastErr = errors.New("没有可用的加速源")
	}
	http.Error(w, "GitHub++ 加速失败: "+lastErr.Error(), http.StatusBadGateway)
}

// tryDirect 尝试直连 GitHub 官方。
//
// 返回 done 表示响应是否已写给客户端（done=false 时尚未写出任何字节，上层可
// 安全改走镜像；done=true 表示响应已提交，调用方不应再尝试其他通路）。healthy
// 仅在 done=true 时有意义，表示这次直连按端到端质量是否算健康，供上层决定熔断：
// 慢成功（响应被 QoS 拖慢）也会判劣质，从而只影响这一次、立即保护后续请求。
//
// 这样设计兼顾成功率与用户体验：直连拿到 404 等业务错误是正常结果，不该重试；
// 连接超时、DNS 失败、探针不达标才值得换通道。
func (e *Engine) tryDirect(w http.ResponseWriter, r *http.Request, cfg *config.Config, t *target) (done bool, healthy bool, err error) {
	// 建连/TLS/等待响应头由 transport 的分阶段超时控制；
	// 这里直接跟随客户端连接生命周期，避免固定读超时在大文件流式传输中途切断连接。
	ctx := r.Context()

	req, err := http.NewRequestWithContext(ctx, r.Method, "https://"+t.host+t.path, r.Body)
	if err != nil {
		return false, false, err
	}
	copyRequestHeaders(req.Header, r.Header, config.Mirror{Kind: config.KindDirect}, t.host)
	req.ContentLength = r.ContentLength
	req.Host = t.host

	start := time.Now()
	// 使用直连专用 client：响应头超过 directHeaderTimeout 未到达即快速失败，
	// 由上层 recordDirect 计入熔断并回退镜像；响应体流式传输不受该超时影响。
	resp, err := e.directClient.Do(req)
	if err != nil {
		// 连接层失败（含响应头超时），允许换通道重试。
		return false, false, err
	}
	defer resp.Body.Close()

	// 上游 5xx 视为直连不稳定，改走镜像更可靠。
	if resp.StatusCode >= 500 {
		return false, false, fmt.Errorf("直连返回 %d", resp.StatusCode)
	}

	// 403/429 通常意味着被限流或按来源拒绝，换镜像通道可能成功。
	// 但若响应体表明是权限问题（例如需要登录），换通道也无济于事，
	// 这里采用简单策略：先换通道试一次，失败则由外层返回最终错误。
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		// 已经是带认证信息的请求，换通道也不会改变结果，直接返回。
		if r.Header.Get("Authorization") != "" || len(body) == 0 {
			copyResponseHeaders(w.Header(), resp, t.host)
			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(body)
			return true, true, nil
		}
		return false, false, fmt.Errorf("直连返回 %d", resp.StatusCode)
	}

	// 直连返回的重定向若指向 GitHub 域名，需改写为嵌入式 URL，
	// 否则嵌入式 URL 形态的客户端会绕过加速通道直连目标。
	if isRedirect(resp.StatusCode) {
		loc := resp.Header.Get("Location")
		if loc != "" {
			resp.Header.Set("Location", rewriteRedirect(loc, config.Mirror{Kind: config.KindDirect}, t, r))
		}
	}

	// 链路探针：响应头虽已到达，响应体仍可能被 QoS 限速掐住。
	// 此刻尚未向客户端写出任何字节，探针不达标可安全放弃直连、回退镜像。
	probe, probeErr := readProbe(resp.Body, probeTimeout)
	if probeErr != nil {
		return false, false, fmt.Errorf("直连链路探针失败: %w", probeErr)
	}

	copyResponseHeaders(w.Header(), resp, t.host)
	w.WriteHeader(resp.StatusCode)

	// 先回放探针阶段已读出的数据，再无超时地透传剩余响应体。
	n, copyErr := io.Copy(w, io.MultiReader(bytes.NewReader(probe), resp.Body))
	e.metrics.BytesOut.Add(n)
	e.metrics.Accelerated.Add(1)

	latency := time.Since(start)
	if copyErr == nil || isClientGone(copyErr) {
		saved := 1200 - latency.Milliseconds()
		if saved > 0 {
			e.metrics.SavedMS.Add(saved)
		}
	}

	// 兜底判定本次直连是否健康。探针只保证前 512KB，整个响应提交后若剩余部分
	// 被掐慢，已无法换源；此时按端到端结论熔断，避免后续请求继续走慢直连。
	healthy = true
	switch {
	case isClientGone(copyErr):
		// 客户端主动断开，不归咎上游链路。
		healthy = true
	case copyErr != nil:
		healthy = false
		e.logf("warn", "%s 直连响应体传输中断: %v", t.host, copyErr)
	case latency > directSlowMinDuration && n > 0:
		// 用平均吞吐而非总耗时，避免误伤吞吐正常但体积大的文件下载。
		secs := int64(latency / time.Second)
		if secs > 0 && n/secs < directMinThroughput {
			healthy = false
			e.logf("warn", "%s 直连端到端吞吐过低（%dKB/%ds ≈ %dKB/s），判定劣质，后续请求改走镜像",
				t.host, n/1024, secs, n/1024/secs)
		}
	}
	// 4xx 也属于"直连完成"，不重试；健康度由上面的传输质量决定。
	return true, healthy, nil
}

// tryMirror 用指定源转发一次请求。
//
// 返回值 done 表示响应是否已经写给客户端（无论成功还是业务性失败），
// 若 done 为 false，则调用方应当换源重试。
func (e *Engine) tryMirror(w http.ResponseWriter, r *http.Request, cfg *config.Config, t *target, m config.Mirror) (done bool, err error) {
	targetURL := buildURL(m, t.host, t.path)

	// 建连/TLS/等待响应头由 transport 的分阶段超时控制；
	// 这里直接跟随客户端连接生命周期，避免固定读超时在大文件流式传输中途切断连接。
	ctx := r.Context()

	outReq, err := http.NewRequestWithContext(ctx, r.Method, targetURL, r.Body)
	if err != nil {
		return false, fmt.Errorf("构造上游请求失败: %w", err)
	}
	copyRequestHeaders(outReq.Header, r.Header, m, t.host)
	outReq.ContentLength = r.ContentLength
	outReq.Host = hostOf(targetURL)

	start := time.Now()
	resp, err := e.client.Do(outReq)
	latency := time.Since(start)
	if err != nil {
		e.pool.Record(m.ID, latency, 0, err)
		return false, err
	}
	defer resp.Body.Close()

	// 上游返回 5xx 视为该源不可用，换源重试。
	if resp.StatusCode >= 500 {
		e.pool.Record(m.ID, latency, 0, fmt.Errorf("上游返回 %d", resp.StatusCode))
		return false, fmt.Errorf("源 %s 返回 %d", m.Name, resp.StatusCode)
	}

	// 某些镜像站用 3xx 指向真实地址，需要改写后交给客户端跟随，
	// 避免客户端直接访问 GitHub 而绕过加速通道。
	if isRedirect(resp.StatusCode) {
		loc := resp.Header.Get("Location")
		if loc != "" {
			resp.Header.Set("Location", rewriteRedirect(loc, m, t, r))
		}
	}

	// 链路探针：识别"响应头秒回、响应体却被掐到几十秒"的劣质源，
	// 在写出任何字节给客户端之前换下一个镜像源，并把失败计入调度。
	probe, probeErr := readProbe(resp.Body, probeTimeout)
	if probeErr != nil {
		e.pool.Record(m.ID, latency, 0, probeErr)
		return false, fmt.Errorf("源 %s 链路探针失败，换源重试: %w", m.Name, probeErr)
	}

	copyResponseHeaders(w.Header(), resp, t.host)

	w.WriteHeader(resp.StatusCode)

	// 先回放探针数据，再无超时地流式透传剩余响应体，边转发边统计流量。
	counter := &countingWriter{w: w}
	n, copyErr := io.Copy(counter, io.MultiReader(bytes.NewReader(probe), resp.Body))

	flagsOK := copyErr == nil
	if !flagsOK && isClientGone(copyErr) {
		// 客户端主动断开（例如下载被取消），不算源的失败。
		flagsOK = true
	}

	e.pool.Record(m.ID, latency, n, errFromOK(flagsOK, copyErr))

	e.metrics.BytesOut.Add(n)
	e.metrics.Accelerated.Add(1)
	// 估算节省耗时：假设直连首包需要 1.2 秒，加速后为实测延迟。
	saved := 1200 - latency.Milliseconds()
	if saved > 0 {
		e.metrics.SavedMS.Add(saved)
	}

	return true, nil
}

// forwardDirect 直接回源到 GitHub 官方，不做任何中转。
func (e *Engine) forwardDirect(w http.ResponseWriter, r *http.Request, cfg *config.Config) {
	host := ""
	path := ""
	if r.URL.IsAbs() && r.URL.Host != "" {
		host = stripPort(r.URL.Host)
		path = pathOf(r)
	} else {
		host = stripPort(r.Host)
		path = pathOf(r)
	}
	if host == "" {
		e.serveInfo(w, r)
		return
	}

	// 建连/TLS/等待响应头由 transport 的分阶段超时控制；
	// 这里直接跟随客户端连接生命周期，避免固定读超时在大文件流式传输中途切断连接。
	ctx := r.Context()

	req, err := http.NewRequestWithContext(ctx, r.Method, "https://"+host+path, r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	copyRequestHeaders(req.Header, r.Header, config.Mirror{Kind: config.KindDirect}, host)
	req.ContentLength = r.ContentLength
	req.Host = host

	resp, err := e.client.Do(req)
	if err != nil {
		e.metrics.Failed.Add(1)
		http.Error(w, "直连失败: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if isRedirect(resp.StatusCode) {
		// 直连模式下重定向保持原样即可。
		_ = resp.Header.Get("Location")
	}

	copyResponseHeaders(w.Header(), resp, host)
	w.WriteHeader(resp.StatusCode)

	counter := &countingWriter{w: w}
	n, _ := io.Copy(counter, resp.Body)
	e.metrics.BytesOut.Add(n)
}

// copyRequestHeaders 复制客户端请求头到上游请求，并做必要清理。
func copyRequestHeaders(dst, src http.Header, m config.Mirror, originHost string) {
	for k, vv := range src {
		lk := strings.ToLower(k)
		switch lk {
		// 逐跳头（hop-by-hop）不能透传。
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
			"proxy-connection", "te", "trailer", "transfer-encoding", "upgrade":
			continue
		// Go 的 http 客户端会自动管理这些头。
		case "content-length":
			dst.Set(k, vv[0])
			continue
		// Accept-Encoding 由传输层控制，避免压缩干扰字节透传。
		case "accept-encoding":
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}

	if dst.Get("User-Agent") == "" {
		dst.Set("User-Agent", config.DefaultUserAgent)
	}
	dst.Set("Accept-Encoding", "identity")
	// 标明真实来源，方便镜像站统计与问题排查。
	dst.Set("X-Forwarded-Host", originHost)
}

// copyResponseHeaders 复制上游响应头到客户端。
func copyResponseHeaders(dst http.Header, resp *http.Response, originHost string) {
	for k, vv := range resp.Header {
		lk := strings.ToLower(k)
		switch lk {
		case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
			"te", "trailer", "transfer-encoding", "upgrade":
			continue
		// 镜像源设置的安全策略头会干扰浏览器，移除后由被加速站点自身的头替代。
		case "content-security-policy", "content-security-policy-report-only",
			"x-frame-options", "strict-transport-security":
			continue
		case "content-length":
			// 保留，否则客户端无法显示下载进度。
			dst.Set(k, vv[0])
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	// 允许跨域，方便用户在网页里直接引用加速后的资源。
	if dst.Get("Access-Control-Allow-Origin") == "" {
		dst.Set("Access-Control-Allow-Origin", "*")
	}
	dst.Set("Via", "1.1 ghpp")
}

// buildURL 依据源类型拼接最终请求地址。
func buildURL(m config.Mirror, host, path string) string {
	return rewriteForKind(m, host, path)
}

// rewriteForKind 实现各类源的 URL 改写规则。
//
// 规则集中在此处，便于与测速模块保持一致。
func rewriteForKind(m config.Mirror, host, path string) string {
	base := strings.TrimSuffix(m.URL, "/")
	switch m.Kind {
	case config.KindDirect:
		return "https://" + host + path
	case config.KindPrefix, config.KindGitClone:
		return base + "/https://" + host + path
	case config.KindRawCDN:
		if strings.Contains(base, "jsdelivr.net") {
			if p, ok := jsdelivrPath(host, path); ok {
				if !strings.Contains(base, "/gh") {
					base += "/gh"
				}
				return base + p
			}
		}
		return base + "/https://" + host + path
	default:
		return "https://" + host + path
	}
}

// jsdelivrPath 把 raw 路径翻译为 jsDelivr 的 gh 语法。
func jsdelivrPath(host, path string) (string, bool) {
	if host != "raw.githubusercontent.com" {
		return "", false
	}
	// 去掉查询串再解析路径。
	p := path
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) < 4 {
		return "", false
	}
	owner, repo := parts[0], parts[1]
	rest := parts[2:]
	for _, b := range []string{"main", "master", "gh-pages", "develop", "dev"} {
		if len(rest) > 1 && rest[0] == b {
			return "/" + owner + "/" + repo + "@" + b + "/" + strings.Join(rest[1:], "/"), true
		}
	}
	if len(rest) >= 2 {
		return "/" + owner + "/" + repo + "@" + rest[0] + "/" + strings.Join(rest[1:], "/"), true
	}
	return "", false
}

// hostOf 从完整 URL 中提取主机名。
func hostOf(rawURL string) string {
	s := rawURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// rewriteRedirect 处理上游返回的重定向地址。
//
// 镜像站有时会把请求 302 到 GitHub 原始域名，这里把它改写为
// 嵌入式 URL 形态（http://<代理>/https://github.com/...），
// 让客户端继续走本代理，以免绕过加速通道。
//
// 非 GitHub 域名的重定向保持原样，由客户端直接访问
// （例如镜像站自有的对象存储 CDN）。
func rewriteRedirect(loc string, m config.Mirror, t *target, r *http.Request) string {
	// 相对路径直接返回，浏览器会基于当前主机解析。
	if strings.HasPrefix(loc, "/") {
		return loc
	}
	// 只改写指向受管 GitHub 域名的绝对重定向。
	var targetHost string
	for h := range config.HostSet() {
		if strings.Contains(loc, "://"+h) {
			targetHost = h
			break
		}
	}
	if targetHost == "" {
		// 同时兜底 GitHub 基础设施域名（*.githubusercontent.com 等）。
		if isGitHubInfra(hostOf(loc)) {
			targetHost = hostOf(loc)
		}
	}
	if targetHost == "" {
		return loc
	}

	// 用代理自身的地址构造嵌入式 URL，让客户端继续走加速通道。
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		return loc
	}
	return scheme + "://" + host + "/" + loc
}

func isRedirect(code int) bool {
	switch code {
	case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		return true
	}
	return false
}

// isClientGone 判断错误是否由客户端主动断开引起。
func isClientGone(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "broken pipe") ||
		strings.Contains(s, "connection reset by peer") ||
		strings.Contains(s, "client disconnected")
}

// errFromOK 把成功标志转换为 error，便于统一记录。
func errFromOK(ok bool, err error) error {
	if ok {
		return nil
	}
	return err
}

// bumpCategory 累加分类计数。
func (e *Engine) bumpCategory(cat string) {
	switch cat {
	case config.CatWeb:
		e.metrics.CategoryWeb.Add(1)
	case config.CatRaw:
		e.metrics.CategoryRaw.Add(1)
	case config.CatClone:
		e.metrics.CategoryClone.Add(1)
	}
}

// countingWriter 统计已写出的字节数。
type countingWriter struct {
	w http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// Flush 透传 Flush，保证流式下载不被缓冲阻塞。
func (c *countingWriter) Flush() {
	if f, ok := c.w.(http.Flusher); ok {
		f.Flush()
	}
}

// serveInfo 在用户直接访问代理端口时返回一张说明页。
func (e *Engine) serveInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	const page = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>GitHub++ 加速服务运行中</title>
<style>
body{font-family:system-ui,-apple-system,"Segoe UI",sans-serif;background:#0d1117;color:#e6edf3;
display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
.card{max-width:560px;padding:40px;border:1px solid #30363d;border-radius:12px;background:#161b22}
h1{margin:0 0 12px;font-size:20px}code{background:#21262d;padding:2px 6px;border-radius:4px}
p{color:#8b949e;line-height:1.7;margin:8px 0}
</style></head><body><div class="card">
<h1>GitHub++ 加速服务运行中</h1>
<p>当前端口是加速代理入口，不是管理界面。</p>
<p>请前往控制台进行配置：<code>http://本机IP:7717</code></p>
<p>或者直接把本地址配置为 HTTP 代理使用。</p>
</div></body></html>`
	_, _ = w.Write([]byte(page))
}
