package handler

import "net"

// IsLocalOrPrivateIP 判定 IP 是否落在 loopback / link-local / 私有网段 / unspecified。
// 空串或非法 IP 一律视为"本地"安全降级。
func IsLocalOrPrivateIP(ipStr string) bool {
	if ipStr == "" {
		return true
	}
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return true
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}
