//go:build ignore

// smoke 是端到端冒烟测试脚本，用于在真实网络环境下验证加速器各接口。
//
// 用法：go run scripts/smoke.go -web http://127.0.0.1:17717 -proxy http://127.0.0.1:17710
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

var (
	webBase   = flag.String("web", "http://127.0.0.1:17717", "控制台地址")
	proxyBase = flag.String("proxy", "http://127.0.0.1:17710", "代理地址")
	user      = flag.String("user", "admin", "登录用户名")
	pass      = flag.String("pass", "", "登录密码")
)

var client = &http.Client{
	Timeout: 120 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

var failures int

func main() {
	flag.Parse()
	if *pass == "" {
		fmt.Println("请用 -pass 指定密码（见启动横幅或配置文件）")
		os.Exit(1)
	}

	fmt.Println("========== GitHub++ 冒烟测试 ==========")

	// 1. 健康检查
	health := doJSON("GET", *webBase+"/api/health", nil, "")
	check("健康检查", health != nil)

	// 2. 登录
	login := doJSON("POST", *webBase+"/api/login", map[string]any{
		"username": *user, "password": *pass,
	}, "")
	data, _ := login["data"].(map[string]any)
	token, _ := data["token"].(string)
	check("登录获取令牌", token != "")
	if token == "" {
		report()
		return
	}

	// 3. 状态
	status := doJSON("GET", *webBase+"/api/status", nil, token)
	sdata, _ := status["data"].(map[string]any)
	st, _ := sdata["status"].(map[string]any)
	check("读取运行状态", st != nil)
	if st != nil {
		fmt.Printf("    运行中: %v，生效模式: %v\n", st["running"], st["effective_mode"])
		fmt.Printf("    代理监听: %v\n", st["proxy_addr"])
	}

	// 4. 配置
	cfg := doJSON("GET", *webBase+"/api/config", nil, token)
	cdata, _ := cfg["data"].(map[string]any)
	check("读取配置", cdata != nil)

	// 5. 镜像源列表
	mirrors := doJSON("GET", *webBase+"/api/mirrors", nil, token)
	mdata, _ := mirrors["data"].([]any)
	check("读取加速源列表", len(mdata) > 0)
	fmt.Printf("    共 %d 个加速源\n", len(mdata))

	// 6. 代理转发测试：通过代理访问 GitHub raw 文件
	fmt.Println("\n---------- 代理转发测试 ----------")
	testURLs := []struct {
		name string
		url  string
	}{
		{"raw 文件", "https://raw.githubusercontent.com/github/gitignore/main/Go.gitignore"},
		{"GitHub API", "https://api.github.com/meta"},
		{"Release", "https://github.com/git/git/archive/refs/tags/v2.43.0.tar.gz"},
	}
	for _, t := range testURLs {
		testProxy(t.name, t.url)
	}

	// 7. DNS 优选
	fmt.Println("\n---------- DNS 优选测试 ----------")
	dns := doJSON("GET", *webBase+"/api/dns", nil, token)
	dnsData, _ := dns["data"].(map[string]any)
	check("读取 DNS 优选结果", dnsData != nil)
	if best, ok := dnsData["best"].(map[string]any); ok {
		fmt.Printf("    已优选出 %d 个域名的最佳 IP\n", len(best))
	}

	// 8. 日志
	logs := doJSON("GET", *webBase+"/api/logs?limit=10", nil, token)
	ldata, _ := logs["data"].([]any)
	check("读取日志", len(ldata) > 0)

	// 9. 未认证访问应被拒绝
	unauth := doJSON("GET", *webBase+"/api/status", nil, "")
	check("未认证请求被拒绝", unauth == nil)

	report()
}

// testProxy 通过代理地址访问目标 URL，验证加速通道是否工作。
func testProxy(name, target string) {
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		fmt.Printf("  [失败] %s: 构造请求出错 %v\n", name, err)
		failures++
		return
	}

	// 显式使用代理，模拟真实客户端行为。
	// 信任本机自签 CA，因为中间人模式会用它签发域名证书。
	transport := &http.Transport{
		Proxy:           http.ProxyURL(mustParse(*proxyBase)),
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	c := &http.Client{Transport: transport, Timeout: 90 * time.Second}

	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		fmt.Printf("  [失败] %s: %v\n", name, err)
		failures++
		return
	}
	defer resp.Body.Close()

	// 只读前 256KB 用于估算速度，避免测试耗时过长。
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 256*1024))
	elapsed := time.Since(start)
	speed := float64(n) / 1024 / elapsed.Seconds()

	ok := resp.StatusCode < 400
	mark := "[通过]"
	if !ok {
		mark = "[失败]"
		failures++
	}
	fmt.Printf("  %s %s: HTTP %d，%d 字节，耗时 %dms，速度 %.0f KB/s\n",
		mark, name, resp.StatusCode, n, elapsed.Milliseconds(), speed)
}

func mustParse(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// doJSON 发起请求并解析 JSON 响应。
func doJSON(method, url string, body any, token string) map[string]any {
	var reader io.Reader
	if body != nil {
		buf, _ := json.Marshal(body)
		reader = bytes.NewReader(buf)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil
	}

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil
	}
	return out
}

// check 记录一项断言结果。
func check(name string, ok bool) {
	mark := "[通过]"
	if !ok {
		mark = "[失败]"
		failures++
	}
	fmt.Printf("  %s %s\n", mark, name)
}

// report 输出汇总结果。
func report() {
	fmt.Println("\n=======================================")
	if failures == 0 {
		fmt.Println("全部测试通过")
	} else {
		fmt.Printf("存在 %d 项失败\n", failures)
	}
	fmt.Println("=======================================")
	if failures > 0 {
		os.Exit(1)
	}
}

var _ = strings.TrimSpace
