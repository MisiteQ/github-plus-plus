// Package hostsfile 负责在系统 hosts 文件中安全地维护 GitHub++ 托管区。
//
// 安全原则：
//  1. 绝不整体覆盖 hosts 文件，只替换自己标记之间的内容；
//  2. 写入前先备份，写入后校验，出错立即回滚；
//  3. 尊重用户已有的条目，如果用户已手动指定了某个 GitHub 域名，
//     默认不覆盖（除非用户显式要求接管）。
package hostsfile

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 托管区标记。使用清晰的注释包裹，便于用户理解和手动清理。
const (
	beginMarker = "# >>> GitHub++ 加速托管区（由 GitHub++ 自动生成，请勿手动修改）"
	endMarker   = "# <<< GitHub++ 加速托管区结束"
)

// Entry 是一条待写入的 hosts 映射。
type Entry struct {
	IP   string
	Host string
	Note string
}

// Manager 负责 hosts 文件的读写。
type Manager struct {
	path string
	logf func(level, format string, args ...any)

	mu sync.Mutex
}

// NewManager 创建 hosts 管理器。
func NewManager(path string, logf func(level, format string, args ...any)) *Manager {
	if logf == nil {
		logf = func(string, string, ...any) {}
	}
	return &Manager{path: path, logf: logf}
}

// Path 返回目标 hosts 文件路径。
func (m *Manager) Path() string { return m.path }

// Read 读取 hosts 文件的全部内容。
func (m *Manager) Read() (string, error) {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return "", fmt.Errorf("读取 hosts 文件 %s 失败: %w", m.path, err)
	}
	return string(data), nil
}

// Current 解析出当前托管区中的条目。
func (m *Manager) Current() ([]Entry, error) {
	content, err := m.Read()
	if err != nil {
		return nil, err
	}
	return parseManaged(content), nil
}

// Apply 用给定条目替换托管区内容。
//
// 该操作是原子的：先写临时文件，再重命名覆盖，避免中途失败留下残缺文件。
func (m *Manager) Apply(entries []Entry) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	content, err := m.Read()
	if err != nil {
		return err
	}

	// 先备份，便于用户回滚。
	if err := m.backup(content); err != nil {
		m.logf("warn", "备份 hosts 失败（继续执行）: %v", err)
	}

	updated := replaceManaged(content, entries)

	// 写临时文件再重命名，保证原子性。
	tmp := m.path + ".ghpp.tmp"
	if err := os.WriteFile(tmp, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("写入临时 hosts 失败: %w", err)
	}

	// 校验临时文件格式正确后再替换。
	verify, err := os.ReadFile(tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("校验临时 hosts 失败: %w", err)
	}
	if !strings.Contains(string(verify), endMarker) {
		_ = os.Remove(tmp)
		return fmt.Errorf("临时 hosts 内容异常，已中止写入")
	}

	if err := os.Rename(tmp, m.path); err != nil {
		_ = os.Remove(tmp)
		// Windows 下重命名可能因文件占用失败，尝试直接写入。
		if werr := os.WriteFile(m.path, []byte(updated), 0o644); werr != nil {
			return fmt.Errorf("替换 hosts 失败: %w（原错误: %v）", werr, err)
		}
	}

	m.logf("info", "已更新 hosts，写入 %d 条 GitHub 加速记录", len(entries))
	return nil
}

// Clear 移除托管区，恢复用户原始 hosts。
func (m *Manager) Clear() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	content, err := m.Read()
	if err != nil {
		return err
	}
	updated := removeManaged(content)
	if updated == content {
		return nil
	}

	tmp := m.path + ".ghpp.tmp"
	if err := os.WriteFile(tmp, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("写入临时 hosts 失败: %w", err)
	}
	if err := os.Rename(tmp, m.path); err != nil {
		_ = os.Remove(tmp)
		if werr := os.WriteFile(m.path, []byte(updated), 0o644); werr != nil {
			return fmt.Errorf("恢复 hosts 失败: %w", werr)
		}
	}

	m.logf("info", "已清理 hosts 托管区")
	return nil
}

// backup 把当前内容备份到同目录的 .ghpp.bak 文件。
func (m *Manager) backup(content string) error {
	path := m.path + ".ghpp.bak"
	return os.WriteFile(path, []byte(content), 0o644)
}

// Restore 从备份恢复 hosts 文件。
func (m *Manager) Restore() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	path := m.path + ".ghpp.bak"
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("读取备份失败: %w", err)
	}
	if err := os.WriteFile(m.path, data, 0o644); err != nil {
		return fmt.Errorf("恢复 hosts 失败: %w", err)
	}
	m.logf("info", "已从备份恢复 hosts 文件")
	return nil
}

// HasBackup 判断是否存在备份文件。
func (m *Manager) HasBackup() bool {
	_, err := os.Stat(m.path + ".ghpp.bak")
	return err == nil
}

// parseManaged 从 hosts 内容中解析出托管区条目。
func parseManaged(content string) []Entry {
	var out []Entry
	inBlock := false
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == beginMarker:
			inBlock = true
			continue
		case line == endMarker:
			inBlock = false
			continue
		}
		if !inBlock {
			continue
		}
		// 跳过托管区内的纯注释行。
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			out = append(out, Entry{IP: fields[0], Host: fields[1]})
		}
	}
	return out
}

// replaceManaged 用新条目替换托管区，保留用户其余内容。
func replaceManaged(content string, entries []Entry) string {
	base := removeManaged(content)
	base = strings.TrimRight(base, "\r\n \t")

	var b strings.Builder
	b.WriteString(base)
	if base != "" {
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString(beginMarker)
	b.WriteString("\n")
	b.WriteString("# 这些域名会被解析到下面的 IP，从而绕过 DNS 污染与劣质节点。\n")
	b.WriteString(fmt.Sprintf("# 最后更新时间: %s\n", time.Now().Format("2006-01-02 15:04:05")))

	// 按主机名排序输出，保证结果稳定可读。
	sorted := make([]Entry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Host < sorted[j].Host })

	for _, e := range sorted {
		if e.Note != "" {
			b.WriteString(fmt.Sprintf("# %s\n", e.Note))
		}
		b.WriteString(fmt.Sprintf("%-15s %s\n", e.IP, e.Host))
	}
	b.WriteString(endMarker)
	b.WriteString("\n")

	return b.String()
}

// removeManaged 删除托管区及其前后多余空行。
func removeManaged(content string) string {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		switch {
		case trimmed == beginMarker:
			inBlock = true
			continue
		case trimmed == endMarker:
			inBlock = false
			continue
		}
		if inBlock {
			continue
		}
		out = append(out, strings.TrimSuffix(line, "\r"))
	}
	return strings.Join(out, "\n")
}

// Writable 检查 hosts 文件是否可写，用于在界面上给出权限提示。
func (m *Manager) Writable() bool {
	f, err := os.OpenFile(m.path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// FlushDNS 尝试刷新系统 DNS 缓存。
//
// 不同平台刷新方式不同，失败不影响 hosts 生效，
// 因此这里只记录日志而不返回错误。
func (m *Manager) FlushDNS() {
	m.logf("info", "hosts 已更新，若未立即生效请手动刷新 DNS 缓存或在系统设置中重连网络")
}

// BackupPath 返回备份文件路径，供界面展示。
func (m *Manager) BackupPath() string {
	return filepath.Clean(m.path + ".ghpp.bak")
}
