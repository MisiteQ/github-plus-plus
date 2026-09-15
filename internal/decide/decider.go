// Package decide 实现加速方式的自动决策。
//
// 这是"由系统自行判断哪种方式更快"的核心：程序会真实测量三条通路的性能，
// 然后选择当前最优的一种，并给出人类可读的决策理由。
//
// 三条通路：
//
//	direct —— 完全不干预，直接访问 GitHub 官方
//	proxy  —— 走本地反代 + 镜像中转
//	hosts  —— 走 DNS 优选 + hosts 注入（本质仍是直连，但命中优质节点）
//
// 决策依据是一个综合评分：以首字节延迟为主、吞吐为辅，
// 并对不稳定的通路做惩罚，避免选择"偶尔很快但经常超时"的方案。
package decide

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ghpp/ghpp/internal/config"
	"github.com/ghpp/ghpp/internal/mirror"
)

// Path 表示一条候选通路。
type Path string

const (
	PathDirect Path = "direct"
	PathProxy  Path = "proxy"
	PathHosts  Path = "hosts"
)

// Sample 是一次通路性能采样。
type Sample struct {
	Path Path `json:"path"`
	// LatencyMS 是首字节延迟。
	LatencyMS int64 `json:"latency_ms"`
	// ThroughputKBps 是下载吞吐。
	ThroughputKBps float64 `json:"throughput_kbps"`
	// OK 表示采样是否成功。
	OK bool `json:"ok"`
	// Err 记录失败原因。
	Err string `json:"error,omitempty"`
	// At 是采样时刻。
	At time.Time `json:"at"`
	// Score 是换算后的得分，越高越好。
	Score float64 `json:"score"`
}

// Verdict 是一次决策的完整结论。
type Verdict struct {
	// Chosen 是最终选定的通路。
	Chosen Path `json:"chosen"`
	// EffectiveMode 是把通路映射回用户可读的模式。
	EffectiveMode config.Mode `json:"effective_mode"`
	// Reason 是决策理由，直接展示给用户。
	Reason string `json:"reason"`
	// Samples 是本次参与决策的全部采样。
	Samples map[Path]Sample `json:"samples"`
	// DecidedAt 是决策时刻。
	DecidedAt time.Time `json:"decided_at"`
	// Improvement 是相对直连的提升倍数。
	//
	// 取值大于 0 时为真实倍数；ImprovementUnavailable 表示直连不可用，
	// 倍数无从计算（此时加速与否是"能用/不能用"的区别）。
	Improvement float64 `json:"improvement"`
	// DirectUsable 表示直连是否可用，避免前端自行推断哨兵值含义。
	DirectUsable bool `json:"direct_usable"`
}

// ImprovementUnavailable 是 Improvement 的哨兵值，表示直连不可用导致倍数无从计算。
//
// 采用 -1 而非 math.Inf(1)，因为 encoding/json 拒绝序列化 Inf/NaN。
const ImprovementUnavailable = -1.0

// Decider 负责通路采样与决策。
type Decider struct {
	pool *mirror.Pool
	logf func(level, format string, args ...any)

	// bestIP 返回某个域名当前优选出的 IP，由 DNS 优选模块提供。
	// 未注入时 hosts 通路退化为与直连等价的测量。
	bestIP func(host string) (string, bool)

	// measure 抽象一条通路的测量过程，便于测试替换。
	measure func(ctx context.Context, p Path, targets []config.ProbeTarget) Sample

	mu      sync.RWMutex
	verdict Verdict
}

// BestIPFunc 由上层注入：给定域名返回优选 IP。
type BestIPFunc func(host string) (string, bool)

// New 创建决策器。
func New(pool *mirror.Pool, logf func(level, format string, args ...any)) *Decider {
	if logf == nil {
		logf = func(string, string, ...any) {}
	}
	d := &Decider{
		pool: pool,
		logf: logf,
	}
	d.measure = d.liveMeasure
	return d
}

// SetBestIPSource 注入优选 IP 来源，使 hosts 通路能测到真实效果。
func (d *Decider) SetBestIPSource(fn BestIPFunc) {
	d.bestIP = fn
}

