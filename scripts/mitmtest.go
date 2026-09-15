//go:build ignore

// mitmtest 专门验证 HTTPS 中间人加速是否生效。
//
// 判断依据：用代理访问 GitHub 网页，若返回的是被加速后的正常内容，
// 说明 CONNECT 隧道 + TLS 中间人 + HTTP 转发全链路打通。
//
// 用法：go run scripts/mitmtest.go
package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func main() {
	proxyURL, _ := url.Parse("http://127.0.0.1:17710")

	// 信任本机自签 CA：这里直接跳过校验，效果等同于安装了根证书。
	transport := &http.Transport{
		Proxy:           http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	client := &http.Client{Transport: transport, Timeout: 60 * time.Second}

	cases := []struct {
		name string
		url  string
		want []string
	}{
		{
			name: "GitHub 首页",
			url:  "https://github.com/",
			want: []string{"github", "<html"},
		},
		{
			name: "raw 文件",
			url:  "https://raw.githubusercontent.com/github/gitignore/main/Go.gitignore",
			want: []string{"*.exe", "*.test"},
		},
		{
			name: "API",
			url:  "https://api.github.com/meta",
			want: []string{"verifiable_password_authentication"},
		},
		{
			name: "codeload 归档",
			url:  "https://codeload.github.com/github/gitignore/tar.gz/refs/heads/main",
			want: []string{},
		},
	}

	fmt.Println("===== HTTPS 中间人加速验证 =====")
	pass := 0

	for _, c := range cases {
		req, _ := http.NewRequest("GET", c.url, nil)
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) GhppTest/1.0")

		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("  [失败] %-16s %v\n", c.name, err)
			continue
		}

		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		resp.Body.Close()
		elapsed := time.Since(start)

		text := strings.ToLower(string(body))
		matched := true
		for _, w := range c.want {
			if !strings.Contains(text, strings.ToLower(w)) {
				matched = false
				break
			}
		}

		status := "通过"
		if resp.StatusCode >= 400 || !matched {
			status = "失败"
		} else {
			pass++
		}

		fmt.Printf("  [%s] %-16s HTTP %d，%d 字节，耗时 %dms\n",
			status, c.name, resp.StatusCode, len(body), elapsed.Milliseconds())

		if !matched {
			// 打印片段便于判断返回的是错误页还是被劫持的内容。
			snippet := string(body)
			if len(snippet) > 160 {
				snippet = snippet[:160]
			}
			fmt.Printf("         内容不匹配，实际开头: %q\n", snippet)
		}
	}

	fmt.Printf("\n%d/%d 项通过\n", pass, len(cases))
}
