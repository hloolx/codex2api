package database

import "strings"

// DaybreakProgram 只影响费率选择，不改变日志中的生效模型。
func daybreakBillingModel(model, program string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return model
	}
	switch program {
	case "daybreak_blue":
		return model + "-daybreak-blue"
	case "daybreak_red":
		return model + "-daybreak-red"
	default:
		return model
	}
}

// Daybreak 专属价格优先于基础模型，同步价不能覆盖管理员的手工价格。
func daybreakAliasPricing(baseModel, alias string) *ModelPricing {
	pricing := *GetModelPricing(baseModel)
	override, ok := lookupModelPricingOverride(alias)
	if !ok {
		return &pricing
	}
	if override.Source != ModelPricingSourceCustom {
		baseKey := PricingManagementModelKey(baseModel)
		canonicalKey := CanonicalBillingModelKey(baseModel)
		if ModelPricingSourceFor(baseKey) == ModelPricingSourceCustom ||
			ModelPricingSourceFor(canonicalKey) == ModelPricingSourceCustom {
			return &pricing
		}
	}
	override.applyNonZero(&pricing)
	return &pricing
}