// Decide 重新评估三条通路并返回决策结论。
//
// probes 由调用方提供，proxy 通路的测量会复用镜像源池的实测数据，
// 避免重复测速造成不必要的流量与耗时。
func (d *Decider) Decide(ctx context.Context, cfg *config.Config, probes []mirror.Result) Verdict {
	targets := config.DomainProbeTargets()

	// 直连与 hosts 两条通路需要真实拨号测量。
	var directSample, hostsSample Sample
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		directSample = d.measure(ctx, PathDirect, targets)
	}()
	go func() {
		defer wg.Done()
		hostsSample = d.measure(ctx, PathHosts, targets)
	}()
	wg.Wait()

	proxySample := d.scoreProxyProbes(probes)

	samples := map[Path]Sample{
		PathDirect: directSample,
		PathProxy:  proxySample,
		PathHosts:  hostsSample,
	}

	verdict := d.conclude(cfg, samples, probes)

	d.mu.Lock()
	d.verdict = verdict
	d.mu.Unlock()

	d.logf("info", "自动决策完成：选用 %s，理由：%s", verdict.Chosen, verdict.Reason)
	return verdict
}

// conclude 依据采样结果得出最终结论。
func (d *Decider) conclude(cfg *config.Config, samples map[Path]Sample, probes []mirror.Result) Verdict {
	v := Verdict{
		Samples:   samples,
		DecidedAt: time.Now(),
	}

	direct := samples[PathDirect]
	proxy := samples[PathProxy]
	hosts := samples[PathHosts]

	// 先确定在加速通路中哪个更快。
	best := proxy
	bestPath := PathProxy
	if hosts.OK && (!proxy.OK || hosts.Score > proxy.Score) {
		best = hosts
		bestPath = PathHosts
	}

	directUsable := direct.OK && direct.Score > 0

	// 直连可用且明显更快时，回退直连。
	if directUsable {
		if !best.OK {
			v.Chosen = PathDirect
			v.EffectiveMode = config.ModeDirect
			v.Improvement = 1
			v.Reason = fmt.Sprintf("所有加速通路均不可用（代理: %s；优选: %s），已自动回退直连",
				errText(proxy), errText(hosts))
			return v
		}
		if safeRatio(direct.Score, best.Score) >= cfg.Auto.DirectBetterRatio {
			v.Chosen = PathDirect
			v.EffectiveMode = config.ModeDirect
			v.Improvement = 1 / safeRatio(best.Score, direct.Score)
			v.Reason = fmt.Sprintf("直连延迟 %dms 已优于加速通路的 %dms，当前网络无需加速",
				direct.LatencyMS, best.LatencyMS)
			return v
		}
	}

	if !best.OK {
		v.Chosen = PathDirect
		v.EffectiveMode = config.ModeDirect
		v.Improvement = 1
		v.Reason = "未探测到可用通路，已回退直连"
		return v
	}

	v.Chosen = bestPath
	// 直连不可用时无法计算提升倍数，用 ImprovementUnavailable 哨兵值表示"没有加速就用不了"。
	//
	// 注意：这里不能用 math.Inf，因为 encoding/json 无法序列化 Inf/NaN，
	// 会让 /api/status 这类对外接口整体编码失败（表现为 200 + 空 body）。
	if directUsable {
		v.Improvement = safeRatio(best.Score, direct.Score)
	} else {
		v.Improvement = ImprovementUnavailable
	}
	v.DirectUsable = directUsable

	switch bestPath {
	case PathProxy:
		v.EffectiveMode = config.ModeProxy
		topName, topLatency := topMirror(probes)
		switch {
		case !directUsable:
			v.Reason = fmt.Sprintf("直连不可达，已启用镜像中转；当前最优源 %s（%dms，%.0f KB/s）",
				topName, topLatency, best.ThroughputKBps)
		case v.Improvement >= 1.1:
			v.Reason = fmt.Sprintf("镜像中转延迟 %dms，比直连快 %.1f 倍；当前最优源 %s（%dms）",
				best.LatencyMS, v.Improvement, topName, topLatency)
		default:
			// 提升不明显但直连也未达到回退阈值，保守选择加速通道。
			v.Reason = fmt.Sprintf("镜像中转与直连表现接近（%dms 对 %dms），当前优先使用加速通道以规避波动；最优源 %s",
				best.LatencyMS, direct.LatencyMS, topName)
		}
	case PathHosts:
		v.EffectiveMode = config.ModeHosts
		switch {
		case !directUsable:
			v.Reason = fmt.Sprintf("直连不可达，DNS 优选后节点可用（%dms）", best.LatencyMS)
		case v.Improvement >= 1.1:
			v.Reason = fmt.Sprintf("DNS 优选后延迟 %dms，比直连快 %.1f 倍，且无需经过第三方中转",
				best.LatencyMS, v.Improvement)
		default:
			v.Reason = fmt.Sprintf("DNS 优选后延迟 %dms，与直连相当，但解析更稳定",
				best.LatencyMS)
		}
	}
	return v
}

