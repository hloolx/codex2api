package database

import (
	"fmt"
	"math"
	"strings"
)

const (
	UserBillingModeToken    = "token"
	UserBillingModePerImage = "per_image"
)

// SupportsImageBilling deliberately excludes text models with inline image tools.
// Those requests retain their existing token billing policy.
func SupportsImageBilling(model string) bool {
	model = normalizeBillingModelName(model)
	return strings.HasPrefix(model, "gpt-image-") || model == "grok-2-image" ||
		model == "grok-2-image-1212" || model == "grok-imagine-image" || model == "grok-imagine-image-pro" || model == "grok-imagine-image-quality"
}

func ValidateModelUserBilling(model string, o ModelPricingOverride) error {
	switch o.UserBillingMode {
	case "", UserBillingModeToken, UserBillingModePerImage, UserBillingModePerVideo, UserBillingModePerSecond:
	default:
		return fmt.Errorf("user_billing_mode must be token, per_image, per_video or per_second")
	}
	if math.IsNaN(o.ImageUnitPrice) || math.IsInf(o.ImageUnitPrice, 0) || o.ImageUnitPrice < 0 {
		return fmt.Errorf("image_unit_price must be a finite non-negative USD amount")
	}
	if math.IsNaN(o.MediaUnitCost) || math.IsInf(o.MediaUnitCost, 0) || o.MediaUnitCost < 0 {
		return fmt.Errorf("media_unit_cost must be a finite non-negative USD amount")
	}
	if o.MediaUnitCost > 0 && MediaBillingUnit(model) == "" {
		return fmt.Errorf("media_unit_cost requires a Grok Imagine media model")
	}
	switch o.UserBillingMode {
	case UserBillingModePerImage:
		if !SupportsImageBilling(model) {
			return fmt.Errorf("per_image billing requires an image model")
		}
	case UserBillingModePerVideo, UserBillingModePerSecond:
		if MediaBillingUnit(model) != MediaUnitSecond {
			return fmt.Errorf("%s billing requires a video model", o.UserBillingMode)
		}
	}
	if IsUnitUserBillingMode(o.UserBillingMode) && o.ImageUnitPrice <= 0 {
		return fmt.Errorf("image_unit_price must be greater than zero for %s billing", o.UserBillingMode)
	}
	return nil
}

// UserBilling records the policy and successful billed units at settlement time.
// Empty mode identifies legacy rows; an explicit unit-mode zero is never replaced
// with upstream cost when displaying failed/undelivered outputs.
// ImageUnitPrice / BilledImageCount 沿用历史列名,语义是"单位价 / 计费单位数":
// per_image 为张,per_video 为次,per_second 为秒。
type UserBilling struct {
	UserBillingMode  string  `json:"user_billing_mode"`
	ImageUnitPrice   float64 `json:"image_unit_price"`
	BilledImageCount int     `json:"billed_image_count"`
}

type usageBillingSnapshot struct {
	UserBilling
	accountCost float64
}

// SnapshotUsageLogBilling freezes image pricing before scope counters and the
// buffered database writer observe an event. It does not mutate the caller's log.
func SnapshotUsageLogBilling(input *UsageLogInput) *UsageLogInput {
	if input == nil || input.billingSnapshot != nil {
		return input
	}
	model := input.EffectiveModel
	if model == "" {
		model = input.Model
	}
	mediaUnit := MediaBillingUnit(model)
	if !SupportsImageBilling(model) && mediaUnit == "" {
		return input
	}
	p := GetModelPricing(model)
	mode := UserBillingModeToken
	if IsUnitUserBillingMode(p.UserBillingMode) && p.ImageUnitPrice > 0 {
		switch {
		case p.UserBillingMode == UserBillingModePerImage && SupportsImageBilling(model):
			mode = p.UserBillingMode
		case p.UserBillingMode != UserBillingModePerImage && mediaUnit == MediaUnitSecond:
			mode = p.UserBillingMode
		}
	}
	snapshot := &usageBillingSnapshot{UserBilling: UserBilling{UserBillingMode: mode}, accountCost: UsageLogBilledCost(input)}
	if mode != UserBillingModeToken {
		snapshot.ImageUnitPrice = p.ImageUnitPrice
		snapshot.BilledImageCount = userBillingUnits(input, mode)
	}
	copy := *input
	copy.billingSnapshot = snapshot
	return &copy
}

func (input *UsageLogInput) UserBillingDetails() UserBilling {
	if input != nil && input.billingSnapshot != nil {
		return input.billingSnapshot.UserBilling
	}
	return UserBilling{}
}

// WithDeliveredImageCount caps fees after Studio has persisted the outputs.
// Upstream usage/cost remain intact even if local storage fails.
func WithDeliveredImageCount(input *UsageLogInput, delivered int) *UsageLogInput {
	input = SnapshotUsageLogBilling(input)
	if input == nil || input.UserBillingDetails().UserBillingMode != UserBillingModePerImage {
		return input
	}
	copy, snapshot := *input, *input.billingSnapshot
	snapshot.BilledImageCount = min(snapshot.BilledImageCount, max(0, delivered))
	copy.billingSnapshot = &snapshot
	return &copy
}

func UsageLogUserBilledCost(input *UsageLogInput) float64 {
	input = SnapshotUsageLogBilling(input)
	if input == nil {
		return 0
	}
	if s := input.billingSnapshot; s != nil {
		if IsUnitUserBillingMode(s.UserBillingMode) {
			return float64(s.BilledImageCount) * s.ImageUnitPrice
		}
		return s.accountCost
	}
	return UsageLogBilledCost(input)
}
