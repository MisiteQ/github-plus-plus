package decide

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/ghpp/ghpp/internal/config"
)

// sharedHTTPClient 是决策模块共用的 HTTP 客户端。
//
// 不设整体超时，改由每次请求的 context 控制；关闭自动重定向，
// 因为决策只关心首包表现，跟随重定向会引入额外噪声。
var (
	clientOnce sync.Once
	client     *http.Client
)

func sharedClient() *http.Client {
	clientOnce.Do(func() {
		transport := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   8 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          64,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   8 * time.Second,
			ForceAttemptHTTP2:     true,
			ExpectContinueTimeout: time.Second,
		}
		client = &http.Client{
			Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	})
	return client
}

// newRequest 构造带标准请求头的 GET 请求。
func newRequest(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", config.ProbeUserAgent)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Cache-Control", "no-cache")
	return req, nil
}

// readLimited 最多读取 n 字节，用于估算吞吐。
func readLimited(r io.Reader, n int64) (int64, error) {
	return io.Copy(io.Discard, io.LimitReader(r, n))
}