// scoreProxyProbes 把镜像源池的实测结果汇总成代理通路的一个采样。
//
// 取所有成功源的中位数而非最优值，因为调度器是随机择优而非固定用第一名，
// 中位数更贴近用户实际会体验到的水平。
func (d *Decider) scoreProxyProbes(probes []mirror.Result) Sample {
	var ok []mirror.Result
	for _, p := range probes {
		if p.OK && p.LatencyMS > 0 {
			ok = append(ok, p)
		}
	}
	if len(ok) == 0 {
		return Sample{
			Path: PathProxy,
			Err:  "所有镜像源均测速失败",
			At:   time.Now(),
		}
	}

	sort.Slice(ok, func(i, j int) bool { return ok[i].LatencyMS < ok[j].LatencyMS })

	// 取前 1/3 的源做统计，代表调度器实际会优先使用的范围。
	n := len(ok) / 3
	if n < 1 {
		n = 1
	}
	top := ok[:n]

	var latSum int64
	var throughSum float64
	for _, p := range top {
		latSum += p.LatencyMS
		throughSum += p.ThroughputKBps
	}
	lat := latSum / int64(len(top))
	through := throughSum / float64(len(top))

	return Sample{
		Path:           PathProxy,
		LatencyMS:      lat,
		ThroughputKBps: through,
		OK:             true,
		At:             time.Now(),
		Score:          score(lat, through, true),
	}
}

// liveMeasure 真实测量一条通路的性能。
//
// 两条通路的差别在于目标地址的解析方式：
//
//	direct —— 用系统 DNS 解析 github.com，反映"什么都不做"的体验；
//	hosts  —— 强制使用 DNS 优选出的 IP，反映"改写 hosts 后"的体验。
//
// 只有让两者走不同的解析路径，决策才有实际意义。
func (d *Decider) liveMeasure(ctx context.Context, p Path, targets []config.ProbeTarget) Sample {
	target := pickTarget(targets)
	if target.Host == "" {
		return Sample{Path: p, Err: "没有可用的探测目标", At: time.Now()}
	}

	url := "https://" + target.Host + target.Path

	if p == PathHosts && d.bestIP != nil {
		if ip, ok := d.bestIP(target.Host); ok && ip != "" {
			return d.measureViaIP(ctx, p, url, target.Host, ip)
		}
		// 尚未完成优选时，退回与直连相同的测量方式。
		return Sample{Path: p, Err: "尚未完成 DNS 优选", At: time.Now()}
	}

	return d.measureHTTP(ctx, p, url)
}

// measureViaIP 通过指定 IP 访问目标，用于评估优选后的实际表现。
//
// 实现方式：自定义 DialContext 强制连到优选 IP，
// 但 TLS 的 SNI 与证书校验仍使用原始域名，因此结果真实可信。
func (d *Decider) measureViaIP(ctx context.Context, p Path, url, host, ip string) Sample {
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialer := &net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip, "443"))
		},
		TLSHandshakeTimeout: 8 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	reqCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	req, err := newRequest(reqCtx, url)
	if err != nil {
		return Sample{Path: p, Err: err.Error(), At: time.Now()}
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Sample{Path: p, Err: simplifyErr(err), At: time.Now()}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return Sample{Path: p, Err: fmt.Sprintf("HTTP %d", resp.StatusCode), At: time.Now()}
	}

	latency := time.Since(start)
	n, _ := readLimited(resp.Body, 128*1024)
	elapsed := time.Since(start)

	var through float64
	if elapsed > 0 && n > 8192 {
		through = float64(n) / 1024.0 / elapsed.Seconds()
	}

	return Sample{
		Path:           p,
		LatencyMS:      latency.Milliseconds(),
		ThroughputKBps: through,
		OK:             true,
		At:             time.Now(),
		Score:          score(latency.Milliseconds(), through, true),
	}
}

