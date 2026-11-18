package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

const testProxyAddr = "http://127.0.0.1:7710"

func writeTempFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写入测试文件失败: %v", err)
	}
}

func readTestJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatalf("解析 %s 失败: %v", path, err)
	}
	return root
}

// TestApplyAndUnapplyDockermgrMirror 覆盖飞牛 fnOS 的核心适配逻辑：
// 注入镜像、切换当前源、保留未知字段、取消应用后完整恢复。
func TestApplyAndUnapplyDockermgrMirror(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "dockermgr.conf")
	statePath := filepath.Join(dir, "state.json")

	writeTempFile(t, confPath, `{
  "data-root": "/vol1/docker",
  "current-mirror": "默认加速源",
  "mirrors-v2": [
    {"name": "默认加速源", "url": "https://docker.fnnas.com"},
    {"name": "docker", "url": "https://registry.hub.docker.com"}
  ]
}`)

	if err := applyDockermgrMirror(confPath, statePath, testProxyAddr); err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	if !dockermgrContainsMirror(confPath, testProxyAddr) {
		t.Fatal("应用后未找到本机代理条目")
	}

	root := readTestJSON(t, confPath)
	if root["data-root"] != "/vol1/docker" {
		t.Fatalf("未知字段被破坏: data-root=%v", root["data-root"])
	}
	if root["current-mirror"] != dockermgrMirrorName {
		t.Fatalf("未切换 current-mirror: %v", root["current-mirror"])
	}

	// 幂等：重复应用不应产生重复条目。
	if err := applyDockermgrMirror(confPath, statePath, testProxyAddr); err != nil {
		t.Fatalf("重复应用失败: %v", err)
	}
	_, mirrors, ok := findMirrorList(readTestJSON(t, confPath))
	if !ok {
		t.Fatal("应用后镜像列表结构异常")
	}
	count := 0
	for _, item := range mirrors {
		m, _ := item.(map[string]any)
		if m != nil {
			if s, _ := m["url"].(string); s == testProxyAddr {
				count++
			}
		}
	}
	if count != 1 {
		t.Fatalf("重复应用产生了 %d 条本机代理条目", count)
	}

	// 取消应用：移除条目并恢复原 current-mirror。
	if err := unapplyDockermgrMirror(confPath, statePath, testProxyAddr); err != nil {
		t.Fatalf("取消应用失败: %v", err)
	}
	if dockermgrContainsMirror(confPath, testProxyAddr) {
		t.Fatal("取消应用后本机代理条目仍存在")
	}
	root = readTestJSON(t, confPath)
	if root["current-mirror"] != "默认加速源" {
		t.Fatalf("current-mirror 未恢复: %v", root["current-mirror"])
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatal("取消应用后状态文件应被清理")
	}
}

// TestDockermgrUnsupportedStructure 结构不符合预期时必须拒绝改写，
// 避免把 NAS 的 Docker 配置写坏。
func TestDockermgrUnsupportedStructure(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "dockermgr.conf")
	statePath := filepath.Join(dir, "state.json")
	original := `{"totally":"different"}`
	writeTempFile(t, confPath, original)

	err := applyDockermgrMirror(confPath, statePath, testProxyAddr)
	if !errors.Is(err, errDockermgrUnsupported) {
		t.Fatalf("期望 errDockermgrUnsupported，实际 %v", err)
	}
	raw, _ := os.ReadFile(confPath)
	if string(raw) != original {
		t.Fatal("无法识别结构时不应修改配置文件")
	}
}

// TestApplyAndUnapplyDaemonJSONMirror 覆盖标准 Docker 平台：
// 合并保留已有镜像源，取消应用只摘掉本机代理。
func TestApplyAndUnapplyDaemonJSONMirror(t *testing.T) {
	dir := t.TempDir()
	daemonPath := filepath.Join(dir, "daemon.json")
	writeTempFile(t, daemonPath, `{
  "data-root": "/vol1/docker",
  "registry-mirrors": ["https://docker.fnnas.com"]
}`)

	preview, err := applyDaemonJSONMirror(daemonPath, testProxyAddr)
	if err != nil {
		t.Fatalf("应用失败: %v", err)
	}
	if preview == "" {
		t.Fatal("应用后应返回 daemon.json 预览")
	}
	if !daemonJSONContainsMirror(daemonPath, testProxyAddr) {
		t.Fatal("应用后未找到本机代理地址")
	}
	// 已有镜像源必须保留，否则取消应用无法真正恢复。
	if !daemonJSONContainsMirror(daemonPath, "https://docker.fnnas.com") {
		t.Fatal("应用覆盖掉了用户已有的镜像源")
	}

	if err := unapplyDaemonJSONMirror(daemonPath, testProxyAddr); err != nil {
		t.Fatalf("取消应用失败: %v", err)
	}
	if daemonJSONContainsMirror(daemonPath, testProxyAddr) {
		t.Fatal("取消应用后本机代理地址仍存在")
	}
	root := readTestJSON(t, daemonPath)
	if root["data-root"] != "/vol1/docker" {
		t.Fatalf("未知字段被破坏: data-root=%v", root["data-root"])
	}
	mirrors := stringList(root["registry-mirrors"])
	if len(mirrors) != 1 || mirrors[0] != "https://docker.fnnas.com" {
		t.Fatalf("镜像源未正确恢复: %v", mirrors)
	}
}
