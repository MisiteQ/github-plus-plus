// Package config 定义 GitHub++ 加速器的配置模型与持久化逻辑。
//
// 配置以 YAML 形式保存，路径解析优先级为：
// 显式命令行参数 > 飞牛 fnOS 环境变量(TRIM_PKGVAR) > 用户目录 > 当前目录。
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Mode 表示加速工作模式。
type Mode string

const (
	// ModeAuto 由程序自行探测网络环境后选择最优方式。
	ModeAuto Mode = "auto"
	// ModeProxy 仅使用本地反代 + 镜像中转。
	ModeProxy Mode = "proxy"
	// ModeHosts 仅使用 DNS 优选 + hosts 注入。
	ModeHosts Mode = "hosts"
	// ModeDirect 完全旁路，不做任何加速。
	ModeDirect Mode = "direct"
)

// AllModes 列出全部合法模式，供配置校验与前端下拉框使用。
func AllModes() []Mode {
	return []Mode{ModeAuto, ModeProxy, ModeHosts, ModeDirect}
}

// IsValid 判断模式字符串是否为受支持的取值。
func (m Mode) IsValid() bool {
	for _, v := range AllModes() {
		if v == m {
			return true
		}
	}
	return false
}

// MirrorKind 区分镜像源的接入形态，决定 URL 如何改写。
type MirrorKind string

const (
	// KindPrefix 前缀型中转站，形如 https://ghproxy.com/https://github.com/...
	KindPrefix MirrorKind = "prefix"
	// KindRawCDN 专门服务 raw.githubusercontent.com 的 CDN 源。
	KindRawCDN MirrorKind = "raw"
	// KindGitClone 仅供 git clone/pull 使用的仓库镜像。
	KindGitClone MirrorKind = "git"
	// KindDirect 直连 GitHub 官方源，作为兜底选项。
	KindDirect MirrorKind = "direct"
)

// Mirror 描述一个可用的 GitHub 加速源。
type Mirror struct {
	// ID 为稳定标识，用于统计与前端引用。
	ID string `yaml:"id" json:"id"`
	// Name 是展示名称。
	Name string `yaml:"name" json:"name"`
	// URL 是源地址，语义随 Kind 变化。
	URL string `yaml:"url" json:"url"`
	// Kind 决定 URL 改写规则。
	Kind MirrorKind `yaml:"kind" json:"kind"`
	// Enabled 控制该源是否参与调度。
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Weight 是人工权重，数值越高越优先，与实测得分相乘。
	Weight float64 `yaml:"weight" json:"weight"`
	// Note 保存备注，便于用户记录来源。
	Note string `yaml:"note,omitempty" json:"note,omitempty"`
}

// ProxyConfig 控制反向代理服务的行为。
type ProxyConfig struct {
	// Listen 是 HTTP 代理服务监听地址。
	Listen string `yaml:"listen" json:"listen"`
	// TLSPort 是 HTTPS 监听端口，用于配合本地 CA 做透明加速。
	TLSPort string `yaml:"tls_port" json:"tls_port"`
	// ConnectTimeoutMS 是建立上游连接的超时毫秒数。
	ConnectTimeoutMS int `yaml:"connect_timeout_ms" json:"connect_timeout_ms"`
	// ReadTimeoutMS 是等待上游首字节的超时毫秒数。
	ReadTimeoutMS int `yaml:"read_timeout_ms" json:"read_timeout_ms"`
	// FailoverThreshold 是连续失败多少次后临时摘除该源。
	FailoverThreshold int `yaml:"failover_threshold" json:"failover_threshold"`
	// CooldownSeconds 是被摘除源的冷却时长。
	CooldownSeconds int `yaml:"cooldown_seconds" json:"cooldown_seconds"`
}

// HostsConfig 控制 DNS 优选与 hosts 注入。
type HostsConfig struct {
	// Enabled 表示是否把优选结果写入系统 hosts。
	// 关闭时仍会执行优选探测，仅是不落盘。
	Enabled bool `yaml:"enabled" json:"enabled"`
	// FilePath 是目标 hosts 文件路径。
	FilePath string `yaml:"file_path" json:"file_path"`
	// RefreshMinutes 是重新测速并刷新 hosts 的间隔。
	RefreshMinutes int `yaml:"refresh_minutes" json:"refresh_minutes"`
	// CandidateLimit 是每个域名参与测速的候选 IP 数量上限。
	CandidateLimit int `yaml:"candidate_limit" json:"candidate_limit"`
	// ProbeTimeoutMS 是单个 IP 测速超时毫秒数。
	ProbeTimeoutMS int `yaml:"probe_timeout_ms" json:"probe_timeout_ms"`

	// EnabledSet 记录用户是否显式设置过 Enabled。
	//
	// 存在的意义：区分"用户主动关闭"与"配置文件来自旧版本、字段缺失"。
	// 前者不该被新默认值覆盖，后者应当采纳新默认值。
	EnabledSet bool `yaml:"enabled_set,omitempty" json:"-"`
}

