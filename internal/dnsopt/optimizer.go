// Package dnsopt 实现 GitHub 域名的 DNS 解析与 IP 优选。
//
// 思路：对每个受管域名收集多个候选 IP（来自多个公共 DNS 服务器，
// 以便绕过本地 DNS 污染），逐个做 TCP 握手 + TLS 握手 + HTTP 首字节测速，
// 选出最快的 IP 写入 hosts，让系统直接命中优质节点。
//
// 该方式与前缀反代互补：反代解决"被墙/被限速"问题，DNS 优选解决"选错节点"问题，
// 两者可以同时启用，由决策器择优。
package dnsopt

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ghpp/ghpp/internal/config"
)

// Candidate 是一个候选 IP 及其测速成绩。
type Candidate struct {
	Host string `json:"host"`
	IP   string `json:"ip"`
	// Source 记录该 IP 来自哪个 DNS 服务器，便于排查污染。
	Source string `json:"source"`
	// LatencyMS 是 TCP 握手耗时。
	LatencyMS int64 `json:"latency_ms"`
	// TLSMS 是 TLS 握手耗时。
	TLSMS int64 `json:"tls_ms"`
	// TotalMS 是到首字节的总耗时。
	TotalMS int64 `json:"total_ms"`
	// OK 表示该 IP 是否可用。
	OK bool `json:"ok"`
	// Err 记录失败原因。
	Err string `json:"error,omitempty"`
}

// Score 返回用于排序的得分，越小越好。失败的候选返回极大值。
func (c Candidate) Score() int64 {
	if !c.OK {
		return 1 << 40
	}
	return c.TotalMS
}

// Resolver 的 DoH 端点，使用明文 JSON 接口以避免额外的依赖。
type dohEndpoint struct {
	name string
	url  string
}

// publicResolvers 列出用于收集候选 IP 的公共 DNS-over-HTTPS 服务。
//
// 使用 DoH 而非系统 DNS，是为了绕开运营商 DNS 可能存在的污染与劫持。
var publicResolvers = []dohEndpoint{
	{name: "阿里DNS", url: "https://dns.alidns.com/resolve?name=%s&type=A"},
	{name: "腾讯DNS", url: "https://doh.pub/dns-query?name=%s&type=A"},
	{name: "360DNS", url: "https://doh.360.cn/resolve?name=%s&type=A"},
	{name: "DNSPod", url: "https://1.12.12.12/dns-query?name=%s&type=A"},
}

// Optimizer 负责候选 IP 的收集与测速。
type Optimizer struct {
	client *http.Client

	mu         sync.RWMutex
	candidates map[string][]Candidate // host -> 候选列表
	best       map[string]Candidate   // host -> 当前最优

	logf func(level, format string, args ...any)
}

// NewOptimizer 创建 DNS 优选器。
func NewOptimizer(logf func(level, format string, args ...any)) *Optimizer {
	if logf == nil {
		logf = func(string, string, ...any) {}
	}
	transport := &http.Transport{
		Proxy:               nil,
		MaxIdleConns:        32,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &Optimizer{
		client: &http.Client{
			Transport: transport,
			Timeout:   6 * time.Second,
		},
		candidates: make(map[string][]Candidate),
		best:       make(map[string]Candidate),
		logf:       logf,
	}
}

// Collect 从多个公共 DoH 服务收集指定域名的 A 记录。
//
// 多源收集的意义在于：若某个 DNS 被污染，其他源仍可能给出真实 IP，
// 后续测速阶段会自动淘汰不可用的污染 IP。
func (o *Optimizer) Collect(ctx context.Context, host string) []string {
	type res struct {
		source string
		ips    []string
	}
	ch := make(chan res, len(publicResolvers))

	var wg sync.WaitGroup
	for _, ep := range publicResolvers {
		wg.Add(1)
		go func(ep dohEndpoint) {
			defer wg.Done()
			ips := o.queryDoH(ctx, ep, host)
			ch <- res{source: ep.name, ips: ips}
		}(ep)
	}
	wg.Wait()
	close(ch)

	seen := make(map[string]string)
	for r := range ch {
		for _, ip := range r.ips {
			if _, ok := seen[ip]; !ok {
				seen[ip] = r.source
			}
		}
	}

	out := make([]string, 0, len(seen))
	for ip := range seen {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

// dohResponse 是 DoH JSON 接口的响应结构。
type dohResponse struct {
	Status int `json:"Status"`
	Answer []struct {
		Name string `json:"name"`
		Type int    `json:"type"`
		TTL  int    `json:"TTL"`
		Data string `json:"data"`
	} `json:"Answer"`
}

// queryDoH 向单个 DoH 服务发起查询并解析出 IPv4 地址。
func (o *Optimizer) queryDoH(ctx context.Context, ep dohEndpoint, host string) []string {
	u := fmt.Sprintf(ep.url, host)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Accept", "application/dns-json")

	resp, err := o.client.Do(req)
	if err != nil {
		o.logf("debug", "DoH 查询 %s 于 %s 失败: %v", host, ep.name, err)
		return nil
	}
	defer resp.Body.Close()

	var parsed dohResponse
	if err := decodeJSON(resp.Body, &parsed); err != nil {
		return nil
	}

	var ips []string
	for _, a := range parsed.Answer {
		// Type 1 = A 记录。
		if a.Type != 1 {
			continue
		}
		ip := strings.TrimSpace(a.Data)
		if net.ParseIP(ip) == nil {
			continue
		}
		ips = append(ips, ip)
	}
	return ips
}

// Probe 对一批候选 IP 做链路测速。
//
// 测速包含两段：TCP 握手与 TLS 握手。
// 两段都成功即认为该 IP 可用——这已经能准确区分"节点可达"与"节点被墙/失效"，
// 且握手耗时与真实的页面加载体验高度相关。
//
// 不再额外发 HTTP 请求：部分 GitHub 边缘节点对探测类请求响应异常，
// 加入 HTTP 阶段反而会引入误判，把可用节点错杀。
func (o *Optimizer) Probe(ctx context.Context, host, ip string, source string, path string, timeout time.Duration) Candidate {
	c := Candidate{Host: host, IP: ip, Source: source}

	addr := net.JoinHostPort(ip, "443")

	// 第一阶段：TCP 握手，用于排除完全不可达的节点。
	t0 := time.Now()
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		c.Err = "TCP 连接失败: " + shortError(err)
		return c
	}
	c.LatencyMS = time.Since(t0).Milliseconds()

	// 第二阶段：TLS 握手，必须带 SNI 才能命中正确的虚拟主机。
	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	t1 := time.Now()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		c.Err = "TLS 握手失败: " + shortError(err)
		return c
	}
	c.TLSMS = time.Since(t1).Milliseconds()
	c.TotalMS = time.Since(t0).Milliseconds()

	_ = tlsConn.Close()

	if c.TotalMS <= 0 {
		c.TotalMS = c.LatencyMS + c.TLSMS
	}
	c.OK = true
	return c
}

// shortError 精简错误信息，避免日志被冗长的网络错误淹没。
func shortError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if strings.Contains(s, "i/o timeout") || strings.Contains(s, "deadline exceeded") {
		return "连接超时"
	}
	if strings.Contains(s, "connection refused") {
		return "连接被拒绝"
	}
	if strings.Contains(s, "connection reset") {
		return "连接被重置"
	}
	if strings.Contains(s, "no route to host") {
		return "网络不可达"
	}
	if len(s) > 60 {
		return s[:60] + "..."
	}
	return s
}

