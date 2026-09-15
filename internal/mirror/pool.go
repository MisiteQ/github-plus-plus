// Package mirror 实现镜像源的连通性探测、吞吐测速与择优调度。
//
// 调度策略说明：
//  1. 每个源维护一个随时间衰减的指数移动得分，兼顾稳定性与实时性；
//  2. 得分由首字节延迟和下载吞吐加权得出，延迟权重更高以贴合交互体验；
//  3. 连续失败达到阈值后源进入冷却期，冷却结束自动回归参与竞争；
//  4. 选择时在前若干名中做一次按权重的随机抖动，避免所有请求压在同一个源上。
package mirror

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ghpp/ghpp/internal/config"
)

// Result 是一次单源测速的原始结果。
type Result struct {
	// MirrorID 是被测源的标识。
	MirrorID string `json:"mirror_id"`
	// OK 表示本次探测是否成功。
	OK bool `json:"ok"`
	// LatencyMS 是首字节延迟毫秒数。
	LatencyMS int64 `json:"latency_ms"`
	// ThroughputKBps 是实测下载吞吐，单位 KB/s。
	ThroughputKBps float64 `json:"throughput_kbps"`
	// Bytes 是本次实际读取的字节数。
	Bytes int64 `json:"bytes"`
	// Err 记录失败原因。
	Err string `json:"error,omitempty"`
	// At 是测速发生的时刻。
	At time.Time `json:"at"`
	// Score 是归一化后的综合得分，越高越好。
	Score float64 `json:"score"`
}

// stat 保存单个源的运行期统计与得分。
type stat struct {
	id       string
	score    float64   // 综合得分，0 表示尚无有效数据
	latency  float64   // 平滑后的延迟毫秒
	through  float64   // 平滑后的吞吐 KB/s
	okCount  int64     // 累计成功次数
	badCount int64     // 累计失败次数
	failStreak int     // 当前连续失败次数
	cooldown time.Time // 冷却截止时刻
	updated  time.Time
	lastErr  string
}

// ProbeFunc 抽象一次真实的网络探测，便于测试时替换为假实现。
type ProbeFunc func(ctx context.Context, m config.Mirror, target config.ProbeTarget, maxBytes int64) (Result, error)

// Pool 是镜像源池，负责测速与择优。
type Pool struct {
	mu       sync.RWMutex
	stats    map[string]*stat
	results  map[string]Result
	probe    ProbeFunc
	client   *http.Client
	fastClient *http.Client

	// 最近一次全量测速的完成时间。
	lastSweep time.Time
	sweeping  bool

	// rand 用于在候选集内做抖动选择。
	rand *rand.Rand
}

