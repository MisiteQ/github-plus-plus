package main

import "net"

// netInterfaces 返回本机所有非回环 IPv4 地址。
//
// 单独抽出是为了让探测逻辑可测试，也便于将来做多网卡优选。
func netInterfaces() ([]string, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
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
		out = append(out, ip4.String())
	}
	return out, nil
}
