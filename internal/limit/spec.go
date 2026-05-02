// Package limit 定义限流层的类型与接口：三层 AND 级联 + 四级查询链。
//
// 详见 docs/architecture/04-rate-limiting.md。
package limit

// Spec M6 Limit middleware 为本次请求构建的三层阈值快照。
type Spec struct {
	User     LayerSpec
	Service  *LayerSpec // 仅 identity.Group == "default" 时非 nil
	Endpoint *LayerSpec // M7 选定 endpoint 后由 dispatch 层填充

	EndpointID string // 选定后填；Consume 时使用

	// 来源标识（trace / debug）
	UserSource    Source
	ServiceSource Source
}

// LayerSpec 单层阈值。RPM / TPM / RPS 中 0 表示不限。
type LayerSpec struct {
	RPM   int64
	TPM   int64
	RPS   int64
	Extra map[string]int64 // 可选维度，如 audio_seconds_per_min
}

// Source 阈值来源标识。
type Source int

const (
	SourceHardcoded    Source = iota // 代码兜底默认值
	SourceModelDefault               // ModelService.SpecDetail 中的默认值
	SourceUser                       // 用户级配置
	SourceAPIKey                     // API Key 级配置
)

func (s Source) String() string {
	switch s {
	case SourceAPIKey:
		return "apikey"
	case SourceUser:
		return "user"
	case SourceModelDefault:
		return "model_default"
	default:
		return "hardcoded"
	}
}