// NewPool 创建镜像源池。
func NewPool() *Pool {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   6 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   6 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ForceAttemptHTTP2:     true,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	fastTransport := transport.Clone()
	fastTransport.TLSHandshakeTimeout = 4 * time.Second

	p := &Pool{
		stats:   make(map[string]*stat),
		results: make(map[string]Result),
		client: &http.Client{
			Transport: transport,
			Timeout:   25 * time.Second,
		},
		fastClient: &http.Client{
			Transport: fastTransport,
			Timeout:   8 * time.Second,
		},
		rand: rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	p.probe = p.httpProbe
	return p
}

// weights 定义打分权重：延迟占七成，吞吐占三成。
// 交互式场景（打开网页、拉取小文件）对延迟更敏感，因此延迟权重更高。
const (
	weightLatency = 0.7
	weightThrough = 0.3

	// 打分参考基准，用于把物理量映射到 0~100 区间。
	refLatencyMS   = 3000.0
	refThroughKBps = 2048.0
)

// scoreOf 把延迟与吞吐换算为 0~100 的综合得分。
func scoreOf(latencyMS, throughKBps float64) float64 {
	if latencyMS <= 0 {
		latencyMS = refLatencyMS
	}
	latScore := refLatencyMS / (refLatencyMS + latencyMS) * 100
	throughScore := throughKBps / (refThroughKBps + throughKBps) * 100
	return latScore*weightLatency + throughScore*weightThrough
}

// Record 记录一次请求的实际表现，用于在线学习源的优劣。
//
// 与主动测速不同，这里用的是真实代理流量，数据更有代表性。
func (p *Pool) Record(id string, latency time.Duration, bytes int64, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	st := p.stats[id]
	if st == nil {
		st = &stat{id: id}
		p.stats[id] = st
	}

	if err != nil {
		st.badCount++
		st.failStreak++
		st.lastErr = err.Error()
		// 失败时给一个惩罚分，让调度器自然避开它。
		st.score *= 0.5
		st.updated = time.Now()
		return
	}

	st.okCount++
	st.failStreak = 0
	st.lastErr = ""

	lat := float64(latency.Milliseconds())
	if lat <= 0 {
		lat = 1
	}
	var through float64
	if latency > 0 && bytes > 0 {
		through = float64(bytes) / 1024.0 / latency.Seconds()
	}

	if st.score == 0 {
		st.latency, st.through = lat, through
	} else {
		// 指数平滑，衰减系数 0.3 让新样本有足够话语权同时避免抖动。
		st.latency = st.latency*0.7 + lat*0.3
		st.through = st.through*0.7 + through*0.3
	}
	st.score = scoreOf(st.latency, st.through)
	st.updated = time.Now()
}

// ShouldEvict 判断某个源是否已连续失败到需要临时摘除。
func (p *Pool) ShouldEvict(id string, threshold int, cooldown time.Duration) (bool, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.stats[id]
	if st == nil || st.failStreak < threshold {
		return false, 0
	}
	until := time.Now().Add(cooldown)
	if st.cooldown.Before(until) {
		st.cooldown = until
	}
	return true, cooldown
}

// inCooldown 在持锁状态下判断源是否处于冷却期。
func (p *Pool) inCooldownLocked(id string) bool {
	st := p.stats[id]
	if st == nil {
		return false
	}
	return time.Now().Before(st.cooldown)
}

// Ranked 返回按得分降序排列的启用源。
//
// 无有效成绩的源获得一个中等偏上的默认分，保证它们能被试用到并积累数据。
func (p *Pool) Ranked(mirrors []config.Mirror, category string) []config.Mirror {
	p.mu.RLock()
	defer p.mu.RUnlock()

	type entry struct {
		m     config.Mirror
		score float64
	}
	entries := make([]entry, 0, len(mirrors))
	for _, m := range mirrors {
		if !m.Enabled || !supports(m, category) {
			continue
		}
		if p.inCooldownLocked(m.ID) {
			continue
		}
		st := p.stats[m.ID]
		score := 45.0 // 未测速源的初始分，略低于及格线以鼓励先测速。
		if st != nil && st.score > 0 {
			score = st.score
		}
		score *= m.Weight
		entries = append(entries, entry{m: m, score: score})
	}

	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].score > entries[j].score
	})
	out := make([]config.Mirror, len(entries))
	for i, e := range entries {
		out[i] = e.m
	}
	return out
}

