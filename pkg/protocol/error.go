package protocol

import (
	"errors"
	"fmt"
)

// PreparePhase 标记 PrepareCall 内部哪一阶段失败——给 dispatcher 翻成 Verdict
// 时区分 "pre-call 协议转换 vs HTTP 构造"。
type PreparePhase int

const (
	// PhaseTranslate translator.TranslateRequest 失败（srcBody 不符合 SourceProtocol schema）。
	// 对应 dispatch.ClassInvalid——同请求换 endpoint 也会失败，应直接 abort。
	PhaseTranslate PreparePhase = iota
	// PhaseBuild adapter session BuildRequest 失败（vendor HTTP 构造错；极少见，
	// 通常是 endpoint 配置非法如 URL 不可解析）。
	// 对应 dispatch.ClassPermanent。
	PhaseBuild
)

func (p PreparePhase) String() string {
	switch p {
	case PhaseTranslate:
		return "translate"
	case PhaseBuild:
		return "build"
	default:
		return "unknown"
	}
}

// PrepareError 包装 PrepareCall 失败的细节。
//
// 调用方（dispatcher）用 errors.As 取出来分类：
//
//	var pe *PrepareError
//	if errors.As(err, &pe) {
//	    switch pe.Phase {
//	    case protocol.PhaseTranslate: ... // → ClassInvalid
//	    case protocol.PhaseBuild:     ... // → ClassPermanent
//	    }
//	}
type PrepareError struct {
	Phase PreparePhase
	Err   error
}

func (e *PrepareError) Error() string {
	return fmt.Sprintf("prepare %s: %v", e.Phase, e.Err)
}

func (e *PrepareError) Unwrap() error { return e.Err }

// NewPrepareError sugar。
func NewPrepareError(phase PreparePhase, err error) *PrepareError {
	return &PrepareError{Phase: phase, Err: err}
}

// IsPrepareError sugar——caller 不关心具体 phase 时用。
func IsPrepareError(err error) bool {
	var pe *PrepareError
	return errors.As(err, &pe)
}
