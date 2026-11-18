package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/ghpp/ghpp/internal/config"
)

// handleDocker 提供 Docker 镜像拉取加速的配置与接入指引。
//
//	GET  /api/docker           查看配置与生成的接入配置片段
//	PUT  /api/docker           更新 Docker 加速配置（enabled / upstreams）
//	POST /api/docker/apply     一键写入系统配置并重启 Docker（小白用户免 SSH）
//	POST /api/docker/unapply   取消应用，恢复写入前的 Docker 配置
func (s *Server) handleDocker(w http.ResponseWriter, r *http.Request) {
	// 注意先判断 /unapply：它同样以 "apply" 结尾，顺序反了会误入应用分支。
	if strings.HasSuffix(r.URL.Path, "/unapply") {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.handleDockerUnapply(w, r)
		return
	}
	// 子路径 /apply 走一键应用流程。
	if strings.HasSuffix(r.URL.Path, "/apply") {
		if r.Method != http.MethodPost && r.Method != http.MethodPut {
			writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
			return
		}
		s.handleDockerApply(w, r)
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeOK(w, s.dockerStatus())
	case http.MethodPut, http.MethodPost:
		var body struct {
			Enabled   *bool     `json:"enabled"`
			Upstreams *[]string `json:"upstreams"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}
		err := s.app.Config().Update(func(c *config.Config) error {
			if body.Enabled != nil {
				c.Docker.Enabled = *body.Enabled
			}
			if body.Upstreams != nil {
				cleaned := make([]string, 0, len(*body.Upstreams))
				for _, u := range *body.Upstreams {
					u = strings.TrimSpace(u)
					if u == "" {
						continue
					}
					cleaned = append(cleaned, u)
				}
				if len(cleaned) == 0 {
					return fmt.Errorf("上游列表不能为空")
				}
				c.Docker.Upstreams = cleaned
			}
			return nil
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}
		writeOK(w, s.dockerStatus())
	default:
		writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

const (
	// dockerDaemonJSONPath 是 Docker 守护进程配置文件路径。
	// 飞牛基于 Debian，Docker 走标准路径。
	dockerDaemonJSONPath = "/etc/docker/daemon.json"

	// 飞牛 fnOS 的 Docker 配置由 dockermgr 统一管理：
	// docker.service 的 ExecStartPre 会在每次 dockerd 启动前依据
	// dockermgr.conf 重写 daemon.json，因此直接写 daemon.json 会被覆盖。
	fnosDockermgrConfPath = "/usr/trim/etc/dockermgr.conf"
	fnosDockermgrBinPath  = "/usr/trim/bin/dockermgr"

	// dockermgrMirrorName 是我们写入 dockermgr.conf 的镜像条目名称。
	dockermgrMirrorName = "GitHub++"

	// dockermgrStateFile 记录应用前的原始状态，供取消应用时恢复。
	dockermgrStateFile = "docker-apply-state.json"
)

// dockerTargets 汇总本机上所有可能写入的 Docker 配置文件路径。
//
// 抽成结构体而非直接使用常量，便于单元测试用临时目录覆盖，
// 否则这些逻辑只能在真机上验证。
type dockerTargets struct {
	daemonPath   string
	fnosConfPath string
	fnosBinPath  string
	statePath    string
}

// defaultDockerTargets 返回生产环境使用的真实路径。
func defaultDockerTargets(dataDir string) dockerTargets {
	if dataDir == "" {
		dataDir = "."
	}
	return dockerTargets{
		daemonPath:   dockerDaemonJSONPath,
		fnosConfPath: fnosDockermgrConfPath,
		fnosBinPath:  fnosDockermgrBinPath,
		statePath:    filepath.Join(dataDir, dockermgrStateFile),
	}
}

// isFnOS 判断当前是否为飞牛 fnOS 且由 dockermgr 托管 Docker 配置。
func (t dockerTargets) isFnOS() bool {
	if _, err := os.Stat(t.fnosBinPath); err != nil {
		return false
	}
	_, err := os.Stat(t.fnosConfPath)
	return err == nil
}

// handleDockerApply 一键把本机代理地址写入系统 Docker 配置并重启 Docker。
//
// 面向小白用户：避免让用户 SSH 改文件或跑去飞牛 Docker 界面手动填镜像源。
//
// 写入目标按平台自动选择：
//   - 飞牛 fnOS：写入 dockermgr.conf 的 mirrors-v2 并切换 current-mirror，
//     由 dockermgr 自己生成 daemon.json，重启后不会被覆盖；
//   - 其他平台：直接合并写入 /etc/docker/daemon.json 的 registry-mirrors。
//
// 风险：重启 Docker 会让所有容器短暂中断，前端必须强确认。
func (s *Server) handleDockerApply(w http.ResponseWriter, r *http.Request) {
	cfg := s.app.CurrentConfig()
	proxyAddr := localProxyURL(cfg.Proxy.Listen)
	targets := defaultDockerTargets(cfg.DataDir)

	platform := "daemon.json"
	targetPath := targets.daemonPath
	var preview string
	fnosFallback := false

	if targets.isFnOS() {
		platform = "fnos-dockermgr"
		targetPath = targets.fnosConfPath
		if err := applyDockermgrMirror(targets.fnosConfPath, targets.statePath, proxyAddr); err != nil {
			// 结构无法识别时不硬写，回退到 daemon.json 并明确告知可能被飞牛覆盖，
			// 至少让标准 Docker 场景继续可用，也不会破坏用户原有配置。
			if !errors.Is(err, errDockermgrUnsupported) {
				writeError(w, http.StatusInternalServerError, "写入飞牛 dockermgr 配置失败：%v", err)
				return
			}
			platform = "daemon.json"
			targetPath = targets.daemonPath
			fnosFallback = true
		}
	}

	if platform == "daemon.json" {
		var err error
		preview, err = applyDaemonJSONMirror(targets.daemonPath, proxyAddr)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "%v", err)
			return
		}
	}

	// 重启 Docker 服务。飞牛上重启会触发 ExecStartPre，由 dockermgr 依据
	// 我们刚写的 dockermgr.conf 重新生成 daemon.json，这正是适配的关键一步。
	// 用 30 秒超时，避免卡死；重启失败不影响已写入的配置文件。
	restartErr := restartDockerService()
	msg := "已写入 Docker 配置并重启 Docker 服务"
	if restartErr != nil {
		msg = fmt.Sprintf("已写入 Docker 配置，但重启 Docker 失败：%v（下次 Docker 重启时自动加载）", restartErr)
	}

	if platform == "fnos-dockermgr" {
		msg = "已写入飞牛 dockermgr 配置（飞牛会在 Docker 启动时生成 daemon.json）并重启 Docker 服务"
		if restartErr != nil {
			msg = fmt.Sprintf("已写入飞牛 dockermgr 配置，但重启 Docker 失败：%v", restartErr)
		} else if !daemonJSONContainsMirror(targets.daemonPath, proxyAddr) {
			// 重启后由 dockermgr 生成 daemon.json，这里做一次结果校验：
			// 若加速地址没有出现，说明该版本 dockermgr 的结构与我们写入的不一致，
			// 直接告诉用户去看配置文件，比默默显示"已应用"有用。
			msg += "；但重启后 daemon.json 中未出现本机加速地址，可能是 dockermgr 版本差异，请检查 " + targets.fnosConfPath
		}
	} else if fnosFallback {
		msg += "；注意：未能识别飞牛 dockermgr 配置结构，已退回直接写 daemon.json，重启后可能被飞牛覆盖"
	}

	writeOK(w, map[string]any{
		"applied":     restartErr == nil,
		"proxy_url":   proxyAddr,
		"daemon_json": preview,
		"platform":    platform,
		"target":      targetPath,
		"message":     msg,
		"restart_ok":  restartErr == nil,
	})
}

// handleDockerUnapply 取消应用：移除本机加速地址并重启 Docker。
func (s *Server) handleDockerUnapply(w http.ResponseWriter, r *http.Request) {
	cfg := s.app.CurrentConfig()
	proxyAddr := localProxyURL(cfg.Proxy.Listen)
	targets := defaultDockerTargets(cfg.DataDir)

	platform := "daemon.json"
	targetPath := targets.daemonPath

	if targets.isFnOS() {
		platform = "fnos-dockermgr"
		targetPath = targets.fnosConfPath
		if err := unapplyDockermgrMirror(targets.fnosConfPath, targets.statePath, proxyAddr); err != nil {
			if !errors.Is(err, errDockermgrUnsupported) {
				writeError(w, http.StatusInternalServerError, "恢复飞牛 dockermgr 配置失败：%v", err)
				return
			}
			platform = "daemon.json"
			targetPath = targets.daemonPath
		}
	}

	if platform == "daemon.json" {
		if err := unapplyDaemonJSONMirror(targets.daemonPath, proxyAddr); err != nil {
			writeError(w, http.StatusInternalServerError, "%v", err)
			return
		}
	}

	restartErr := restartDockerService()
	msg := "已移除 Docker 加速配置并重启 Docker 服务"
	if restartErr != nil {
		msg = fmt.Sprintf("已移除 Docker 加速配置，但重启 Docker 失败：%v", restartErr)
	}

	writeOK(w, map[string]any{
		"applied":    false,
		"platform":   platform,
		"target":     targetPath,
		"message":    msg,
		"restart_ok": restartErr == nil,
	})
}

// errDockermgrUnsupported 表示 dockermgr.conf 结构与预期不符，无法安全改写。
// 调用方据此回退到直接写 daemon.json 的通用路径。
var errDockermgrUnsupported = errors.New("dockermgr.conf 结构无法识别")

// dockermgrState 记录应用前 dockermgr.conf 中 current-mirror 的原始取值。
type dockermgrState struct {
	// HadCurrentMirror 表示应用前是否存在 current-mirror 键。
	HadCurrentMirror bool `json:"had_current_mirror"`
	// PreviousCurrentMirror 是应用前的原值。
	PreviousCurrentMirror string `json:"previous_current_mirror"`
}

// applyDockermgrMirror 把本机代理注入 dockermgr.conf 的镜像列表。
//
// 只在能识别出数组结构时改写；其余情况返回 errDockermgrUnsupported，
// 绝不猜测格式硬写系统配置（写坏会导致 NAS 上所有容器网络异常）。
func applyDockermgrMirror(confPath, statePath, proxyAddr string) error {
	root, err := readJSONObject(confPath)
	if err != nil {
		return err
	}

	key, mirrors, ok := findMirrorList(root)
	if !ok {
		return errDockermgrUnsupported
	}

	// 已存在同地址条目就更新名称，避免重复注入。
	replaced := false
	for _, item := range mirrors {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if s, _ := m["url"].(string); s == proxyAddr {
			m["name"] = dockermgrMirrorName
			replaced = true
		}
	}
	if !replaced {
		mirrors = append(mirrors, map[string]any{
			"name": dockermgrMirrorName,
			"url":  proxyAddr,
		})
	}
	root[key] = mirrors

	// 首次应用时保存 current-mirror 原值，取消应用时才能恢复。
	if _, err := os.Stat(statePath); err != nil {
		prev, had := root["current-mirror"].(string)
		state := dockermgrState{HadCurrentMirror: had, PreviousCurrentMirror: prev}
		raw, _ := json.MarshalIndent(state, "", "  ")
		if werr := writeFileAtomic(statePath, append(raw, '\n'), 0o644); werr != nil {
			return fmt.Errorf("记录 dockermgr 原始状态失败：%w", werr)
		}
	}

	// 切换当前生效的镜像。仅当原值为字符串或键不存在时才改写，
	// 其他类型说明结构不符合预期，保持不动以免破坏飞牛配置。
	if cur, exists := root["current-mirror"]; !exists {
		root["current-mirror"] = dockermgrMirrorName
	} else if _, isStr := cur.(string); isStr {
		root["current-mirror"] = dockermgrMirrorName
	}

	_, err = writeJSONObjectAtomic(confPath, root)
	return err
}

// unapplyDockermgrMirror 移除注入的镜像条目并恢复 current-mirror。
func unapplyDockermgrMirror(confPath, statePath, proxyAddr string) error {
	root, err := readJSONObject(confPath)
	if err != nil {
		return err
	}

	key, mirrors, ok := findMirrorList(root)
	if !ok {
		return errDockermgrUnsupported
	}

	kept := make([]any, 0, len(mirrors))
	for _, item := range mirrors {
		if m, ok := item.(map[string]any); ok {
			if s, _ := m["url"].(string); s == proxyAddr {
				continue
			}
		}
		kept = append(kept, item)
	}
	root[key] = kept

	// 恢复应用前的 current-mirror。
	if raw, err := os.ReadFile(statePath); err == nil {
		var state dockermgrState
		if json.Unmarshal(raw, &state) == nil {
			if state.HadCurrentMirror {
				root["current-mirror"] = state.PreviousCurrentMirror
			} else {
				delete(root, "current-mirror")
			}
		}
		_ = os.Remove(statePath)
	}

	_, err = writeJSONObjectAtomic(confPath, root)
	return err
}

// findMirrorList 在 dockermgr.conf 中定位镜像数组。
//
// 飞牛使用 "mirrors-v2"；不同版本可能退化为 "mirrors"，两者都尝试，
// 找不到就视为结构不可识别。
func findMirrorList(root map[string]any) (string, []any, bool) {
	for _, key := range []string{"mirrors-v2", "mirrors"} {
		raw, ok := root[key]
		if !ok {
			continue
		}
		list, ok := raw.([]any)
		if !ok {
			return "", nil, false
		}
		return key, list, true
	}
	return "", nil, false
}

// applyDaemonJSONMirror 把本机代理合并进 daemon.json 的 registry-mirrors。
//
// 合并而非覆盖：保留用户已有的镜像源，取消应用时也只需要摘掉我们这一条。
// 返回写入后的完整 JSON 文本，供前端展示。
func applyDaemonJSONMirror(daemonPath, proxyAddr string) (string, error) {
	root, err := readJSONObject(daemonPath)
	if err != nil {
		return "", err
	}

	mirrors := make([]any, 0, 8)
	mirrors = append(mirrors, proxyAddr)
	for _, item := range stringList(root["registry-mirrors"]) {
		if item == proxyAddr {
			continue
		}
		mirrors = append(mirrors, item)
	}
	root["registry-mirrors"] = mirrors

	raw, err := writeJSONObjectAtomic(daemonPath, root)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// unapplyDaemonJSONMirror 从 daemon.json 中移除本机代理地址。
func unapplyDaemonJSONMirror(daemonPath, proxyAddr string) error {
	if _, err := os.Stat(daemonPath); os.IsNotExist(err) {
		// 文件本来就不存在，无需创建空文件，直接视为已恢复。
		return nil
	}

	root, err := readJSONObject(daemonPath)
	if err != nil {
		return err
	}

	kept := make([]any, 0, len(root))
	for _, item := range stringList(root["registry-mirrors"]) {
		if item == proxyAddr {
			continue
		}
		kept = append(kept, item)
	}
	if len(kept) == 0 {
		delete(root, "registry-mirrors")
	} else {
		root["registry-mirrors"] = kept
	}

	_, err = writeJSONObjectAtomic(daemonPath, root)
	return err
}

// readJSONObject 读取并解析 JSON 对象；文件不存在视为空对象。
func readJSONObject(path string) (map[string]any, error) {
	root := map[string]any{}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return root, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败：%w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return root, nil
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		return nil, fmt.Errorf("解析 %s 失败：%v（请手动检查该文件是否为合法 JSON）", path, err)
	}
	return root, nil
}

// writeJSONObjectAtomic 原子写回 JSON 文件，并返回写入的字节。
func writeJSONObjectAtomic(path string, root map[string]any) ([]byte, error) {
	raw, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("生成 %s 内容失败：%w", path, err)
	}
	raw = append(raw, '\n')
	if err := writeFileAtomic(path, raw, 0o644); err != nil {
		return nil, fmt.Errorf("写入 %s 失败：%v（请确认加速器以 root 运行且目录可写）", path, err)
	}
	return raw, nil
}

// writeFileAtomic 先写临时文件再 rename，避免 dockerd 读到半写入的配置。
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmpPath := path + ".ghpp-tmp"
	if err := os.WriteFile(tmpPath, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// stringList 把 JSON 数组宽松转换为字符串切片，忽略非法元素。
func stringList(v any) []string {
	list, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return ss
		}
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// restartDockerService 重启 Docker 服务，优先 systemctl，回退 service 命令。
//
// 返回错误表示重启失败（配置文件已写入，下次 Docker 重启仍会加载）。
// 30 秒超时：Docker 正常重启 5-15 秒，超时说明服务卡住，不能一直挂着请求。
func restartDockerService() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// systemctl 在 systemd 系统上可用；飞牛基于 Debian，默认有 systemd。
	if _, err := exec.LookPath("systemctl"); err == nil {
		cmd := exec.CommandContext(ctx, "systemctl", "restart", "docker")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("systemctl restart docker 执行失败：%w", err)
		}
		return nil
	}
	// 回退：SysVinit 的 service 命令。
	if _, err := exec.LookPath("service"); err == nil {
		cmd := exec.CommandContext(ctx, "service", "docker", "restart")
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("service docker restart 执行失败：%w", err)
		}
		return nil
	}
	return fmt.Errorf("未找到 systemctl 或 service 命令，无法自动重启 Docker")
}

// dockerStatus 汇总 Docker 加速的当前状态与各场景的接入配置片段。
//
// Docker 加速地址固定使用 127.0.0.1：<代理端口>：
// Docker daemon 跑在飞牛 NAS 本机，通过 loopback 访问本机代理最稳定，
// 不受用户从内网还是外网访问控制台影响，也避免把外网地址配进 daemon.json
// 导致 NAS 本机 docker pull 反而绕外网一圈回来。
//
// applied 字段反映系统配置里是否已包含本代理地址，
// 前端据此显示"已应用 / 未应用"徽章，引导小白用户一键应用。
func (s *Server) dockerStatus() map[string]any {
	cfg := s.app.CurrentConfig()
	proxyAddr := localProxyURL(cfg.Proxy.Listen)
	targets := defaultDockerTargets(cfg.DataDir)

	// daemon.json 参考片段：高级用户可参考，小白用户直接点"一键应用"按钮即可。
	daemonJSON := "{\n" +
		"  \"registry-mirrors\": [\"" + proxyAddr + "\"]\n" +
		"}"

	platform := "daemon.json"
	targetPath := targets.daemonPath
	applied := daemonJSONContainsMirror(targets.daemonPath, proxyAddr)
	if targets.isFnOS() {
		platform = "fnos-dockermgr"
		targetPath = targets.fnosConfPath
		// 飞牛上以 dockermgr.conf 为准；dockermgr 尚未把配置落到 daemon.json 时
		// 也应当显示已应用，否则用户会以为点击无效。
		applied = dockermgrContainsMirror(targets.fnosConfPath, proxyAddr) ||
			daemonJSONContainsMirror(targets.daemonPath, proxyAddr)
	}

	note := "Docker daemon 跑在 NAS 本机，固定用 127.0.0.1 + 代理端口；" +
		"点「一键应用到系统」按钮自动写入 Docker 配置并重启 Docker，无需 SSH。"
	if platform == "fnos-dockermgr" {
		note = "已检测到飞牛 dockermgr：加速配置会写入 /usr/trim/etc/dockermgr.conf，" +
			"由飞牛在 Docker 启动时生成 daemon.json，重启后不会被覆盖。"
	}

	return map[string]any{
		"enabled":         cfg.Docker.Enabled,
		"upstreams":       cfg.Docker.Upstreams,
		"proxy_url":       proxyAddr,
		"daemon_json":     daemonJSON,
		"registry_mirror": proxyAddr,
		"applied":         applied,
		"platform":        platform,
		"apply_target":    targetPath,
		"supported_registries": []map[string]string{
			{"registry": "docker.io", "note": "Docker Hub，registry-mirrors 直接支持"},
			{"registry": "ghcr.io", "note": "通过本机代理 + 信任 CA 后加速，或使用免费镜像 ghcr.m.daocloud.io"},
			{"registry": "gcr.io", "note": "通过本机代理 + 信任 CA 后加速，或使用免费镜像 gcr.m.daocloud.io"},
			{"registry": "quay.io", "note": "通过本机代理 + 信任 CA 后加速，或使用免费镜像 quay.m.daocloud.io"},
			{"registry": "registry.k8s.io", "note": "通过本机代理 + 信任 CA 后加速，或使用免费镜像 k8s.m.daocloud.io"},
		},
		"note": note,
	}
}

// daemonJSONContainsMirror 检查 daemon.json 的 registry-mirrors
// 是否已包含指定的代理地址。文件不存在或解析失败返回 false。
func daemonJSONContainsMirror(daemonPath, proxyAddr string) bool {
	root, err := readJSONObject(daemonPath)
	if err != nil {
		return false
	}
	for _, m := range stringList(root["registry-mirrors"]) {
		if m == proxyAddr {
			return true
		}
	}
	return false
}

// dockermgrContainsMirror 检查 dockermgr.conf 的镜像列表是否已包含本机代理。
func dockermgrContainsMirror(confPath, proxyAddr string) bool {
	root, err := readJSONObject(confPath)
	if err != nil {
		return false
	}
	_, mirrors, ok := findMirrorList(root)
	if !ok {
		return false
	}
	for _, item := range mirrors {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if s, _ := m["url"].(string); s == proxyAddr {
			return true
		}
	}
	return false
}

// localProxyURL 返回 NAS 本机访问代理的地址：http://127.0.0.1:<代理端口>。
//
// 端口取自代理服务的实际监听地址；监听地址异常时回退 7710。
func localProxyURL(proxyListen string) string {
	_, port, err := net.SplitHostPort(proxyListen)
	if err != nil || port == "" {
		port = "7710"
	}
	return "http://" + net.JoinHostPort("127.0.0.1", port)
}
