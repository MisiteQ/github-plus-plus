//go:build ignore

// dnsprobe 是 DNS 优选模块的诊断工具，用于在真实网络下排查候选收集与测速问题。
//
// 用法：go run scripts/dnsprobe.go
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

func main() {
	hosts := []string{
		"github.com",
		"api.github.com",
		"raw.githubusercontent.com",
		"codeload.github.com",
	}

	fmt.Println("===== DoH 候选收集诊断 =====")
	for _, h := range hosts {
		ips := collect(h)
		fmt.Printf("%-32s 收集到 %d 个候选: %v\n", h, len(ips), ips)
	}

	fmt.Println("\n===== IP 连通性测速 =====")
	for _, h := range hosts {
		ips := collect(h)
		if len(ips) == 0 {
			fmt.Printf("%-32s 无候选，跳过\n", h)
			continue
		}
		for _, ip := range ips {
			ok, lat, tlsMS, err := probe(h, ip)
			status := "可用"
			if !ok {
				status = "失败"
			}
			fmt.Printf("%-32s %-16s %-4s TCP=%4dms TLS=%4dms %s\n",
				h, ip, status, lat, tlsMS, err)
		}
	}
}

// collect 从多个公共 DoH 服务收集域名的 A 记录。
func collect(host string) []string {
	endpoints := []string{
		"https://dns.alidns.com/resolve?name=%s&type=A",
		"https://doh.pub/dns-query?name=%s&type=A",
		"https://doh.360.cn/resolve?name=%s&type=A",
	}

	client := &http.Client{Timeout: 8 * time.Second}
	seen := map[string]bool{}

	for _, ep := range endpoints {
		url := fmt.Sprintf(ep, host)
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			continue
		}
		req.Header.Set("Accept", "application/dns-json")

		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("  [%s] 查询失败: %v\n", url[:32], err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		if resp.StatusCode != 200 {
			fmt.Printf("  [%s] HTTP %d\n", url[:32], resp.StatusCode)
			continue
		}

		// 简单提取所有 IPv4 字符串，避免引入 JSON 结构依赖。
		for _, m := range findIPv4(string(body)) {
			seen[m] = true
		}
	}

	out := make([]string, 0, len(seen))
	for ip := range seen {
		out = append(out, ip)
	}
	return out
}

// findIPv4 从文本中提取所有 IPv4 字面量。
func findIPv4(s string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			continue
		}
		j := i
		for j < len(s) && (s[j] == '.' || (s[j] >= '0' && s[j] <= '9')) {
			j++
		}
		cand := s[i:j]
		if strings.Count(cand, ".") == 3 {
			if ip := net.ParseIP(cand); ip != nil && ip.To4() != nil && !seen[cand] {
				seen[cand] = true
				out = append(out, cand)
			}
		}
		i = j
	}
	return out
}

// probe 对单个 IP 做 TCP + TLS 握手测速。
func probe(host, ip string) (bool, int64, int64, string) {
	addr := net.JoinHostPort(ip, "443")

	t0 := time.Now()
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).Dial("tcp", addr)
	if err != nil {
		return false, 0, 0, "TCP: " + shortErr(err)
	}
	tcpMS := time.Since(t0).Milliseconds()

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	t1 := time.Now()
	if err := tlsConn.Handshake(); err != nil {
		conn.Close()
		return false, tcpMS, 0, "TLS: " + shortErr(err)
	}
	tlsMS := time.Since(t1).Milliseconds()
	tlsConn.Close()

	return true, tcpMS, tlsMS, ""
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 70 {
		return s[:70] + "..."
	}
	return s
}

var _ = context.Background
