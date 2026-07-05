package openai_cohere
import ("strings";"testing";"github.com/tidwall/gjson")
func TestCohereStreamTranslate(t *testing.T) {
	h := &responseHandler{}
	// 模拟 Cohere v2 SSE，分两段喂(含半行跨 Feed)
	out1,_ := h.Feed([]byte("data: {\"type\":\"message-start\"}\n\ndata: {\"type\":\"content-delta\",\"delta\":{\"message\":{\"content\":{\"text\":\"Hel\"}}}}\n\ndata: {\"type\":\"content-de"))
	out2,_ := h.Feed([]byte("lta\",\"delta\":{\"message\":{\"content\":{\"text\":\"lo\"}}}}\n\ndata: {\"type\":\"message-end\",\"delta\":{\"finish_reason\":\"COMPLETE\",\"usage\":{\"tokens\":{\"input_tokens\":3,\"output_tokens\":2}}}}\n\n"))
	fin,usage,_ := h.Flush()
	all := string(out1)+string(out2)+string(fin)
	// 拼出的文本
	var text strings.Builder
	for _, line := range strings.Split(all,"\n") {
		line=strings.TrimSpace(line)
		if !strings.HasPrefix(line,"data: {"){continue}
		text.WriteString(gjson.Get(line[6:],"choices.0.delta.content").String())
	}
	if text.String()!="Hello" { t.Errorf("streamed text=%q want Hello", text.String()) }
	if !strings.Contains(all,"[DONE]") { t.Error("缺 [DONE]") }
	if !strings.Contains(all,`"finish_reason":"stop"`) { t.Error("缺 finish_reason stop") }
	if usage==nil || usage.Total!=5 { t.Errorf("usage=%+v want total 5", usage) }
}
