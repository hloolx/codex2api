package database

import "testing"

func withPricingOverrides(t *testing.T, overrides map[string]ModelPricingOverride) {
	t.Helper()
	previous := currentModelPricingOverrides()
	t.Cleanup(func() { SetModelPricingOverrides(previous) })
	SetModelPricingOverrides(overrides)
}

func TestGrokMediaModelsNeverFallBackToTokenPricing(t *testing.T) {
	withPricingOverrides(t, nil)
	cases := map[string]struct {
		unit string
		cost float64
	}{
		"grok-imagine-image":             {MediaUnitImage, 0.02},
		"grok-imagine-image-quality":     {MediaUnitImage, 0.05},
		"grok-imagine":                   {MediaUnitImage, 0.05},
		"grok-imagine-video":             {MediaUnitSecond, 0.05},
		"grok-imagine-video-1.5":         {MediaUnitSecond, 0.08},
		"grok-imagine-video-1.5-preview": {MediaUnitSecond, 0.08},
		"grok-imagine-image-pro":         {MediaUnitImage, 0},
	}
	for model, want := range cases {
		p := GetModelPricing(model)
		if MediaBillingUnit(model) != want.unit || !approxEqual(p.MediaUnitCost, want.cost) {
			t.Fatalf("%s: unit=%q cost=%v, want %q %v", model, MediaBillingUnit(model), p.MediaUnitCost, want.unit, want.cost)
		}
		if p.InputPricePerMToken != 0 || p.OutputPricePerMToken != 0 {
			t.Fatalf("%s inherited token fallback pricing: %+v", model, p)
		}
	}
	if CanonicalBillingModelKey("grok-imagine-video-1.5-preview") != "grok-imagine-video-1.5" {
		t.Fatal("preview video alias must share the canonical pricing row")
	}
	if MediaBillingUnit("grok-4.7") != "" || MediaBillingUnit("gpt-image-2") != "" {
		t.Fatal("non-media models must not be classified as media")
	}
}

func TestGrokMediaAccountCostUsesUnits(t *testing.T) {
	withPricingOverrides(t, nil)
	image := &UsageLogInput{Model: "grok-imagine-image", StatusCode: 200, ImageCount: 26}
	if got := UsageLogBilledCost(image); !approxEqual(got, 0.52) {
		t.Fatalf("26 images cost = %v, want 0.52", got)
	}
	video := &UsageLogInput{Model: "grok-imagine-video-1.5-preview", StatusCode: 200, VideoSeconds: 10, VideoCount: 1}
	if got := UsageLogBilledCost(video); !approxEqual(got, 0.8) {
		t.Fatalf("10s video cost = %v, want 0.8", got)
	}
	submit := &UsageLogInput{Model: "grok-imagine-video", StatusCode: 200}
	if got := UsageLogBilledCost(submit); got != 0 {
		t.Fatalf("video submission must not be billed, got %v", got)
	}
	failed := &UsageLogInput{Model: "grok-imagine-image", StatusCode: 502, ImageCount: 1}
	if got := UsageLogBilledCost(failed); got != 0 {
		t.Fatalf("failed image request billed %v", got)
	}
}

func TestGrokMediaUpstreamReportedCostPrecedence(t *testing.T) {
	withPricingOverrides(t, nil)
	log := &UsageLogInput{Model: "grok-imagine-video", StatusCode: 200, VideoSeconds: 8, UpstreamCostUSD: 0.5}
	if got := UsageLogBilledCost(log); !approxEqual(got, 0.5) {
		t.Fatalf("upstream-reported cost should win over the built-in rate: %v", got)
	}
	withPricingOverrides(t, map[string]ModelPricingOverride{
		"grok-imagine-video": {Source: ModelPricingSourceCustom, MediaUnitCost: 0.1},
	})
	if got := UsageLogBilledCost(log); !approxEqual(got, 0.8) {
		t.Fatalf("custom unit cost should win over upstream-reported cost: %v", got)
	}
	withPricingOverrides(t, map[string]ModelPricingOverride{
		"grok-imagine-video": {Source: ModelPricingSourceSynced, MediaUnitCost: 0.1},
	})
	if got := UsageLogBilledCost(log); !approxEqual(got, 0.5) {
		t.Fatalf("synced unit cost must not override upstream-reported cost: %v", got)
	}
}

