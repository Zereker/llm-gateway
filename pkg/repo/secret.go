package repo

// Secret 是凭证 / 密钥的字符串别名，自带屏蔽实现：
//
//   - String() / fmt.Stringer 返回 "***"
//   - MarshalJSON / MarshalText 返回 "***"
//
// 通过 .Reveal() 显式取明文（仅 Adapter 调上游时使用）。
// 这样 *RequestContext 的 dump、日志打印、配置导出都不会泄漏 API key。
//
// 底层是 string，sqlx / gorm 都能透明 scan / value（走 string 转换路径）。
type Secret string

// String 屏蔽明文。
func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return "***"
}

// MarshalJSON 屏蔽明文。
func (s Secret) MarshalJSON() ([]byte, error) {
	if s == "" {
		return []byte(`""`), nil
	}
	return []byte(`"***"`), nil
}

// MarshalText 屏蔽明文（用于 yaml 等 text marshaler）。
func (s Secret) MarshalText() ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	return []byte("***"), nil
}

// Reveal 返回明文。**只在传给上游 HTTP / SDK 时调用**。
func (s Secret) Reveal() string { return string(s) }
