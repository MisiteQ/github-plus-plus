package api

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/ghpp/ghpp/internal/config"
)

// handleDocker 提供 Docker 镜像拉取加速的配置与接入指引。
//
//	GET /api/docker  查看配置与生成的接入配置片段
//	PUT /api/docker  更新 Docker 加速配置（enabled / upstreams）
func (s *Server) handleDocker(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeOK(w, s.dockerStatus(r))
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
		writeOK(w, s.dockerStatus(r))
	default:
		writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

// dockerStatus 汇总 Docker 加速的当前状态与各场景的接入配置片段。
func (s *Server) dockerStatus(r *http.Request) map[string]any {
	cfg := s.app.CurrentConfig()
	proxyAddr := selfProxyURL(r, cfg.Proxy.Listen)

	// daemon.json 片段：适用于 SSH 直接改 /etc/docker/daemon.json 的场景。
	daemonJSON := "{\n" +
		"  \"registry-mirrors\": [\"" + proxyAddr + "\"]\n" +
		"}"

	// 飞牛 Docker 界面：直接粘贴本地址作为镜像源。
	return map[string]any{
		"enabled":            cfg.Docker.Enabled,
		"upstreams":          cfg.Docker.Upstreams,
		"proxy_url":          proxyAddr,
		"daemon_json":        daemonJSON,
		"registry_mirror":    proxyAddr,
		"supported_registries": []map[string]string{
			{"registry": "docker.io", "note": "Docker Hub，registry-mirrors 直接支持"},
			{"registry": "ghcr.io", "note": "通过本机代理 + 信任 CA 后加速，或使用免费镜像 ghcr.m.daocloud.io"},
			{"registry": "gcr.io", "note": "通过本机代理 + 信任 CA 后加速，或使用免费镜像 gcr.m.daocloud.io"},
			{"registry": "quay.io", "note": "通过本机代理 + 信任 CA 后加速，或使用免费镜像 quay.m.daocloud.io"},
			{"registry": "registry.k8s.io", "note": "通过本机代理 + 信任 CA 后加速，或使用免费镜像 k8s.m.daocloud.io"},
		},
		"note": "把 registry-mirrors 配置为本机代理地址后，docker pull 会自动经由加速器，" +
			"上游按顺序尝试，失效自动切换。",
	}
}

// selfProxyURL 推断客户端访问本加速器应使用的代理地址。
//
// 优先用请求的 Host（用户从局域网访问控制台时，拿到的就是局域网可达地址），
// 端口部分取代理服务的实际监听端口。
func selfProxyURL(r *http.Request, proxyListen string) string {
	_, port, err := net.SplitHostPort(proxyListen)
	if err != nil {
		port = "7710"
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1"
	}
	// 去掉控制台端口，换成代理端口。
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return "http://" + net.JoinHostPort(host, port)
}