func TestGrokVideoUserBillingModes(t *testing.T) {
	withPricingOverrides(t, map[string]ModelPricingOverride{
		"grok-imagine-video-1.5": {Source: ModelPricingSourceCustom, UserBillingMode: UserBillingModePerSecond, ImageUnitPrice: 0.1},
		"grok-imagine-video":     {Source: ModelPricingSourceCustom, UserBillingMode: UserBillingModePerVideo, ImageUnitPrice: 0.3},
	})
	perSecond := &UsageLogInput{Model: "grok-imagine-video-1.5", StatusCode: 200, VideoSeconds: 10, VideoCount: 1}
	if got := UsageLogUserBilledCost(perSecond); !approxEqual(got, 1.0) {
		t.Fatalf("per_second user cost = %v, want 1.0", got)
	}
	if details := SnapshotUsageLogBilling(perSecond).UserBillingDetails(); details.UserBillingMode != UserBillingModePerSecond || details.BilledImageCount != 10 {
		t.Fatalf("per_second snapshot = %+v", details)
	}
	moderated := &UsageLogInput{Model: "grok-imagine-video-1.5", StatusCode: 200, VideoSeconds: 10, ErrorMessage: "withheld"}
	if got := UsageLogUserBilledCost(moderated); got != 0 {
		t.Fatalf("undelivered video billed to user: %v", got)
	}
	if got := UsageLogBilledCost(moderated); !approxEqual(got, 0.8) {
		t.Fatalf("upstream still charges generated seconds, got %v", got)
	}
	perVideo := &UsageLogInput{Model: "grok-imagine-video", StatusCode: 200, VideoSeconds: 8, VideoCount: 1}
	if got := UsageLogUserBilledCost(perVideo); !approxEqual(got, 0.3) {
		t.Fatalf("per_video user cost = %v, want 0.3", got)
	}
	tokenMode := &UsageLogInput{Model: "grok-imagine-image", StatusCode: 200, ImageCount: 3}
	if got := UsageLogUserBilledCost(tokenMode); !approxEqual(got, 0.06) {
		t.Fatalf("default user cost should follow upstream cost, got %v", got)
	}
}

func TestValidateMediaUserBilling(t *testing.T) {
	cases := []struct {
		model string
		o     ModelPricingOverride
		ok    bool
	}{
		{"grok-imagine-video", ModelPricingOverride{UserBillingMode: UserBillingModePerSecond, ImageUnitPrice: 0.1}, true},
		{"grok-imagine-video-1.5-preview", ModelPricingOverride{UserBillingMode: UserBillingModePerVideo, ImageUnitPrice: 0.5}, true},
		{"grok-imagine-image", ModelPricingOverride{UserBillingMode: UserBillingModePerImage, ImageUnitPrice: 0.03, MediaUnitCost: 0.02}, true},
		{"grok-imagine-image", ModelPricingOverride{UserBillingMode: UserBillingModePerSecond, ImageUnitPrice: 0.1}, false},
		{"grok-imagine-video", ModelPricingOverride{UserBillingMode: UserBillingModePerImage, ImageUnitPrice: 0.1}, false},
		{"grok-imagine-video", ModelPricingOverride{UserBillingMode: UserBillingModePerVideo}, false},
		{"grok-4.7", ModelPricingOverride{MediaUnitCost: 0.1}, false},
		{"grok-imagine-video", ModelPricingOverride{UserBillingMode: "per_minute", ImageUnitPrice: 1}, false},
	}
	for _, tc := range cases {
		err := ValidateModelUserBilling(tc.model, tc.o)
		if (err == nil) != tc.ok {
			t.Fatalf("%s %+v: err=%v, want ok=%v", tc.model, tc.o, err, tc.ok)
		}
	}
}

func TestUsageLogDisplayForMediaRows(t *testing.T) {
	l := &UsageLog{Model: "grok-imagine-video", AccountBilled: 0.4, UserBilled: 0.4, VideoSeconds: 8}
	l.populateBillingBreakdown()
	if !approxEqual(l.TotalCost, 0.4) || l.InputCost != 0 || l.OutputCost != 0 {
		t.Fatalf("media display cost = %+v", l)
	}
}