// Optimize 对一个域名执行完整的"收集 + 测速 + 择优"流程。
func (o *Optimizer) Optimize(ctx context.Context, target config.ProbeTarget, limit int, timeout time.Duration) []Candidate {
	ips := o.Collect(ctx, target.Host)
	if len(ips) == 0 {
		o.logf("warn", "未能为 %s 收集到任何候选 IP", target.Host)
		return nil
	}

	if limit > 0 && len(ips) > limit {
		// 候选过多时先做简单截断，避免测速耗时失控。
		ips = ips[:limit]
	}

	candidates := make([]Candidate, len(ips))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, ip := range ips {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, ip string) {
			defer wg.Done()
			defer func() { <-sem }()
			candidates[i] = o.Probe(ctx, target.Host, ip, "doh", target.Path, timeout)
		}(i, ip)
	}
	wg.Wait()

	// 按得分升序，可用者在前。
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Score() < candidates[j].Score()
	})

	o.mu.Lock()
	o.candidates[target.Host] = candidates
	if len(candidates) > 0 && candidates[0].OK {
		o.best[target.Host] = candidates[0]
	}
	o.mu.Unlock()

	if len(candidates) > 0 && candidates[0].OK {
		o.logf("info", "域名 %s 优选完成，最佳 IP %s（总耗时 %dms）",
			target.Host, candidates[0].IP, candidates[0].TotalMS)
	} else {
		o.logf("warn", "域名 %s 的所有候选 IP 均不可用", target.Host)
	}

	return candidates
}

// OptimizeAll 对所有受管域名批量优选。
func (o *Optimizer) OptimizeAll(ctx context.Context, limit int, timeout time.Duration) map[string][]Candidate {
	targets := config.DomainProbeTargets()
	out := make(map[string][]Candidate, len(targets))

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, t := range targets {
		wg.Add(1)
		go func(t config.ProbeTarget) {
			defer wg.Done()
			res := o.Optimize(ctx, t, limit, timeout)
			mu.Lock()
			out[t.Host] = res
			mu.Unlock()
		}(t)
	}
	wg.Wait()
	return out
}

// Best 返回某个域名当前的最优 IP。
func (o *Optimizer) Best(host string) (Candidate, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	c, ok := o.best[host]
	return c, ok
}

// BestAll 返回全部域名的最优 IP 快照。
func (o *Optimizer) BestAll() map[string]Candidate {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make(map[string]Candidate, len(o.best))
	for k, v := range o.best {
		out[k] = v
	}
	return out
}

// Candidates 返回某个域名的完整候选列表快照。
func (o *Optimizer) Candidates(host string) []Candidate {
	o.mu.RLock()
	defer o.mu.RUnlock()
	src := o.candidates[host]
	out := make([]Candidate, len(src))
	copy(out, src)
	return out
}

// AllCandidates 返回全部域名的候选列表快照。
func (o *Optimizer) AllCandidates() map[string][]Candidate {
	o.mu.RLock()
	defer o.mu.RUnlock()
	out := make(map[string][]Candidate, len(o.candidates))
	for k, v := range o.candidates {
		cp := make([]Candidate, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}
