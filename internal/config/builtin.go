package config

import "time"

// BuiltinMirrors 返回出厂内置的 GitHub 加速源列表。
//
// 这些源均为社区公开的公益中转服务，可用性随时间波动，
// 因此程序会在运行期持续测速并按实测结果动态排序，
// 用户也可以在控制台中增删自己的私有源。
func BuiltinMirrors() []Mirror {
	return []Mirror{
		// ---- 前缀型中转站：覆盖 releases / archive / raw / git clone ----
		{ID: "ghproxy-net", Name: "ghproxy.net", URL: "https://ghproxy.net/", Kind: KindPrefix, Enabled: true, Weight: 1, Note: "日本节点，覆盖面广"},
		{ID: "ghfast-top", Name: "ghfast.top", URL: "https://ghfast.top/", Kind: KindPrefix, Enabled: true, Weight: 1, Note: "韩国首尔节点"},
		{ID: "gh-proxy-com", Name: "gh-proxy.com", URL: "https://gh-proxy.com/", Kind: KindPrefix, Enabled: true, Weight: 1, Note: "韩国首尔节点，大文件表现好"},
		{ID: "githubfast", Name: "githubfast.com", URL: "https://githubfast.com/", Kind: KindPrefix, Enabled: true, Weight: 1, Note: "韩国首尔节点"},
		{ID: "ghproxy-vip", Name: "ghproxy.vip", URL: "https://ghproxy.vip/", Kind: KindPrefix, Enabled: true, Weight: 0.9, Note: "多线路，支持 LFS"},
		{ID: "moeyy", Name: "github.moeyy.xyz", URL: "https://github.moeyy.xyz/", Kind: KindPrefix, Enabled: true, Weight: 0.9, Note: "美国节点"},
		{ID: "hub-gitmirror", Name: "hub.gitmirror.com", URL: "https://hub.gitmirror.com/", Kind: KindPrefix, Enabled: true, Weight: 0.9, Note: "适合大体积 Release"},
		{ID: "gh-llkk", Name: "gh.llkk.cc", URL: "https://gh.llkk.cc/", Kind: KindPrefix, Enabled: true, Weight: 0.8, Note: "国内 CDN"},

		// ---- Raw 文件专用 CDN ----
		{ID: "jsdelivr", Name: "jsDelivr CDN", URL: "https://fastly.jsdelivr.net/", Kind: KindRawCDN, Enabled: true, Weight: 1, Note: "全球 CDN，raw 文件最快"},
		{ID: "raw-kkgithub", Name: "raw.kkgithub.com", URL: "https://raw.kkgithub.com/", Kind: KindRawCDN, Enabled: true, Weight: 0.8, Note: "香港节点"},

		// ---- Git 仓库镜像 ----
		{ID: "gitclone", Name: "gitclone.com", URL: "https://gitclone.com/", Kind: KindGitClone, Enabled: true, Weight: 0.9, Note: "国内 clone 镜像，首次较慢后续走缓存"},
		{ID: "kkgithub", Name: "kkgithub.com", URL: "https://kkgithub.com/", Kind: KindGitClone, Enabled: true, Weight: 0.8, Note: "香港节点，GitHub 全站镜像"},

		// ---- 官方直连：始终保留作为兜底 ----
		{ID: "origin", Name: "GitHub 官方直连", URL: "https://github.com/", Kind: KindDirect, Enabled: true, Weight: 0.5, Note: "兜底源，网络通畅时最快"},
	}
}

// BuiltinDockerUpstreams 返回出厂内置的 Docker 上游列表。
//
// 依次为社区公益镜像站，最后 "official" 表示 Docker Hub 官方源（registry-1.docker.io），
// 作为兜底。请求按顺序尝试，上游不可用时自动切换。
func BuiltinDockerUpstreams() []string {
	return []string{
		"https://docker.1ms.run",
		"https://docker.m.daocloud.io",
		"https://docker.xuanyuan.me",
		"official",
	}
}

// GHHost 描述一个需要加速的 GitHub 域名及其属性。
type GHHost struct {
	// Host 是域名本身。
	Host string
	// Category 是域名分类，决定路由到哪一类镜像源。
	Category string
	// TLS 表示该域名是否强制 HTTPS。
	TLS bool
}

