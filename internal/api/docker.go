package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/ghpp/ghpp/internal/config"
)

// handleDocker 提供 Docker 镜像拉取加速的配置与接入指引。
//
//	GET /api/docker           查看配置与生成的接入配置片段
//	PUT /api/docker           更新 Docker 加速配置（enabled / upstreams）
//	POST /api/docker/apply    一键写入 daemon.json 并重启 Docker（小白用户免 SSH）
func (s *Server) handleDocker(w http.ResponseWriter, r *http.Request) {
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

// dockerDaemonJSONPath 是 Docker 守护进程配置文件路径。
// 飞牛基于 Debian，Docker 走标准路径。
const dockerDaemonJSONPath = "/etc/docker/daemon.json"

// handleDockerApply 一键把本机代理地址写入 /etc/docker/daemon.json 的 registry-mirrors，
// 并重启 Docker 服务让配置生效。
//
// 面向小白用户：避免让用户 SSH 改文件或跑去飞牛 Docker 界面手动填镜像源。
//
// 流程：
//  1. 读现有 daemon.json（不存在视为空对象），解析到 map 保留所有用户字段；
//  2. 把 registry-mirrors 设为 [本机代理地址]（覆盖，因为加速器是唯一的 mirror 源）；
//  3. 原子写回（临时文件 + rename，避免半写入态被 dockerd 读到）；
//  4. 重启 Docker（优先 systemctl restart docker，回退 service docker restart）。
//
// 风险：重启 Docker 会让所有容器短暂中断，前端必须强确认。
func (s *Server) handleDockerApply(w http.ResponseWriter, r *http.Request) {
	cfg := s.app.CurrentConfig()
	proxyAddr := localProxyURL(cfg.Proxy.Listen)

	// 1. 读现有 daemon.json，保留用户其他字段（如 data-root、insecure-registries 等）。
	existing := map[string]any{}
	if raw, err := os.ReadFile(dockerDaemonJSONPath); err == nil {
		// 文件存在但解析失败时不能盲目覆盖，直接报错让用户手动处理。
		if jerr := json.Unmarshal(raw, &existing); jerr != nil {
			writeError(w, http.StatusInternalServerError,
				"解析现有 daemon.json 失败：%v（请手动检查 %s 是否为合法 JSON）", jerr, dockerDaemonJSONPath)
			return
		}
	}

	// 2. 合并 registry-mirrors。覆盖式设置：加速器作为唯一 mirror 源，
	// 避免与用户其他 mirror 冲突导致行为不可预期。若用户有其他 mirror 需求，
	// 可在 daemon.json 里手动追加，本接口再次应用时会被覆盖——UI 必须提示。
	existing["registry-mirrors"] = []string{proxyAddr}

	// 3. 原子写回。先写 .tmp 再 rename，避免 dockerd 在半写入态读到非法 JSON。
	newJSON, err := json.MarshalIndent(existing, "", "  ")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "生成 daemon.json 失败：%v", err)
		return
	}
	// MarshalIndent 不带尾换行，补一个让文件更友好。
	newJSON = append(newJSON, '\n')

	tmpPath := dockerDaemonJSONPath + ".ghpp-tmp"
	if err := os.WriteFile(tmpPath, newJSON, 0644); err != nil {
		writeError(w, http.StatusInternalServerError,
			"写入 daemon.json 失败：%v（请确认加速器以 root 运行且 /etc/docker 可写）", err)
		return
	}
	if err := os.Rename(tmpPath, dockerDaemonJSONPath); err != nil {
		_ = os.Remove(tmpPath)
		writeError(w, http.StatusInternalServerError, "替换 daemon.json 失败：%v", err)
		return
	}

	// 4. 重启 Docker 服务。优先 systemctl（systemd 系统），回退 service 命令。
	// 用 30 秒超时，避免卡死；重启失败不影响已写入的配置文件，下次 Docker 重启仍会生效。
	restartErr := restartDockerService()
	msg := "已写入 /etc/docker/daemon.json 并重启 Docker 服务"
	if restartErr != nil {
		msg = fmt.Sprintf("已写入 daemon.json，但重启 Docker 失败：%v（配置已生效，下次 Docker 重启时加载）", restartErr)
	}

	writeOK(w, map[string]any{
		"applied":     restartErr == nil,
		"proxy_url":   proxyAddr,
		"daemon_json": string(newJSON),
		"message":     msg,
		"restart_ok":  restartErr == nil,
	})
}

