package translator

import (
	"log/slog"
	"sync"

	"github.com/tidwall/gjson"

	"github.com/zereker/llm-gateway/internal/domain"
	"github.com/zereker/llm-gateway/internal/metric"
)

// warnedLossy remembers which (src, tgt, feature) combinations have already been
// logged, so a client that sends tools on every request produces one warning,
// not one per request. The metric is still incremented on every call.
var warnedLossy sync.Map

// ReportLossyRequest inspects a client request body and, for each feature that
// this cross-protocol translator silently drops (tool definitions / tool calls,
// non-text multimodal content), increments a metric and logs a one-time warning.
// This makes lossy translation observable instead of silent — the same
// discipline as the pivot-composition warning in internal/protocol.
//
// only, if non-empty, restricts reporting to that set of feature labels: a pair
// that has since implemented tool translation passes only="multimodal" so it no
// longer warns about tools/tool_calls it now carries. With no filter, every
// detected feature is reported.
//
// It never mutates the body and never fails; detection is best-effort via gjson.
// Same-protocol (identity) translators carry everything through and must not
// call this.
func ReportLossyRequest(src, tgt domain.Protocol, clientBody []byte, only ...string) {
	for _, feature := range droppedRequestFeatures(src, clientBody) {
		if len(only) > 0 && !containsString(only, feature) {
			continue
		}
		metric.Inc(metric.TranslatorFeatureDroppedTotal,
			"src", src.String(), "tgt", tgt.String(), "feature", feature)
		key := src.String() + "|" + tgt.String() + "|" + feature
		if _, seen := warnedLossy.LoadOrStore(key, struct{}{}); seen {
			continue
		}
		slog.Warn("translator: cross-protocol translation drops an unsupported request feature",
			"src", src.String(), "tgt", tgt.String(), "feature", feature)
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// droppedRequestFeatures returns the lossy feature labels present in the client
// body, judged by the source protocol's shape. Text content is always carried,
// so it is never reported.
func droppedRequestFeatures(src domain.Protocol, body []byte) []string {
	var feats []string
	add := func(f string) {
		for _, e := range feats {
			if e == f {
				return
			}
		}
		feats = append(feats, f)
	}

	// Tool definitions live under the top-level "tools" array in both the OpenAI
	// and Anthropic request shapes.
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() && len(tools.Array()) > 0 {
		add("tools")
	}

	switch src {
	case domain.ProtoAnthropic:
		// Anthropic carries tool calls / images as typed content blocks.
		gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
			msg.Get("content").ForEach(func(_, block gjson.Result) bool {
				switch block.Get("type").String() {
				case "tool_use", "tool_result":
					add("tool_calls")
				case "image":
					add("multimodal")
				}
				return true
			})
			return true
		})
	default:
		// OpenAI-family request shape: tool_calls on assistant messages, role
		// "tool" result messages, and array content parts other than text.
		gjson.GetBytes(body, "messages").ForEach(func(_, msg gjson.Result) bool {
			if msg.Get("tool_calls").IsArray() || msg.Get("role").String() == "tool" {
				add("tool_calls")
			}
			if content := msg.Get("content"); content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					if t := part.Get("type").String(); t != "" && t != "text" {
						add("multimodal")
					}
					return true
				})
			}
			return true
		})
	}
	return feats
}
