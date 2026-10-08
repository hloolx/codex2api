package proxy

import (
	"testing"

	"github.com/tidwall/gjson"

	"github.com/codex2api/database"
)

func TestUpstreamResponseModelObserverFirstAndTerminal(t *testing.T) {
	o := &upstreamResponseModelObserver{}
	// 流中段声明
	observeUpstreamResponseModelFrame(o, gjson.Parse(`{"type":"response.created","response":{"model":"gpt-5.2"}}`), "response.created")
	if got := o.Model(); got != "gpt-5.2" {
		t.Fatalf("first declaration model = %q, want gpt-5.2", got)
	}
	// 终态声明优先
	observeUpstreamResponseModelFrame(o, gjson.Parse(`{"type":"response.completed","response":{"model":"gpt-5.2-codex"}}`), "response.completed")
	if got := o.Model(); got != "gpt-5.2-codex" {
		t.Fatalf("terminal declaration model = %q, want gpt-5.2-codex", got)
	}
	// 两个不同的自报值 = conflict（中途换模型的信号），即使同属一个模型家族。
	if !o.Conflict() {
		t.Fatal("differing declarations should set conflict even within one model family")
	}

	// 全程一致的声明不应 conflict
	o = &upstreamResponseModelObserver{}
	observeUpstreamResponseModelFrame(o, gjson.Parse(`{"type":"response.created","response":{"model":"gpt-5.2"}}`), "response.created")
	observeUpstreamResponseModelFrame(o, gjson.Parse(`{"type":"response.in_progress","response":{"model":"gpt-5.2"}}`), "response.in_progress")
	observeUpstreamResponseModelFrame(o, gjson.Parse(`{"type":"response.completed","response":{"model":"GPT-5.2"}}`), "response.completed")
	if o.Conflict() {
		t.Fatal("consistent (case-insensitive) declarations should not conflict")
	}
	if got := o.Model(); got != "GPT-5.2" {
		t.Fatalf("model = %q, want GPT-5.2 (terminal wins)", got)
	}
}

func TestUpstreamResponseModelObserverConflict(t *testing.T) {
	o := &upstreamResponseModelObserver{}
	observeUpstreamResponseModelFrame(o, gjson.Parse(`{"type":"response.created","response":{"model":"gpt-5.2"}}`), "response.created")
	observeUpstreamResponseModelFrame(o, gjson.Parse(`{"type":"response.completed","response":{"model":"gpt-5.6-luna"}}`), "response.completed")
	if !o.Conflict() {
		t.Fatal("contradictory declarations should set conflict")
	}
	// 终态声明仍优先
	if got := o.Model(); got != "gpt-5.6-luna" {
		t.Fatalf("model = %q, want gpt-5.6-luna", got)
	}
}

func TestUpstreamModelMismatchTriState(t *testing.T) {
	if upstreamModelMismatch("gpt-5.2", "") != nil {
		t.Fatal("empty response model should be nil (upstream did not declare)")
	}
	if upstreamModelMismatch("", "gpt-5.2") == nil || *upstreamModelMismatch("", "gpt-5.2") != true {
		t.Fatal("empty sent model with declared response model should mismatch (true)")
	}
	if m := upstreamModelMismatch("GPT-5.2", "gpt-5.2"); m == nil || *m {
		t.Fatal("case-insensitive equal models should not mismatch")
	}
	if m := upstreamModelMismatch("gpt-5.2", "gpt-5.6-luna"); m == nil || !*m {
		t.Fatal("different models should mismatch")
	}
}

func TestApplyUpstreamResponseModelObservation(t *testing.T) {
	// 未自报：字段保持零值，不写日志输入
	o := &upstreamResponseModelObserver{}
	input := &database.UsageLogInput{Endpoint: "/v1/responses", Model: "gpt-5.2"}
	applyUpstreamResponseModelObservation(input, o, "gpt-5.2", 1)
	if input.UpstreamResponseModel != "" || input.UpstreamModelMismatch != nil {
		t.Fatal("no declaration should leave input untouched")
	}

	// 一致自报：mismatch=false
	o = &upstreamResponseModelObserver{}
	observeUpstreamResponseModelFrame(o, gjson.Parse(`{"type":"response.completed","response":{"model":"gpt-5.2"}}`), "response.completed")
	input = &database.UsageLogInput{Endpoint: "/v1/responses", Model: "gpt-5.2"}
	applyUpstreamResponseModelObservation(input, o, "gpt-5.2", 1)
	if input.UpstreamResponseModel != "gpt-5.2" || input.UpstreamModelMismatch == nil || *input.UpstreamModelMismatch {
		t.Fatalf("matching declaration: model=%q mismatch=%v", input.UpstreamResponseModel, input.UpstreamModelMismatch)
	}

	// 不一致自报：mismatch=true
	o = &upstreamResponseModelObserver{}
	observeUpstreamResponseModelFrame(o, gjson.Parse(`{"type":"response.completed","response":{"model":"gpt-5.6-luna"}}`), "response.completed")
	input = &database.UsageLogInput{Endpoint: "/v1/responses", Model: "gpt-5.2"}
	applyUpstreamResponseModelObservation(input, o, "gpt-5.2", 1)
	if input.UpstreamModelMismatch == nil || !*input.UpstreamModelMismatch {
		t.Fatal("mismatching declaration should record true")
	}

	// sentModel 为空（无映射、logModel 也有值时不会为空，但兜底空串仍要正确判 true）
	input = &database.UsageLogInput{Endpoint: "/v1/responses", Model: ""}
	applyUpstreamResponseModelObservation(input, o, "", 1)
	if input.UpstreamModelMismatch == nil || !*input.UpstreamModelMismatch {
		t.Fatal("empty sent model should mismatch=true when upstream declared a model")
	}
}

func TestObserveUpstreamResponseModelBody(t *testing.T) {
	o := &upstreamResponseModelObserver{}
	observeUpstreamResponseModelBody(o, []byte(`{"id":"resp_1","model":"gpt-5.2","usage":{"input_tokens":1}}`))
	if got := o.Model(); got != "gpt-5.2" {
		t.Fatalf("body model = %q, want gpt-5.2", got)
	}
	// 非 JSON / 空体安全
	observeUpstreamResponseModelBody(o, nil)
	if got := o.Model(); got != "gpt-5.2" {
		t.Fatalf("model changed after empty body: %q", got)
	}
}

func TestUpstreamSentModelForAudit(t *testing.T) {
	if got := upstreamSentModelForAudit("mapped-model", "client-model"); got != "mapped-model" {
		t.Fatalf("sent = %q, want mapped-model", got)
	}
	if got := upstreamSentModelForAudit("  ", "client-model"); got != "client-model" {
		t.Fatalf("sent = %q, want client-model", got)
	}
	if got := upstreamSentModelForAudit("", ""); got != "" {
		t.Fatalf("sent = %q, want empty", got)
	}
}