// Pick 从候选源中挑选一个用于本次请求的源。
//
// 在前 topN 名中按得分加权随机，既优先使用最优源，又避免单点压力过大。
func (p *Pool) Pick(mirrors []config.Mirror, category string) (config.Mirror, bool) {
	ranked := p.Ranked(mirrors, category)
	if len(ranked) == 0 {
		return config.Mirror{}, false
	}

	const topN = 3
	n := len(ranked)
	if n > topN {
		n = topN
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// 以 1/score^2 之外的简单方式加权：得分越高权重越大。
	weights := make([]float64, n)
	var total float64
	for i := 0; i < n; i++ {
		st := p.stats[ranked[i].ID]
		w := 1.0
		if st != nil && st.score > 0 {
			w = st.score * ranked[i].Weight
		}
		// 第 i 名额外加成，保证排名靠前的源被优先选择。
		w *= 1.0 / (1.0 + float64(i)*0.6)
		if w <= 0 {
			w = 0.01
		}
		weights[i] = w
		total += w
	}

	r := p.rand.Float64() * total
	for i := 0; i < n; i++ {
		r -= weights[i]
		if r <= 0 {
			return ranked[i], true
		}
	}
	return ranked[0], true
}

// Supports 判断某个源是否适用于该请求类别。
func Supports(m config.Mirror, category string) bool {
	return supports(m, category)
}

// supports 判断某个源是否适用于该请求类别。
func supports(m config.Mirror, category string) bool {
	if m.Kind == config.KindDirect {
		return true
	}
	switch category {
	case config.CatRaw:
		// raw 文件既可以用 raw CDN，也可以用前缀型中转。
		return m.Kind == config.KindRawCDN || m.Kind == config.KindPrefix
	case config.CatClone:
		return m.Kind == config.KindGitClone || m.Kind == config.KindPrefix
	case config.CatWeb:
		// 网页与 API 只能走全站型中转，raw CDN 无法处理。
		return m.Kind == config.KindPrefix
	default:
		return m.Kind == config.KindPrefix
	}
}

// Sweep 对全部启用的源执行一轮测速。
//
// 返回本轮的全部结果，同时更新内部得分表。
func (p *Pool) Sweep(ctx context.Context, mirrors []config.Mirror, targets []config.ProbeTarget, maxBytes int64) []Result {
	p.mu.Lock()
	if p.sweeping {
		p.mu.Unlock()
		return nil
	}
	p.sweeping = true
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		p.sweeping = false
		p.lastSweep = time.Now()
		p.mu.Unlock()
	}()

	type job struct {
		m config.Mirror
		t config.ProbeTarget
	}
	jobs := make([]job, 0, len(mirrors)*len(targets))
	for _, m := range mirrors {
		if !m.Enabled {
			continue
		}
		for _, t := range targets {
			if !supports(m, t.Category) {
				continue
			}
			jobs = append(jobs, job{m: m, t: t})
		}
	}

	const concurrency = 8
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	results := make([]Result, 0, len(jobs))

	for _, j := range jobs {
		select {
		case <-ctx.Done():
			wg.Wait()
			return results
		default:
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(j job) {
			defer wg.Done()
			defer func() { <-sem }()

			res, err := p.probe(ctx, j.m, j.t, maxBytes)
			if err != nil {
				res = Result{MirrorID: j.m.ID, OK: false, Err: err.Error()}
			}
			res.MirrorID = j.m.ID
			res.At = time.Now()
			res.Score = scoreOf(float64(res.LatencyMS), res.ThroughputKBps)
			if !res.OK {
				res.Score = 0
			}

			mu.Lock()
			results = append(results, res)
			mu.Unlock()

			p.applyProbe(j.m, res)
		}(j)
	}
	wg.Wait()
	return results
}

// applyProbe 把一次主动测速结果并入得分表。
func (p *Pool) applyProbe(m config.Mirror, res Result) {
	p.mu.Lock()
	defer p.mu.Unlock()

	st := p.stats[m.ID]
	if st == nil {
		st = &stat{id: m.ID}
		p.stats[m.ID] = st
	}
	if !res.OK {
		st.badCount++
		st.failStreak++
		st.lastErr = res.Err
		st.score *= 0.6
		if st.score < 1 {
			st.score = 0
		}
		st.updated = res.At
		return
	}

	st.okCount++
	st.failStreak = 0
	st.lastErr = ""
	st.cooldown = time.Time{}

	lat := float64(res.LatencyMS)
	through := res.ThroughputKBps
	if st.score == 0 {
		st.latency, st.through = lat, through
	} else {
		st.latency = st.latency*0.5 + lat*0.5
		st.through = st.through*0.5 + through*0.5
	}
	st.score = scoreOf(st.latency, st.through)
	st.updated = res.At
}

// httpProbe 是默认的真实网络测速实现。
//
// 流程：建立连接并记录首字节延迟，然后限速读取 maxBytes 字节来估算吞吐。
func (p *Pool) httpProbe(ctx context.Context, m config.Mirror, t config.ProbeTarget, maxBytes int64) (Result, error) {
	targetURL := rewriteURL(m, t.Host, t.Path)

	reqCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return Result{}, fmt.Errorf("构造探测请求失败: %w", err)
	}
	req.Header.Set("User-Agent", config.ProbeUserAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", maxBytes-1))

	start := time.Now()
	resp, err := p.fastClient.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("连接失败: %w", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode >= 400 {
		return Result{}, fmt.Errorf("返回状态码 %d", resp.StatusCode)
	}

	latency := time.Since(start)

	// 读取固定字节数用于吞吐估算，读取量本身就等于实际传输量。
	n, readErr := io.Copy(io.Discard, io.LimitReader(resp.Body, maxBytes))
	elapsed := time.Since(start)

	if n == 0 && readErr != nil {
		return Result{}, fmt.Errorf("读取响应失败: %w", readErr)
	}

	var through float64
	if elapsed > 0 {
		through = float64(n) / 1024.0 / elapsed.Seconds()
	}

	// 读取太少的数据无法反映真实吞吐，此时只保留延迟成绩。
	if n < 8192 {
		through = 0
	}

	return Result{
		OK:             true,
		LatencyMS:      latency.Milliseconds(),
		ThroughputKBps: through,
		Bytes:          n,
	}, nil
}

// rewriteURL 依据源类型把 GitHub 的 host+path 改写成实际请求地址。
//
// 这是整个加速器的核心改写规则：
//
//	direct   -> https://github.com/owner/repo/...
//	prefix   -> https://ghproxy.net/https://github.com/owner/repo/...
//	raw CDN  -> https://fastly.jsdelivr.net/gh/owner/repo@ref/path（jsDelivr 专用语义）
//	git      -> https://gitclone.com/github.com/owner/repo.git
func rewriteURL(m config.Mirror, host, path string) string {
	switch m.Kind {
	case config.KindDirect:
		return "https://" + host + path

	case config.KindPrefix:
		base := strings.TrimSuffix(m.URL, "/")
		return base + "/https://" + host + path

	case config.KindRawCDN:
		// jsDelivr 有专有的 gh 前缀语法，需要把 raw 路径翻译过去。
		if strings.Contains(m.URL, "jsdelivr.net") {
			if u, ok := jsdelivrURL(host, path); ok {
				base := strings.TrimSuffix(m.URL, "/")
				if !strings.Contains(base, "/gh") {
					base += "/gh"
				}
				return base + u
			}
		}
		base := strings.TrimSuffix(m.URL, "/")
		return base + "/https://" + host + path

	case config.KindGitClone:
		base := strings.TrimSuffix(m.URL, "/")
		return base + "/https://" + host + path

	default:
		return "https://" + host + path
	}
}

// jsdelivrURL 把 raw.githubusercontent.com 的路径翻译成 jsDelivr 的 gh 语法。
//
// raw 路径形如 /owner/repo/ref/path/to/file，
// jsDelivr 形如 /owner/repo@ref/path/to/file。
// 由于 ref 中可能含有斜杠（如 feature/foo），这里采用启发式拆分：
// 逐段尝试，找到第一个能让后续路径成立的 ref 组合。
func jsdelivrURL(host, path string) (string, bool) {
	if host != "raw.githubusercontent.com" && host != "gist.githubusercontent.com" {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) < 4 {
		return "", false
	}

	owner, repo := parts[0], parts[1]
	rest := parts[2:]

	// 优先尝试常见分支名，命中率最高。
	branches := []string{"main", "master", "gh-pages", "develop", "dev"}
	for _, b := range branches {
		if len(rest) > 1 && rest[0] == b {
			return "/" + owner + "/" + repo + "@" + b + "/" + strings.Join(rest[1:], "/"), true
		}
	}

	// 回退策略：把第一段当作 ref。
	if len(rest) >= 2 {
		return "/" + owner + "/" + repo + "@" + rest[0] + "/" + strings.Join(rest[1:], "/"), true
	}
	return "", false
}

// Stats 返回所有源的运行期统计快照，供 API 展示。
func (p *Pool) Stats() map[string]Result {
	p.mu.RLock()
	defer p.mu.RUnlock()

	out := make(map[string]Result, len(p.stats))
	for id, st := range p.stats {
		r := Result{
			MirrorID:       id,
			OK:             st.score > 0,
			LatencyMS:      int64(st.latency),
			ThroughputKBps: st.through,
			Score:          st.score,
			At:             st.updated,
			Err:            st.lastErr,
		}
		if time.Now().Before(st.cooldown) {
			r.Err = fmt.Sprintf("%s（冷却至 %s）", st.lastErr, st.cooldown.Format("15:04:05"))
		}
		out[id] = r
	}
	return out
}

// LastSweep 返回最近一次全量测速的时间。
func (p *Pool) LastSweep() time.Time {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.lastSweep
}

// ScoreHalfLifeDecay 按半衰期衰减历史得分。
//
// 长时间未更新的源其成绩会逐渐失去参考价值，这里把它拉回未测速的默认分，
// 迫使调度器重新试用该源。
func (p *Pool) ScoreHalfLifeDecay(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, st := range p.stats {
		if st.updated.IsZero() {
			continue
		}
		age := now.Sub(st.updated)
		if age <= config.ScoreHalfLife {
			continue
		}
		halves := age.Seconds() / config.ScoreHalfLife.Seconds()
		factor := math.Pow(0.5, halves)
		if factor < 0.05 {
			factor = 0.05
		}
		// 向默认分 45 收敛，而不是直接归零。
		st.score = 45 + (st.score-45)*factor
	}
}

// ResetCooldown 立即清除所有源的冷却状态，供用户手动重测时使用。
func (p *Pool) ResetCooldown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, st := range p.stats {
		st.cooldown = time.Time{}
		st.failStreak = 0
	}
}

// 保证 url 包被使用（rewriteURL 返回字符串，但保留此引用以便后续扩展）。
var _ = url.Parse
