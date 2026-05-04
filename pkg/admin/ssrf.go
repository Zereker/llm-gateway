package admin

import (
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/zereker-labs/ai-gateway/pkg/repo"
)

// validateRoutingURL 拒绝指向危险目标的 endpoint URL（防 SSRF + 配置事故）。
//
// **拒绝清单**：
//   - 非 https / http scheme
//   - hostname 是 IP 字面量且属于：
//     · loopback (127.0.0.0/8 / ::1)
//     · private (10/8 / 172.16/12 / 192.168/16 / fc00::/7)
//     · link-local (169.254/16 / fe80::/10)
//     · 元数据服务（169.254.169.254 — AWS/GCP/Azure metadata）
//   - hostname 是 "localhost" / "metadata.google.internal" 等已知敏感域名
//
// **故意不**拒：
//   - 域名（无法在 admin POST 时解析；DNS rebinding 是另一类攻击，本层不防）
//   - 端口（任何端口都允许，包括 80/8080/etc）
//   - 路径（admin 配什么 path 都行）
//
// **配置覆盖**：未来 v1.x 可加 --allow-internal-endpoints flag 给真有合法理由
// （内网自托管模型）的部署用；当前 v1.0 无 escape hatch，是 strict-by-default。
//
// **空 URL** 走调用方校验（其它 routing 方式如 region/project 也合法）；本函数返 nil。
func validateRoutingURL(rc *repo.RoutingConfig) error {
	if rc == nil || rc.URL == "" {
		return nil
	}
	u, err := url.Parse(rc.URL)
	if err != nil {
		return fmt.Errorf("routing.url parse: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	default:
		return fmt.Errorf("routing.url scheme %q not allowed (need http/https)", u.Scheme)
	}

	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("routing.url missing host")
	}

	lower := strings.ToLower(host)
	switch lower {
	case "localhost", "metadata.google.internal":
		return fmt.Errorf("routing.url host %q rejected (SSRF guard)", host)
	}

	// 字面 IP 才做 RFC1918 / loopback / link-local 判断；域名让 DNS 决定（生产应在网络层拦）
	ip := net.ParseIP(host)
	if ip == nil {
		return nil
	}
	if ip.IsLoopback() {
		return fmt.Errorf("routing.url IP %s is loopback (rejected)", ip)
	}
	if ip.IsPrivate() {
		return fmt.Errorf("routing.url IP %s is private RFC1918/RFC4193 (rejected)", ip)
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return fmt.Errorf("routing.url IP %s is link-local (rejected; covers cloud metadata service)", ip)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("routing.url IP %s is unspecified (rejected)", ip)
	}
	return nil
}