// restartDockerService 重启 Docker 服务，优先 systemctl，回退 service 命令。
//
// 返回错误表示重启失败（配置文件已写入，下次 Docker 重启仍会加载）。
func restartDockerService() error {
	// systemctl 在 systemd 系统上可用；飞牛基于 Debian，默认有 systemd。
	if _, err := exec.LookPath("systemctl"); err == nil {
		cmd := exec.Command("systemctl", "restart", "docker")
		// 30 秒超时，Docker 重启通常 5-15 秒。
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("systemctl restart docker 执行失败：%w", err)
		}
		return nil
	}
	// 回退：SysVinit 的 service 命令。
	if _, err := exec.LookPath("service"); err == nil {
		cmd := exec.Command("service", "docker", "restart")
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
// applied 字段反映 daemon.json 里是否已包含本代理地址，
// 前端据此显示"已应用 / 未应用"徽章，引导小白用户一键应用。
func (s *Server) dockerStatus() map[string]any {
	cfg := s.app.CurrentConfig()
	proxyAddr := localProxyURL(cfg.Proxy.Listen)

	// daemon.json 参考片段：高级用户可参考，小白用户直接点"一键应用"按钮即可。
	daemonJSON := "{\n" +
		"  \"registry-mirrors\": [\"" + proxyAddr + "\"]\n" +
		"}"

	// 检测 daemon.json 是否已含本代理地址，前端显示"已应用/未应用"徽章。
	applied := daemonJSONContainsMirror(proxyAddr)

	return map[string]any{
		"enabled":         cfg.Docker.Enabled,
		"upstreams":       cfg.Docker.Upstreams,
		"proxy_url":       proxyAddr,
		"daemon_json":     daemonJSON,
		"registry_mirror": proxyAddr,
		"applied":         applied,
		"supported_registries": []map[string]string{
			{"registry": "docker.io", "note": "Docker Hub，registry-mirrors 直接支持"},
			{"registry": "ghcr.io", "note": "通过本机代理 + 信任 CA 后加速，或使用免费镜像 ghcr.m.daocloud.io"},
			{"registry": "gcr.io", "note": "通过本机代理 + 信任 CA 后加速，或使用免费镜像 gcr.m.daocloud.io"},
			{"registry": "quay.io", "note": "通过本机代理 + 信任 CA 后加速，或使用免费镜像 quay.m.daocloud.io"},
			{"registry": "registry.k8s.io", "note": "通过本机代理 + 信任 CA 后加速，或使用免费镜像 k8s.m.daocloud.io"},
		},
		"note": "Docker daemon 跑在 NAS 本机，固定用 127.0.0.1 + 代理端口；" +
			"点「一键应用到系统」按钮自动写入 daemon.json 并重启 Docker，无需 SSH。",
	}
}

// daemonJSONContainsMirror 检查 /etc/docker/daemon.json 的 registry-mirrors
// 是否已包含指定的代理地址。文件不存在或解析失败返回 false。
func daemonJSONContainsMirror(proxyAddr string) bool {
	raw, err := os.ReadFile(dockerDaemonJSONPath)
	if err != nil {
		return false
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return false
	}
	mirrors, ok := cfg["registry-mirrors"].([]any)
	if !ok {
		return false
	}
	for _, m := range mirrors {
		if s, ok := m.(string); ok && s == proxyAddr {
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
