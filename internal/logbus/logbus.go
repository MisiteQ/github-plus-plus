// Package logbus 提供一个内存环形日志总线。
//
// 作用是把服务运行期的事件同时送到三个地方：
//  1. 标准输出，供飞牛应用中心的日志查看；
//  2. 磁盘文件，便于事后排查；
//  3. 内存环，供 Web 控制台实时拉取。
package logbus

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry 是一条日志记录。
type Entry struct {
	Seq     int64     `json:"seq"`
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// Bus 是日志总线。
type Bus struct {
	mu      sync.RWMutex
	entries []Entry
	seq     int64
	size    int

	// subs 是实时订阅者，每个订阅者持有一个带缓冲的通道。
	subs map[int]chan Entry
	next int

	file *os.File
	path string
}

// New 创建日志总线，size 是内存中保留的日志条数上限。
func New(size int, filePath string) *Bus {
	if size <= 0 {
		size = 1000
	}
	b := &Bus{
		size: size,
		subs: make(map[int]chan Entry),
		path: filePath,
	}
	if filePath != "" {
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err == nil {
			// 以追加模式打开，多次启动日志连续。
			b.file, _ = os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		}
	}
	return b
}

// Logf 写入一条日志，兼容 logf(level, format, args...) 的调用形式。
func (b *Bus) Logf(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	e := Entry{
		Time:    time.Now(),
		Level:   level,
		Message: msg,
	}

	b.mu.Lock()
	b.seq++
	e.Seq = b.seq
	b.entries = append(b.entries, e)
	if len(b.entries) > b.size {
		// 环形淘汰，保留最近的 size 条。
		b.entries = b.entries[len(b.entries)-b.size:]
	}
	subs := make([]chan Entry, 0, len(b.subs))
	for _, ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	// 输出到标准输出，飞牛的应用日志会采集这里的内容。
	fmt.Printf("[%s] %-5s %s\n", e.Time.Format("15:04:05"), level, msg)

	if b.file != nil {
		fmt.Fprintf(b.file, "%s [%s] %s\n", e.Time.Format("2006-01-02 15:04:05"), level, msg)
	}

	// 分发给实时订阅者，通道满时丢弃该条以免阻塞业务。
	for _, ch := range subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Recent 返回最近 n 条日志，n<=0 时返回全部内存日志。
func (b *Bus) Recent(n int, level string, afterSeq int64) []Entry {
	b.mu.RLock()
	defer b.mu.RUnlock()

	out := make([]Entry, 0, len(b.entries))
	for _, e := range b.entries {
		if e.Seq <= afterSeq {
			continue
		}
		if level != "" && e.Level != level {
			continue
		}
		out = append(out, e)
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}

// Subscribe 注册一个实时日志订阅者。
//
// 返回接收通道与注销函数，调用方必须在结束时调用注销函数释放资源。
func (b *Bus) Subscribe() (<-chan Entry, func()) {
	ch := make(chan Entry, 128)

	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}

// Close 关闭日志文件句柄。
func (b *Bus) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.file != nil {
		err := b.file.Close()
		b.file = nil
		return err
	}
	return nil
}

// Path 返回日志文件路径。
func (b *Bus) Path() string { return b.path }
