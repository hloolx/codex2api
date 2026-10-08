package proxy

import "github.com/tidwall/gjson"

// Native Gemini usage is a cumulative snapshot, not a per-frame delta. Keep
// only counters, and preserve fields omitted by a later usage-only tail.
type geminiNativeUsage struct {
	inputTokens     int
	outputTokens    int
	reasoningTokens int
	cachedTokens    int
	totalTokens     int
}

func (u *geminiNativeUsage) observe(body []byte) {
	// The native response adapter has already unwrapped response and restored
	// cpaUsageMetadata to usageMetadata before these bytes reach the handler.
	metadata := gjson.GetBytes(body, "usageMetadata")
	if !metadata.IsObject() {
		return
	}
	for _, field := range []struct {
		name  string
		value *int
	}{
		{"promptTokenCount", &u.inputTokens},
		{"candidatesTokenCount", &u.outputTokens},
		{"thoughtsTokenCount", &u.reasoningTokens},
		{"cachedContentTokenCount", &u.cachedTokens},
		{"totalTokenCount", &u.totalTokens},
	} {
		if count := metadata.Get(field.name); count.Type == gjson.Number {
			*field.value = max(0, int(count.Int()))
		}
	}
}

func (u *geminiNativeUsage) usageInfo() UsageInfo {
	// Match the Responses adapter: input includes cached tokens and output
	// includes thinking. ReasoningTokens is a subset, not an extra billable sum.
	usage := newUsageInfo(u.inputTokens, u.outputTokens+u.reasoningTokens, u.reasoningTokens, min(u.cachedTokens, u.inputTokens))
	usage.TotalTokens = max(usage.TotalTokens, u.totalTokens)
	return *usage
}

func geminiNativeUsageFromBody(body []byte) UsageInfo {
	var usage geminiNativeUsage
	usage.observe(body)
	return usage.usageInfo()
}

func geminiNativeHasContent(body []byte) bool {
	for _, candidate := range gjson.GetBytes(body, "candidates").Array() {
		for _, part := range candidate.Get("content.parts").Array() {
			if part.Get("text").String() != "" ||
				part.Get("functionCall.name").String() != "" ||
				part.Get("inlineData.data").String() != "" ||
				part.Get("executableCode.code").String() != "" ||
				part.Get("codeExecutionResult.output").String() != "" {
				return true
			}
		}
	}
	return false
}
