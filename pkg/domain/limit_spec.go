package domain

// LimitSpec M6 Limit middleware 为本次请求构建的三层阈值快照。
type LimitSpec struct {
	User     LayerSpec
	Service  *LayerSpec // 仅 Identity.Group == "default" 时非 nil
	Endpoint *LayerSpec // M7 选定 endpoint 后由 dispatch 层填充

	EndpointID string // 选定后填；Consume 时使用

	// 来源标识（trace / debug）
	UserSource    LimitSource
	ServiceSource LimitSource
}

// LayerSpec 单层阈值。RPM / TPM / RPS 中 0 表示不限。
type LayerSpec struct {
	RPM   int64
	TPM   int64
	RPS   int64
	Extra map[string]int64 // 可选维度，如 audio_seconds_per_min
}

// LimitSource 阈值来源标识。
type LimitSource int

const (
	LimitSourceHardcoded    LimitSource = iota // 代码兜底默认值
	LimitSourceModelDefault                    // ModelService.SpecDetail 中的默认值
	LimitSourceUser                            // 用户级配置
	LimitSourceAPIKey                          // API Key 级配置
)

func (s LimitSource) String() string {
	switch s {
	case LimitSourceAPIKey:
		return "apikey"
	case LimitSourceUser:
		return "user"
	case LimitSourceModelDefault:
		return "model_default"
	default:
		return "hardcoded"
	}
}