// AutoConfig 定义自动模式的判定阈值。
type AutoConfig struct {
	// ProbeIntervalMinutes 是重新评估网络环境的间隔。
	ProbeIntervalMinutes int `yaml:"probe_interval_minutes" json:"probe_interval_minutes"`
	// DirectBetterRatio 表示直连快于加速通道多少倍时才回退直连。
	DirectBetterRatio float64 `yaml:"direct_better_ratio" json:"direct_better_ratio"`
	// MinImproveRatio 表示加速通道至少要比直连快多少倍才启用。
	MinImproveRatio float64 `yaml:"min_improve_ratio" json:"min_improve_ratio"`
}

// ServerConfig 控制 Web 控制台。
type ServerConfig struct {
	// Listen 是控制台监听地址。
	Listen string `yaml:"listen" json:"listen"`
	// Username 是控制台登录用户名。
	Username string `yaml:"username" json:"username"`
	// Password 是控制台登录密码，默认 admin123，登录后可在控制台修改。
	Password string `yaml:"password" json:"password"`
	// SessionTTLHours 是登录会话有效期小时数。
	SessionTTLHours int `yaml:"session_ttl_hours" json:"session_ttl_hours"`
}

// DockerConfig 控制 Docker 镜像拉取加速。
//
// 实现方式：代理端口同时充当 Docker Registry v2 入口，
// 把 /etc/docker/daemon.json 的 registry-mirrors 指向本机即可加速 docker pull。
type DockerConfig struct {
	// Enabled 表示是否在代理端口上启用 Docker Registry 加速。
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Upstreams 是 Docker 上游列表，按优先级排序。
	// 支持镜像站地址（https://docker.1ms.run 等）与内置值 "official"（Docker 官方源）。
	// 请求会依次尝试，某个上游不可用时自动切换下一个。
	Upstreams []string `yaml:"upstreams" json:"upstreams"`
}

// Config 是应用的总配置。
type Config struct {
	Mode    Mode         `yaml:"mode" json:"mode"`
	Proxy   ProxyConfig  `yaml:"proxy" json:"proxy"`
	Hosts   HostsConfig  `yaml:"hosts" json:"hosts"`
	Auto    AutoConfig   `yaml:"auto" json:"auto"`
	Server  ServerConfig `yaml:"server" json:"server"`
	Docker  DockerConfig `yaml:"docker" json:"docker"`
	Mirrors []Mirror     `yaml:"mirrors" json:"mirrors"`

	// DataDir 记录配置与运行数据的存放目录，不写入 YAML。
	DataDir string `yaml:"-" json:"data_dir"`
	// path 记录配置文件自身位置，不写入 YAML。
	path string `yaml:"-" json:"-"`

	// mu 用指针持有，使得 Config 可以安全地按值拷贝快照。
	mu *sync.RWMutex `yaml:"-" json:"-"`
}

// Default 返回一份带内置镜像源与合理默认值的配置。
func Default() *Config {
	return &Config{
		mu:   &sync.RWMutex{},
		Mode: ModeAuto,
		Proxy: ProxyConfig{
			Listen:            "0.0.0.0:7710",
			TLSPort:           "7711",
			ConnectTimeoutMS:  8000,
			ReadTimeoutMS:     30000,
			FailoverThreshold: 3,
			CooldownSeconds:   300,
		},
		Hosts: HostsConfig{
			// 默认开启优选能力：程序会持续探测最优 IP。
			// 是否真正写入系统 hosts 还会额外检查文件写入权限，
			// 没有权限时仅展示结果，不会报错中断服务。
			Enabled:        true,
			FilePath:       defaultHostsPath(),
			RefreshMinutes: 60,
			CandidateLimit: 6,
			ProbeTimeoutMS: 3000,
		},
		Auto: AutoConfig{
			ProbeIntervalMinutes: 30,
			DirectBetterRatio:    1.5,
			MinImproveRatio:      1.3,
		},
		Server: ServerConfig{
			Listen:          "0.0.0.0:7717",
			Username:        "admin",
			Password:        "admin123",
			SessionTTLHours: 72,
		},
		Docker: DockerConfig{
			Enabled:   true,
			Upstreams: BuiltinDockerUpstreams(),
		},
		Mirrors: BuiltinMirrors(),
	}
}

