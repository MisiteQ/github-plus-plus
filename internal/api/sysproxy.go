package api

import (
	"net/http"
)

// handleSysProxy 管理飞牛 NAS 系统级 HTTP 代理开关。
//
//	GET /api/sysproxy  查询当前是否已开启系统代理（/etc/profile.d/ghpp-proxy.sh 是否存在）
//	PUT /api/sysproxy  开启或关闭系统代理（body: {"enabled": true|false}）
//
// 开启后所有新 login shell 启动的软件默认走加速器，无需软件自身配置代理。
// 已运行的进程不受影响，需重启对应软件才生效——前端会给出明确提示。
func (s *Server) handleSysProxy(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeOK(w, map[string]any{
			// 实际状态以文件是否存在为准，避免配置与文件不一致时误报。
			"enabled": s.app.Status().SystemProxyEnabled,
		})
	case http.MethodPut, http.MethodPost:
		var body struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeError(w, http.StatusBadRequest, "%v", err)
			return
		}
		if body.Enabled == nil {
			writeError(w, http.StatusBadRequest, "缺少 enabled 字段")
			return
		}
		if err := s.app.SetSystemProxy(*body.Enabled); err != nil {
			writeError(w, http.StatusInternalServerError, "%v", err)
			return
		}
		writeOK(w, map[string]any{
			"enabled": s.app.Status().SystemProxyEnabled,
		})
	default:
		writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
	}
}