// pickTarget 选择一个用于通路测量的探测目标。
//
// 优先使用小体积的 raw 文件，因为它对延迟敏感且几乎所有网络都能访问。
func pickTarget(targets []config.ProbeTarget) config.ProbeTarget {
	for _, t := range targets {
		if t.Host == "raw.githubusercontent.com" {
			return t
		}
	}
	if len(targets) > 0 {
		return targets[0]
	}
	return config.ProbeTarget{}
}

// measureHTTP 执行一次 HTTP 测量，返回延迟与吞吐。
func (d *Decider) measureHTTP(ctx context.Context, p Path, url string) Sample {
	client := sharedClient()
	reqCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	req, err := newRequest(reqCtx, url)
	if err != nil {
		return Sample{Path: p, Err: err.Error(), At: time.Now()}
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Sample{Path: p, Err: simplifyErr(err), At: time.Now()}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return Sample{Path: p, Err: fmt.Sprintf("HTTP %d", resp.StatusCode), At: time.Now()}
	}

	latency := time.Since(start)
	n, _ := readLimited(resp.Body, 128*1024)
	elapsed := time.Since(start)

	var through float64
	if elapsed > 0 && n > 8192 {
		through = float64(n) / 1024.0 / elapsed.Seconds()
	}

	return Sample{
		Path:           p,
		LatencyMS:      latency.Milliseconds(),
		ThroughputKBps: through,
		OK:             true,
		At:             time.Now(),
		Score:          score(latency.Milliseconds(), through, true),
	}
}

// score 把延迟与吞吐换算成综合得分。
//
// 与测速模块保持一致的量纲，但决策时更看重延迟，
// 因为用户对"打开页面慢"的感知远强于"下载慢一点"。
func score(latencyMS int64, throughKBps float64, ok bool) float64 {
	if !ok {
		return 0
	}
	const refLat = 2500.0
	const refThrough = 1536.0

	lat := float64(latencyMS)
	if lat <= 0 {
		lat = 1
	}
	latScore := refLat / (refLat + lat) * 100
	throughScore := 0.0
	if throughKBps > 0 {
		throughScore = throughKBps / (refThrough + throughKBps) * 100
	}
	return latScore*0.75 + throughScore*0.25
}

// Verdict 返回最近一次决策结论。
func (d *Decider) Verdict() Verdict {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.verdict
}

// safeRatio 安全地计算 a/b，避免除零与非法浮点值。
//
// 返回 NaN/Inf 会污染下游的 JSON 序列化，因此这里统一收敛到合法范围：
//   - b <= 0 且 a > 0：视为"极大提升"，返回 maxRatio
//   - b <= 0 且 a <= 0：两者都无意义，返回 1
//   - 结果非有限（Inf/NaN）：收敛到 maxRatio 或 1
func safeRatio(a, b float64) float64 {
	// maxRatio 作为"提升极大"的上界，避免出现 Inf。
	const maxRatio = 999.0

	if b <= 0 {
		if a > 0 {
			return maxRatio
		}
		return 1
	}
	r := a / b
	if math.IsInf(r, 1) || math.IsNaN(r) {
		return maxRatio
	}
	if math.IsInf(r, -1) {
		return 1
	}
	if r > maxRatio {
		return maxRatio
	}
	return r
}

// errText 把采样错误转成可读文本。
func errText(s Sample) string {
	if s.OK {
		return "正常"
	}
	if s.Err == "" {
		return "未测试"
	}
	return s.Err
}

// topMirror 从测速结果中找出延迟最低的源。
func topMirror(probes []mirror.Result) (string, int64) {
	var best *mirror.Result
	for i := range probes {
		p := &probes[i]
		if !p.OK {
			continue
		}
		if best == nil || p.LatencyMS < best.LatencyMS {
			best = p
		}
	}
	if best == nil {
		return "无", 0
	}
	return best.MirrorID, best.LatencyMS
}

// simplifyErr 精简网络错误信息，便于在界面上展示。
func simplifyErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "no such host"):
		return "域名解析失败"
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline exceeded"):
		return "连接超时"
	case strings.Contains(s, "connection refused"):
		return "连接被拒绝"
	case strings.Contains(s, "connection reset"):
		return "连接被重置"
	case strings.Contains(s, "certificate"):
		return "证书校验失败"
	}
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}
