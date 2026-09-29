// Package netutil 提供本机网络接口探测工具。
//
// 主要用户是控制台「接入方式」区域：需要把 NAS 的所有局域网 IPv4 列出来，
// 让用户在前端切换"用哪个地址生成接入示例"——飞牛 NAS 多网卡、多 LAN 口、
// Docker 网桥等场景下机器会有多个 IPv4，单一探测容易选错。
package netutil

import (
	"net"
	"strings"
)

// LocalIPs 返回本机所有可用于局域网通信的 IPv4 地址。
//
// 过滤规则：
//   - 跳过回环（127.0.0.0/8）；
//   - 跳过链路本地（169.254.0.0/16，常见于未获取 DHCP 的网卡）；
//   - 跳过 Docker / 容器网桥的 172.17.0.0/16 等常见内部网段（避免把 docker0
//     地址当成局域网地址展示给用户，用户照着配也连不上）；
//   - 仅返回 IPv4，IPv6 在局域网加速场景几乎不用，先不暴露。
//
// 返回顺序保持系统接口顺序，第一个通常是主网卡。
func LocalIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}

	out := make([]string, 0, len(addrs))
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		ip4 := ipNet.IP.To4()
		if ip4 == nil {
			continue
		}
		s := ip4.String()
		if isLinkLocal(s) || isDockerBridge(s) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// isLinkLocal 判断是否为 169.254.0.0/16 链路本地地址。
// 这种地址通常是网卡没拿到 DHCP 时操作系统临时分配的，用户照着配也连不上。
func isLinkLocal(ip string) bool {
	return strings.HasPrefix(ip, "169.254.")
}

// isDockerBridge 判断是否为 Docker / 容器网桥常见内部地址。
// 飞牛 NAS 上 docker0 默认 172.17.0.1，自定义网桥多在 172.18~172.31，
// 这些地址对外不可达，展示给用户会造成困惑。
// 仍保留 192.168.* 与 10.* 与真正的 172.16~172.31 局域网（少见但合法）。
func isDockerBridge(ip string) bool {
	// 仅过滤 docker0 默认网段 172.17.0.0/16 与 br-* 自定义网桥 172.18~172.31。
	// 172.16.0.0/12 中 172.16 是合法企业局域网，不能误杀。
	if !strings.HasPrefix(ip, "172.") {
		return false
	}
	parts := strings.Split(ip, ".")
	if len(parts) < 2 {
		return false
	}
	// 第二段 17~31 视为 docker 自定义网桥；16 保留给真实局域网。
	switch parts[1] {
	case "17", "18", "19", "20", "21", "22", "23", "24", "25", "26", "27", "28", "29", "30", "31":
		return true
	}
	return false
}

// PrimaryLAN 返回主局域网 IPv4，取 LocalIPs 第一个；无可用时返回空串。
//
// 用于启动期打印访问地址等"只需要一个地址"的场景。
func PrimaryLAN() string {
	if ips := LocalIPs(); len(ips) > 0 {
		return ips[0]
	}
	return ""
}
