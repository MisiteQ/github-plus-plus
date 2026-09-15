package api

import (
	"net/http"
	"runtime"
)

// 关于本软件的静态信息。作者与仓库信息在此维护。
const (
	aboutName     = "GitHub++ 飞牛 NAS 加速器"
	aboutAuthor   = "MisiteQ"
	aboutGitHub   = "https://github.com/MisiteQ"
	aboutRepo     = "https://github.com/MisiteQ/github-plus-plus"
	aboutLanguage = "Go"
	aboutLicense  = "MIT"
	aboutDesc     = "为飞牛 fnOS 打造的 GitHub / Docker 加速器：智能测速选择最优镜像源，" +
		"支持反代加速、DNS 优选 hosts 加速、HTTPS 中间人加速与 Docker 镜像拉取加速，" +
		"提供 Web 控制台，Go 单二进制开箱即用。"
)

// handleAbout 返回"关于"信息：作者、技术栈、仓库地址与致谢。
func (s *Server) handleAbout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "不支持的方法")
		return
	}

	writeOK(w, map[string]any{
		"name":        aboutName,
		"version":     Version,
		"author":      aboutAuthor,
		"github":      aboutGitHub,
		"repo":        aboutRepo,
		"language":    aboutLanguage,
		"go_version":  runtime.Version(),
		"license":     aboutLicense,
		"description": aboutDesc,

		// 安全设计说明：明确本软件不会影响 NAS 自身与局域网其他设备。
		"safety": []map[string]string{
			{"point": "仅监听自身端口", "detail": "加速代理与控制台只监听配置声明的端口（默认 7710 / 7717），不修改系统防火墙，不做端口转发。"},
			{"point": "不劫持系统流量", "detail": "默认不启用透明网关模式，不改动 iptables/NAT/DNS 服务，NAS 自身与局域网其他设备的联网完全不受影响。"},
			{"point": "hosts 仅动托管区", "detail": "hosts 模式只在系统 hosts 文件的『GitHub++ 加速托管区』标记内写入记录，其他条目原样保留；停止服务时自动清理托管区。"},
			{"point": "加速随开关启停", "detail": "客户端只有主动把代理指向 NAS、或 hosts/Docker 配置接入后才经过本服务；关闭加速或卸载应用后一切恢复系统默认行为。"},
			{"point": "上游自动降级", "detail": "镜像源或上游不可用时自动切换到下一通道直至官方直连，不会因加速器自身故障导致网络不可用。"},
		},

		// 致谢：本系统设计参考了以下优秀的开源项目。
		"references": []map[string]string{
			{"name": "hunshcn/gh-proxy", "url": "https://github.com/hunshcn/gh-proxy", "note": "GitHub 加速的经典实现，release/archive/raw/gist 的 URL 识别与改写规则参考了该项目"},
			{"name": "jsDelivr", "url": "https://github.com/jsdelivr/jsdelivr", "note": "免费开源的全球 CDN，作为 raw 文件的优选加速通道之一"},
			{"name": "DaoCloud 公益镜像", "url": "https://github.com/DaoCloud/public-image-mirror", "note": "Docker Hub 与 ghcr/gcr/quay 的免费镜像服务，作为 Docker 加速的内置上游之一"},
			{"name": "ghproxy 系列镜像站", "url": "https://github.com/topics/github-proxy", "note": "ghproxy.net、ghfast.top 等社区公益中转站，作为内置加速源"},
			{"name": "Go 语言", "url": "https://go.dev/", "note": "本项目使用 Go 编写，单二进制、交叉编译、内存占用低，适合 NAS 常驻运行"},
		},

		// 免费开源资源说明。
		"open_source": "本软件的默认加速源均为社区免费公益服务；本地 CA 证书由程序自动生成（免费，无需购买），" +
			"用户也可以导入自己的证书与自定义加速源。",
	})
}
