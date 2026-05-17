package domain

// Secret 是凭证 / 密钥的字符串别名，自带屏蔽实现：
//
//   - String() / fmt.Stringer 返回 "***"
//   - MarshalJSON / MarshalText 返回 "***"
//
// 通过 .Reveal() 显式取明文（仅 Adapter 调上游时使用）。
//
// 设计原则（docs/06 §3）：纯业务类型，无 SQL 依赖；底层 string 让 sqlx / gorm 走 string 转换路径。
type Secret string

func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return "***"
}

func (s Secret) MarshalJSON() ([]byte, error) {
	if s == "" {
		return []byte(`""`), nil
	}
	return []byte(`"***"`), nil
}

func (s Secret) MarshalText() ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	return []byte("***"), nil
}

// Reveal 返回明文。**只在传给上游 HTTP / SDK 时调用**。
func (s Secret) Reveal() string { return string(s) }
