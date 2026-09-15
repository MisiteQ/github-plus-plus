package proxy

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ghpp/ghpp/internal/config"
)

// sha256Sum 计算字节切片的 SHA-256 摘要。
func sha256Sum(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

// handleConn 处理单个 TCP 连接。
//
// 先窥探首个请求行，据此分流：
//   - CONNECT：客户端要建 HTTPS 隧道，走中间人解密后转发；
//   - 其余：普通 HTTP 请求（代理形式的绝对 URI 或透明反代的相对路径），
//     交给标准库的 HTTP 服务处理。
//
// 窥探会消耗连接上的字节，因此用一个可"退回"已读数据的包装连接，
// 保证上层仍能从头读取完整请求。
func (e *Engine) handleConn(c net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			e.logf("error", "处理连接时发生异常: %v", r)
			_ = c.Close()
		}
	}()

	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))

	br := bufio.NewReaderSize(c, 8192)
	peek, err := br.Peek(7)
	if err != nil && len(peek) < 7 {
		if err != io.EOF {
			e.logf("debug", "读取连接首部失败: %v", err)
		}
		_ = c.Close()
		return
	}
	_ = c.SetReadDeadline(time.Time{})

	if strings.EqualFold(string(peek), "CONNECT") {
		e.handleConnect(c, br)
		return
	}

	e.serveHTTPConn(&replayConn{Conn: c, r: br})
}

// replayConn 包装一个已部分读取的连接，使上层可完整重新读取。
type replayConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *replayConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// serveHTTPConn 用标准库的 HTTP 服务处理单个非 CONNECT 连接。
//
// 通过 net.Pipe 把真实连接桥接到 http.Server：
// 既复用了标准库成熟的 HTTP 解析与连接管理，又绕开了它不支持 CONNECT 的限制。
//
// 关键：连接结束时必须关闭监听器，让 http.Server.Serve 的 Accept 返回
// net.ErrClosed，否则 srv.Serve 会永远阻塞，导致 goroutine 与信号量槽位泄漏。
func (e *Engine) serveHTTPConn(conn net.Conn) {
	client, server := net.Pipe()
	ln := &singleConnListener{conn: client}

	go func() {
		done := make(chan struct{}, 2)
		go func() {
			_, _ = io.Copy(server, conn)
			done <- struct{}{}
		}()
		go func() {
			_, _ = io.Copy(conn, server)
			done <- struct{}{}
		}()
		<-done
		_ = server.Close()
		_ = conn.Close()
		// 关闭监听器使 http.Server.Serve 退出，释放 goroutine 与信号量。
		_ = ln.Close()
	}()

	srv := &http.Server{
		Handler:           e,
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          nil,
	}
	_ = srv.Serve(ln)
}

// singleConnListener 是只提供一个连接的监听器。
//
// 让 http.Server 处理单个已建立的连接：首次 Accept 返回连接，
// 之后阻塞直到 Close 被调用；Close 后 Accept 返回 net.ErrClosed，
// http.Server.Serve 随之返回。
type singleConnListener struct {
	conn   net.Conn
	mu     sync.Mutex
	served bool
	done   chan struct{}
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.served {
		l.served = true
		l.mu.Unlock()
		return l.conn, nil
	}
	done := l.done
	l.mu.Unlock()
	if done != nil {
		<-done
	}
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error {
	l.mu.Lock()
	if l.done == nil {
		l.done = make(chan struct{})
	}
	done := l.done
	l.mu.Unlock()
	select {
	case <-done:
	default:
		close(done)
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

// handleConnect 处理 HTTPS 的 CONNECT 隧道请求。
//
// 两种模式由 mitmEnabled 决定：
//
//	中间人模式（已安装 CA）：解密客户端流量，改写后经加速通道转发，
//	                         浏览器访问 github.com 也能获得加速。
//	透传模式（默认）：仅做 TCP 转发到最优镜像节点，不解密内容，
//	                  对 git 等本身就走代理的场景依然有效。
//
// 采用"先尝试中间人、失败即降级"的策略，避免因客户端不信任 CA 而中断服务。
func (e *Engine) handleConnect(clientConn net.Conn, br *bufio.Reader) {
	line, err := br.ReadString('\n')
	if err != nil {
		_ = clientConn.Close()
		return
	}

	parts := strings.Fields(strings.TrimSpace(line))
	if len(parts) < 2 {
		writeConnectError(clientConn, http.StatusBadRequest, "请求格式错误")
		return
	}

	target := parts[1]
	host := stripPort(target)

	// 读掉剩余请求头直到空行。
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			_ = clientConn.Close()
			return
		}
		if strings.TrimSpace(h) == "" {
			break
		}
	}

	if !e.isManagedHost(host) {
		writeConnectError(clientConn, http.StatusForbidden, "GitHub++ 仅加速 GitHub 相关域名")
		return
	}

	cfg := e.cfg()
	e.bumpCategory(categoryOf(e.hosts, host))

	// 先告知客户端隧道已建立，这是 CONNECT 协议的固定流程。
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		_ = clientConn.Close()
		return
	}

	if ca := e.CurrentCA(); ca != nil {
		if err := e.serveMITM(cfg, clientConn, host); err == nil {
			return
		} else {
			e.logf("debug", "%s 的中间人握手未成功，尝试透传: %v", host, err)
		}
	}

	e.serveTunnel(cfg, clientConn, host)
}

