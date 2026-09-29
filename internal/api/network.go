package api

import (
	"net/http"
	"strings"

	"github.com/ghpp/ghpp/internal/netutil"
)

// handleNetwork 提供「接入方式」区域切换 LAN/WAN 所需的网络信息与外网地址配置。
//
//	GET /api/network  返回本机所有局域网 IPv4 + 用户保存的外网地址
//	PUT /api/network  更新外网地址（body: {"host": "g.example.com:7710"}）
//
// 设计：飞牛 NAS 通常多网卡多 LAN 口，前端需要列出所有候选地址供用户选择；
// 外网地址由用户手动填写并持久化到 config.yaml 的 external.host 字段，
// 下次切到「外网模式」直接复用，不必重新输入。
func (s *Server) handleNetwork(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeOK(w, s.networkStatus())
	case http.MethodPut, http.MethodPost:
		var body struct {
			Host *string `json:"host"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}
		if body.Host == nil {
			writeError(w, http.StatusBadRequest, "缺少 host 字段")
			return
		}
		host := strings.TrimSpace(*body.Host)
		if err := s.app.SetExternalHost(host); err != nil {
			writeError(w, http.StatusInternalServerError, "保存外网地址失败：%v", err)
			return
		}
		writeOK(w, s.networkStatus())
	default:
		writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}

// networkStatus 汇总当前网络信息：本机 IPv4 列表 + 已保存的外网地址。
func (s *Server) networkStatus() map[string]any {
	cfg := s.app.CurrentConfig()
	return map[string]any{
		// 本机所有可用于局域网通信的 IPv4，已过滤回环 / 链路本地 / docker 网桥。
		"local_ips": netutil.LocalIPs(),
		// 用户保存的外网接入地址，空串表示未配置。
		"external_host": cfg.External.Host,
	}
}
