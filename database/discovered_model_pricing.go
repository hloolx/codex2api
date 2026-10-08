package database

import (
	"regexp"
	"strings"
)

var (
	versionedGPTPricingID = regexp.MustCompile(`^gpt-\d+(?:\.\d+)?(?:-|$)`)
	gptPricingSnapshot    = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// discoveredGPTPricingKey 为尚未内置的 GPT 型号保留独立价格键。
// 已知型号的快照、推理档位别名继续复用原有价格规则。
func discoveredGPTPricingKey(model string) string {
	model = normalizeBillingModelName(model)
	model = strings.TrimSuffix(model, "-openai-compact")
	if !versionedGPTPricingID.MatchString(model) {
		return ""
	}
	if index := strings.IndexByte(model, '('); index >= 0 {
		model = model[:index]
	}
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		model = strings.TrimSuffix(model, "-"+effort)
	}
	for _, rule := range modelPricingRules {
		if model == rule.model {
			return ""
		}
		if suffix, ok := strings.CutPrefix(model, rule.model+"-"); ok && gptPricingSnapshot.MatchString(suffix) {
			return ""
		}
	}
	return model
}