// isManagedHost 判断域名是否属于受管范围。
func (e *Engine) isManagedHost(host string) bool {
	if _, ok := e.hosts[host]; ok {
		return true
	}
	return isGitHubInfra(host)
}

// categoryOf 返回域名所属的请求类别。
func categoryOf(hosts map[string]config.GHHost, host string) string {
	if gh, ok := hosts[host]; ok {
		return gh.Category
	}
	return config.CatRaw
}

// serveMITM 以中间人方式处理解密后的 HTTPS 流量。
//
// 流程：用本地 CA 签发的证书与客户端完成 TLS 握手，
// 然后把它当作普通 HTTP 连接交给同一个 Handler 处理，
// 从而复用完整的镜像调度、失败重试与统计逻辑。
//
// 注意：此时 CONNECT 请求头已被消费完毕，连接上接下来的字节
// 就是客户端发起的 TLS ClientHello，因此直接对原始连接做 TLS 服务端即可。
func (e *Engine) serveMITM(cfg *config.Config, clientConn net.Conn, host string) error {
	ca := e.CurrentCA()
	if ca == nil {
		return errors.New("中间人能力不可用")
	}
	cert, err := ca.CertificateFor(host)
	if err != nil {
		return fmt.Errorf("签发 %s 的证书失败: %w", host, err)
	}

	if err := clientConn.SetDeadline(time.Now().Add(15 * time.Second)); err != nil {
		return err
	}

	tlsConn := tls.Server(clientConn, &tls.Config{
		Certificates: []tls.Certificate{*cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	})

	if err := tlsConn.Handshake(); err != nil {
		return fmt.Errorf("TLS 握手失败: %w", err)
	}
	// 握手完成后取消超时，交由后续读写自行控制。
	_ = clientConn.SetDeadline(time.Time{})

	// 已建立 TLS 的连接交给标准库继续解析 HTTP 请求。
	e.serveMITMConn(cfg, tlsConn, host)
	return nil
}

// serveMITMConn 在已解密的 TLS 连接上提供 HTTP 服务。
func (e *Engine) serveMITMConn(cfg *config.Config, conn net.Conn, host string) {
	client, server := net.Pipe()
	ln := &singleConnListener{conn: client}

	go func() {
		done := make(chan struct{}, 2)
		go func() {
			_, _ = io.Copy(server, conn)
			done <- struct{}{}
		}()
		go func() {
			_, _ = io.Copy(conn, server)
			done <- struct{}{}
		}()
		<-done
		_ = server.Close()
		_ = conn.Close()
		// 关闭监听器使 http.Server.Serve 退出，释放 goroutine 与信号量。
		_ = ln.Close()
	}()

	// 用包装 Handler 强制把 Host 改写为真实域名，
	// 避免中间人解密后 Host 头丢失导致目标判定失败。
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Host = host
		r.URL.Host = ""
		r.URL.Scheme = "https"
		e.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          nil,
	}
	_ = srv.Serve(ln)
}

// serveTunnel 以纯 TCP 透传方式处理 CONNECT 请求。
//
// 不解密内容，只做原始 TCP 转发。适用于：
//   - 用户未安装 CA 证书；
//   - 客户端使用证书固定（certificate pinning）无法中间人的场景。
//
// 关键限制：透传模式无法改写 TLS 内部的内容，因此只能直连 GitHub 官方，
// 无法经镜像站中转（镜像站的证书与 SNI 不匹配会导致握手失败）。
// 若 DNS 优选已生效，透传连接同样能命中优质节点。
func (e *Engine) serveTunnel(cfg *config.Config, clientConn net.Conn, host string) {
	dialer := &net.Dialer{
		Timeout:   time.Duration(cfg.Proxy.ConnectTimeoutMS) * time.Millisecond,
		KeepAlive: 30 * time.Second,
	}

	upstream, err := dialer.Dial("tcp", net.JoinHostPort(host, "443"))
	if err != nil {
		e.logf("warn", "透传到 %s 失败: %v", host, err)
		_ = clientConn.Close()
		return
	}

	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(upstream, clientConn)
		e.metrics.BytesIn.Add(n)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(clientConn, upstream)
		e.metrics.BytesOut.Add(n)
		done <- struct{}{}
	}()

	<-done
	_ = upstream.Close()
	_ = clientConn.Close()
	<-done
}

// writeConnectError 在隧道建立失败时返回可读的错误响应。
func writeConnectError(conn net.Conn, code int, msg string) {
	body := fmt.Sprintf(
		"HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		code, http.StatusText(code), len(msg), msg)
	_, _ = conn.Write([]byte(body))
	_ = conn.Close()
}