// defaultHostsPath 依据操作系统返回系统 hosts 文件位置。
func defaultHostsPath() string {
	if isWindows() {
		root := os.Getenv("SystemRoot")
		if root == "" {
			root = `C:\Windows`
		}
		return filepath.Join(root, "System32", "drivers", "etc", "hosts")
	}
	return "/etc/hosts"
}

// ResolveDataDir 决定运行数据目录。
//
// 优先级：显式传入 > TRIM_PKGVAR(飞牛 fnOS) > 用户主目录下的 .ghpp > 当前目录。
func ResolveDataDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("TRIM_PKGVAR"); v != "" {
		return v
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".ghpp")
	}
	return ".ghpp"
}

// ResolveEtcDir 决定配置文件目录，飞牛环境下使用 TRIM_PKGETC。
func ResolveEtcDir(fallback string) string {
	if v := os.Getenv("TRIM_PKGETC"); v != "" {
		return v
	}
	return fallback
}

// Load 从指定目录读取配置，文件不存在时写入默认配置并返回。
func Load(dataDir string) (*Config, error) {
	etcDir := ResolveEtcDir(dataDir)
	if err := os.MkdirAll(etcDir, 0o755); err != nil {
		return nil, fmt.Errorf("创建配置目录失败: %w", err)
	}

	cfg := Default()
	cfg.DataDir = dataDir
	cfg.path = filepath.Join(etcDir, "config.yaml")

	var freshConfig bool
	diskPassword := ""
	data, err := os.ReadFile(cfg.path)
	switch {
	case err == nil:
		// 先解析出文件中实际出现的键，以便区分"用户显式设置"与"字段缺失"。
		if err := detectExplicitFields(data, cfg); err != nil {
			return nil, fmt.Errorf("解析配置文件 %s 失败: %w", cfg.path, err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("解析配置文件 %s 失败: %w", cfg.path, err)
		}
		// 记录落盘时的原始密码，供 normalize 生成随机密码后判断是否需要回写。
		diskPassword = cfg.Server.Password
	case os.IsNotExist(err):
		freshConfig = true
	default:
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}

	// 配置迁移：旧版本的配置文件中没有 hosts.enabled 字段，
	// 此时 YAML 解析会得到零值 false，会意外覆盖新版本的默认开启行为。
	// 通过 EnabledSet 标记识别这种情况，采纳新的默认值。
	if !cfg.Hosts.EnabledSet {
		cfg.Hosts.Enabled = Default().Hosts.Enabled
		cfg.Hosts.EnabledSet = true
	}

	cfg.normalize()

	// 首次运行落盘默认配置；配置文件里密码为空时也必须回写。
	// normalize 会把空密码回退为默认密码，若不立即持久化，历史空密码
	// 配置每次启动都会在内存里回退一次，配置文件与实际登录凭据不一致。
	// 回写后配置文件始终反映真实的登录凭据，可在控制台修改。
	if freshConfig || diskPassword == "" {
		if err := cfg.Save(); err != nil {
			return nil, err
		}
	}
	return cfg, nil
}

// detectExplicitFields 检查配置文件中是否有用户显式写入的字段。
//
// 目前用于判断 hosts.enabled 是用户主动设置还是旧版本遗留的缺失项。
// 做法是先把文件内容解析成通用映射，再逐层查键是否存在。
func detectExplicitFields(data []byte, cfg *Config) error {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return err
	}

	if hosts, ok := raw["hosts"].(map[string]any); ok {
		if _, exists := hosts["enabled"]; exists {
			cfg.Hosts.EnabledSet = true
		}
	}
	return nil
}

