package invoker

import (
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// awsIMDSv6 是 AWS 实例元数据服务（IMDS）的 IPv6 地址 fd00:ec2::254。它属 ULA
// （fc00::/7），**不是** link-local，所以 netip.Addr.IsLinkLocalUnicast 抓不到——
// 单列硬编码。其余云厂商（GCP / Azure / Oracle / DO）的 IMDS 都是 IPv4
// 169.254.169.254，落在 link-local，由 IsLinkLocalUnicast 覆盖。
var awsIMDSv6 = netip.MustParseAddr("fd00:ec2::254")

// blockMetadataDial 是 net.Dialer.Control 钩子：真正建立连接之前，对**已解析出的
// 目标 IP** 做云 metadata 端点拦截。
//
// **为什么在 Control 而非校验 URL 主机名**：Control 拿到的是 DNS 解析后即将拨号的
// 实际 IP，能挡住 DNS-rebinding——攻击者把 endpoint 配成 `http://evil.com/...`，
// evil.com 解析到 169.254.169.254，纯主机名校验会放行，dial 层校验则精准拦住。
// 每个候选地址都会过一次 Control，多 A 记录也覆盖。
//
// **只挡 metadata，不挡私网**：自建上游常驻 10/172.16/192.168 私网，docs/07 §5 +
// MED#12 明确允许；这里仅拦截 link-local（含所有云 IMDS v4 的 169.254.169.254）、
// IPv6 link-local 与 AWS IMDSv6，绝不误伤私网自建 endpoint。
func blockMetadataDial(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		// Control 阶段 address 应已是 IP:port；解析不出来不做主机名猜测，放行。
		return nil
	}
	if IsMetadataIP(ip) {
		return fmt.Errorf("invoker: refusing to dial cloud metadata endpoint %s (SSRF guard)", ip)
	}
	return nil
}

// IsMetadataIP 判断 ip 是否云 metadata 端点。dial-time 防线与启动期 endpoint
// 扫描共用它，保证"什么算 metadata"只有一处定义。
//
//	169.254.0.0/16 (IPv4 link-local) ── AWS/GCP/Azure/Oracle/DigitalOcean IMDS
//	fe80::/10      (IPv6 link-local)
//	fd00:ec2::254  (AWS IMDSv6，ULA，link-local 判定抓不到)
func IsMetadataIP(ip netip.Addr) bool {
	ip = ip.Unmap() // ::ffff:169.254.169.254 → 169.254.169.254
	if ip.IsLinkLocalUnicast() {
		return true
	}
	return ip == awsIMDSv6
}
