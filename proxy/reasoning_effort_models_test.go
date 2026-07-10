package proxy

import "testing"

// 设置里的思考强度别名只把 max 放行给已确认支持的三个 5.6 模型；
// 旧模型维持 max->xhigh 的兼容钳位。
func TestParseReasoningEffortModelEntries_MaxGatedByModel(t *testing.T) {
	supported := []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.4"}
	entries, err := parseReasoningEffortModelEntries(
		`[{"model":"gpt-5.6-sol","effort":"max"},{"model":"gpt-5.6-terra","effort":"max"},{"model":"gpt-5.6-luna","effort":"max"},{"model":"gpt-5.4","effort":"max"}]`,
		supported, true)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("entries = %d, want 4: %+v", len(entries), entries)
	}
	for i := 0; i < 3; i++ {
		if entries[i].Effort != "max" {
			t.Fatalf("%s effort = %q, want max", entries[i].Model, entries[i].Effort)
		}
	}
	if entries[3].Effort != "xhigh" {
		t.Fatalf("gpt-5.4 effort = %q, want xhigh", entries[3].Effort)
	}
}

func TestNormalizeReasoningEffortModelsJSONRejectsUltraAsServerEffort(t *testing.T) {
	_, err := NormalizeReasoningEffortModelsJSON(
		`[{"model":"gpt-5.6-sol","effort":"ultra"}]`,
		[]string{"gpt-5.6-sol"},
	)
	if err == nil {
		t.Fatal("expected ultra server-side effort alias to be rejected")
	}
}