// normalize 补齐缺失字段并校正非法取值，保证运行期配置始终可用。
func (c *Config) normalize() {
	if c.mu == nil {
		c.mu = &sync.RWMutex{}
	}
	if !c.Mode.IsValid() {
		c.Mode = ModeAuto
	}
	if c.Proxy.Listen == "" {
		c.Proxy.Listen = "0.0.0.0:7710"
	}
	if c.Proxy.ConnectTimeoutMS <= 0 {
		c.Proxy.ConnectTimeoutMS = 8000
	}
	if c.Proxy.ReadTimeoutMS <= 0 {
		c.Proxy.ReadTimeoutMS = 30000
	}
	if c.Proxy.FailoverThreshold <= 0 {
		c.Proxy.FailoverThreshold = 3
	}
	if c.Proxy.CooldownSeconds <= 0 {
		c.Proxy.CooldownSeconds = 300
	}
	if c.Hosts.FilePath == "" {
		c.Hosts.FilePath = defaultHostsPath()
	}
	if c.Hosts.RefreshMinutes <= 0 {
		c.Hosts.RefreshMinutes = 60
	}
	if c.Hosts.CandidateLimit <= 0 {
		c.Hosts.CandidateLimit = 6
	}
	if c.Hosts.ProbeTimeoutMS <= 0 {
		c.Hosts.ProbeTimeoutMS = 3000
	}
	if c.Auto.ProbeIntervalMinutes <= 0 {
		c.Auto.ProbeIntervalMinutes = 30
	}
	if c.Auto.DirectBetterRatio <= 1 {
		c.Auto.DirectBetterRatio = 1.5
	}
	if c.Auto.MinImproveRatio <= 1 {
		c.Auto.MinImproveRatio = 1.3
	}
	if c.Server.Listen == "" {
		c.Server.Listen = "0.0.0.0:7717"
	}
	if c.Server.Username == "" {
		c.Server.Username = "admin"
	}
	if c.Server.SessionTTLHours <= 0 {
		c.Server.SessionTTLHours = 72
	}
	if c.Server.Password == "" {
		// 密码不随机生成：随机值不看配置文件就无从得知，对 NAS 局域网
		// 场景徒增困扰。统一回退到固定默认密码，登录后可在控制台修改。
		c.Server.Password = "admin123"
	}
	// Docker 上游列表缺省时补上内置值，"official" 必须保留在末尾作为兜底。
	if len(c.Docker.Upstreams) == 0 {
		c.Docker.Upstreams = BuiltinDockerUpstreams()
	}
	if len(c.Mirrors) == 0 {
		c.Mirrors = BuiltinMirrors()
	}
	// 补全缺失的 ID，并丢弃既无 ID 也无 URL 的无效条目。
	seen := make(map[string]bool, len(c.Mirrors))
	valid := c.Mirrors[:0]
	for i := range c.Mirrors {
		m := c.Mirrors[i]
		if m.URL == "" {
			continue
		}
		if m.ID == "" {
			m.ID = fmt.Sprintf("mirror-%s", randomToken(4))
		}
		if seen[m.ID] {
			continue
		}
		if m.Weight == 0 {
			m.Weight = 1
		}
		seen[m.ID] = true
		valid = append(valid, m)
	}
	c.Mirrors = valid
}

// Save 将配置原子写入磁盘，避免进程中断导致文件损坏。
func (c *Config) Save() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.saveLocked()
}

func (c *Config) saveLocked() error {
	if c.path == "" {
		c.path = filepath.Join(ResolveEtcDir(c.DataDir), "config.yaml")
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("写入临时配置失败: %w", err)
	}
	return os.Rename(tmp, c.path)
}

// Path 返回配置文件所在路径。
func (c *Config) Path() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.path
}

// Snapshot 返回配置的深拷贝，供 API 层安全读取。
//
// 拷贝出的副本拥有独立的锁，读取时不会与原配置相互阻塞。
func (c *Config) Snapshot() *Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cp := *c
	cp.Mirrors = make([]Mirror, len(c.Mirrors))
	copy(cp.Mirrors, c.Mirrors)
	cp.mu = &sync.RWMutex{}
	return &cp
}

// Update 在写锁保护下修改配置并立即持久化。
func (c *Config) Update(fn func(*Config) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := fn(c); err != nil {
		return err
	}
	c.normalize()
	return c.saveLocked()
}

// EnabledMirrors 返回当前启用的镜像源副本。
func (c *Config) EnabledMirrors() []Mirror {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Mirror, 0, len(c.Mirrors))
	for _, m := range c.Mirrors {
		if m.Enabled {
			out = append(out, m)
		}
	}
	return out
}

// randomToken 生成指定字节长度的十六进制随机串，用于密码与 ID。
func randomToken(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// 极少数情况下系统熵源不可用，退回时间戳派生的可读串。
		return fmt.Sprintf("%x", os.Getpid())
	}
	return hex.EncodeToString(buf)
}

// RandomToken 对外暴露随机串生成，供认证模块复用。
func RandomToken(n int) string { return randomToken(n) }

// JoinHostPort 安全拼接监听地址，允许用户只填写端口号。
func JoinHostPort(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	if !strings.Contains(addr, ":") {
		return "0.0.0.0:" + addr
	}
	return addr
}
