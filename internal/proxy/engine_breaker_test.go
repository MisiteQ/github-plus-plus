package proxy

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// newBreakerTestEngine 构造一个仅用于熔断状态机测试的最小 Engine。
func newBreakerTestEngine() *Engine {
	return &Engine{
		directCB: map[string]*directBreaker{},
		logf:     func(string, string, ...any) {},
	}
}

func TestReadProbe(t *testing.T) {
	// 数据充足：应在限时内读满 probeBytes。
	big := bytes.NewReader(make([]byte, probeBytes+100))
	b, err := readProbe(big, time.Second)
	if err != nil || len(b) != probeBytes {
		t.Fatalf("full read: len=%d err=%v, want %d nil", len(b), err, probeBytes)
	}

	// 小响应读到 EOF：返回实际字节且不视为失败。
	small := bytes.NewReader([]byte("hello"))
	b, err = readProbe(small, time.Second)
	if err != nil || string(b) != "hello" {
		t.Fatalf("small read: b=%q err=%v, want hello nil", b, err)
	}

	// 对端迟迟不给数据：应在超时后返回 errProbeTimeout（而不是无限等待）。
	pr, pw := io.Pipe()
	_, err = readProbe(pr, 30*time.Millisecond)
	if !errors.Is(err, errProbeTimeout) {
		t.Fatalf("stalled read: err=%v, want errProbeTimeout", err)
	}
	// 关闭以释放内部仍在阻塞的读取 goroutine。
	_ = pw.Close()
}

func TestDirectBreakerHalfOpen(t *testing.T) {
	const host = "github.com"
	e := newBreakerTestEngine()
	// 用例过程中触发熔断会安排 60s 后的探测定时器，结束时统一停止，防止泄漏。
	defer e.resetDirectBreakers()

	if !e.directAvailable(host) {
		t.Fatal("初始状态应当允许直连")
	}

	e.recordDirect(host, false) // fails=1：容忍一次抖动
	if !e.directAvailable(host) {
		t.Fatal("仅失败 1 次时仍应再给一次直连机会")
	}

	e.recordDirect(host, false) // fails=2：熔断
	if e.directAvailable(host) {
		t.Fatal("达到失败阈值后应进入熔断、不再内联直连")
	}

	// 后台探测通过 → 半开：只放行一次真实请求做验证。
	e.markDirectHalfOpen(host)
	if !e.directAvailable(host) {
		t.Fatal("半开状态应放行一次试探请求")
	}
	// 唯一令牌已被取走：与之并发的其它请求必须继续走镜像，不能再放直连。
	if e.directAvailable(host) {
		t.Fatal("半开状态只能放行一次，并发的第二个请求应走镜像")
	}

	// 半开后的这次真实请求仍失败：必须立即重新熔断，而不是再累计到两次。
	e.recordDirect(host, false)
	if e.directAvailable(host) {
		t.Fatal("半开试探失败后应立即重新熔断")
	}

	// 再次探测通过 → 半开 → 真实请求成功 → 完全恢复。
	e.markDirectHalfOpen(host)
	e.recordDirect(host, true)
	if !e.directAvailable(host) {
		t.Fatal("半开试探成功后应完全恢复直连")
	}

	// 完全恢复后的偶发单次失败不应立刻熔断（回到容忍一次抖动的策略）。
	e.recordDirect(host, false)
	if !e.directAvailable(host) {
		t.Fatal("恢复后的单次抖动不应立即熔断")
	}
	e.recordDirect(host, true)
	if !e.directAvailable(host) {
		t.Fatal("再次成功后应保持健康")
	}
}

func TestDirectProbePath(t *testing.T) {
	if p := directProbePath("github.com"); !strings.Contains(p, "info/refs") {
		t.Fatalf("github.com 应使用大响应端点探测吞吐，实际 %q", p)
	}
	if p := directProbePath("www.github.com"); !strings.Contains(p, "info/refs") {
		t.Fatalf("www.github.com 应使用大响应端点，实际 %q", p)
	}
	if p := directProbePath("api.github.com"); p != "/zen" {
		t.Fatalf("api.github.com 应使用省配额的 /zen，实际 %q", p)
	}
	if p := directProbePath("gist.github.com"); p != "/" {
		t.Fatalf("其余域名默认探测 /，实际 %q", p)
	}
}