// 域名分类常量，与镜像源的 Kind 一一对应。
const (
	CatWeb   = "web"   // 网页与 API
	CatRaw   = "raw"   // 裸文件
	CatClone = "clone" // git 仓库传输
)

// GitHubHosts 列出全部受管的 GitHub 域名。
//
// 这些域名会在代理层被识别并重定向到加速通道，
// 同时也会作为 DNS 优选的目标。
func GitHubHosts() []GHHost {
	return []GHHost{
		{Host: "github.com", Category: CatWeb, TLS: true},
		{Host: "www.github.com", Category: CatWeb, TLS: true},
		{Host: "api.github.com", Category: CatWeb, TLS: true},
		{Host: "gist.github.com", Category: CatWeb, TLS: true},
		{Host: "codeload.github.com", Category: CatWeb, TLS: true},
		{Host: "raw.githubusercontent.com", Category: CatRaw, TLS: true},
		{Host: "gist.githubusercontent.com", Category: CatRaw, TLS: true},
		{Host: "objects.githubusercontent.com", Category: CatRaw, TLS: true},
		{Host: "release-assets.githubusercontent.com", Category: CatRaw, TLS: true},
		{Host: "github-releases.githubusercontent.com", Category: CatRaw, TLS: true},
		{Host: "cam.githubusercontent.com", Category: CatRaw, TLS: true},
		{Host: "avatars.githubusercontent.com", Category: CatRaw, TLS: true},
		{Host: "user-images.githubusercontent.com", Category: CatRaw, TLS: true},
		{Host: "private-user-images.githubusercontent.com", Category: CatRaw, TLS: true},
		{Host: "media.githubusercontent.com", Category: CatRaw, TLS: true},
	}
}

// HostSet 返回受管域名的集合，便于 O(1) 判断。
func HostSet() map[string]GHHost {
	hs := GitHubHosts()
	out := make(map[string]GHHost, len(hs))
	for _, h := range hs {
		out[h.Host] = h
	}
	return out
}

// DirectPreferredHosts 列出应当优先直连的域名。
//
// 原因：这些接口对请求来源与认证态敏感，
// 经第三方镜像中转容易被 GitHub 判定为异常流量而返回 403/401，
// 直连的成功率反而更高。程序仅在直连失败时才会使用镜像。
func DirectPreferredHosts() map[string]bool {
	return map[string]bool{
		"api.github.com":  true,
		"github.com":      true, // 网页与登录态相关请求
		"www.github.com":  true,
		"gist.github.com": true,
	}
}

// HostIsDirectPreferred 判断某域名是否应优先直连。
func HostIsDirectPreferred(host string) bool {
	return DirectPreferredHosts()[host]
}

// DomainProbeTargets 返回 DNS 优选时使用的探测目标，
// 每一项都附带一个体积可控的真实文件路径用于吞吐测速。
func DomainProbeTargets() []ProbeTarget {
	return []ProbeTarget{
		{Host: "github.com", Path: "/favicon.ico", Category: CatWeb},
		{Host: "api.github.com", Path: "/meta", Category: CatWeb},
		{Host: "raw.githubusercontent.com", Path: "/github/gitignore/main/Go.gitignore", Category: CatRaw},
		{Host: "codeload.github.com", Path: "/github/gitignore/tar.gz/refs/heads/main", Category: CatRaw},
		{Host: "objects.githubusercontent.com", Path: "/", Category: CatRaw},
	}
}

// ProbeTarget 描述一次测速探测的目标。
type ProbeTarget struct {
	Host     string `json:"host"`
	Path     string `json:"path"`
	Category string `json:"category"`
}

// DefaultUserAgent 是代理转发时使用的 UA，尽量贴近真实浏览器以避免被判为异常流量。
const DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// ProbeUserAgent 是测速探测使用的 UA，标识自身便于镜像站统计。
const ProbeUserAgent = "ghpp-probe/1.0"

// ScoreHalfLife 是测速得分的半衰期，越久远的成绩权重越低。
const ScoreHalfLife = 10 * time.Minute
