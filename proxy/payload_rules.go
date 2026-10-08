package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== Payload 请求体重写规则 ====================
//
// 声明式规则引擎：管理员配置 JSON 规则，在请求体发往上游前按条件改写字段。
// 典型用途：覆盖/追加 instructions（系统提示词）、覆盖 service_tier、
// 按条件改写 reasoning.effort（如 medium → high）。
//
// 规则 JSON 结构（存于 system_settings.payload_rules）：
//
//	{
//	  "default":      [ {rule} ],  // 字段缺失时才写入（first-write-wins）
//	  "default_raw":  [ {rule} ],  // 同上，值为原始 JSON 字符串
//	  "override":     [ {rule} ],  // 无条件覆盖（last-write-wins）
//	  "override_raw": [ {rule} ],  // 同上，值为原始 JSON 字符串
//	  "append":       [ {rule} ],  // 字符串字段追加（原值 + "\n\n" + 配置值）
//	  "filter":       [ {rule} ]   // 删除字段（params 为路径数组）
//	}
//
// 每条 rule 的匹配门（全部满足才应用；同一门内多值为 OR，门与门之间为 AND）：
//
//	{
//	  "models":        ["gpt-*"],                    // 模型名通配，空=全部
//	  "headers":       {"X-Client": "codex*"},       // 入站请求头通配
//	  "api_key_ids":   ["12", "3*"],                 // 下游 API Key ID（字符串化）通配
//	  "api_key_names": ["fast*"],                    // 下游 API Key 名称通配
//	  "group_ids":     ["5"],                        // 该 Key 允许的账号组 ID（字符串化）通配
//	  "group_names":   ["fast*"],                    // 该 Key 允许的账号组名通配
//	  "account_group_ids":   ["5"],                  // 本次实际调度账号所属组 ID 通配
//	  "account_group_names": ["plus*"],              // 本次实际调度账号所属组名通配
//	  "account_plans":       ["plus"],               // 本次实际调度账号套餐通配
//	  "match":         {"reasoning.effort": "medium"}, // JSON 路径等值
//	  "not_match":     {"metadata.mode": "dev"},     // JSON 路径不等值
//	  "exist":         ["tools.0"],                  // 路径必须存在
//	  "not_exist":     ["metadata.skip"],            // 路径必须不存在
//	  "params":        {...}                         // 动作参数
//	}
//
// api_key_* / group_* 依赖请求身份（PayloadRuleIdentity）：无身份的内部/未鉴权路径下，
// 凡配置了任一 key/组门的规则一律不匹配（fail-closed）。compact/relay 端点不套用本引擎。
//
// 注意 group_* 与 account_group_* 的语义区别（issue #410）：
//   - group_*         匹配该 Key **允许使用**的账号组，与本次实际调度到哪个账号无关；
//   - account_group_* / account_plans 匹配本次 attempt **实际选中账号**的组/套餐。
//     重试换号后按新账号重新匹配。典型用途：pro Key 兜底到 plus 组账号时才覆写
//     service_tier=fast，路由回 pro 账号则不覆写。
//     账号身份未解析的路径（识别 AccountResolved）下，带 account_* 门的规则同样 fail-closed。
//
// 应用顺序：default → default_raw → override → override_raw → append → filter。

// payloadProtectedRoots 是禁止改写的字段根：网关的调度、计费、会话隔离与
// WS 帧协议依赖这些字段，规则应用又发生在选号之后，改写会造成状态错位。
// 模型改写请使用现成的模型映射功能。
var payloadProtectedRoots = map[string]struct{}{
	"model":            {},
	"stream":           {},
	"input":            {},
	"prompt_cache_key": {},
	"store":            {},
	"type":             {},
}

// PayloadRule 单条重写规则：匹配门 + 动作参数。
type PayloadRule struct {
	Models  []string          `json:"models,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// 身份门：均为通配列表，命中任一即通过；空=该维度不限。id 门按字符串化后的值通配。
	// 依赖 PayloadRuleIdentity；无身份时凡配置了任一身份门的规则一律不匹配（fail-closed）。
	APIKeyIDs   []string `json:"api_key_ids,omitempty"`
	APIKeyNames []string `json:"api_key_names,omitempty"`
	GroupIDs    []string `json:"group_ids,omitempty"`
	GroupNames  []string `json:"group_names,omitempty"`
	// 账号门：匹配本次实际调度选中账号的组/套餐（区别于上面按 Key 允许组匹配的
	// group_*，见文件头注释）。账号身份未解析时 fail-closed 不匹配。
	AccountGroupIDs   []string       `json:"account_group_ids,omitempty"`
	AccountGroupNames []string       `json:"account_group_names,omitempty"`
	AccountPlans      []string       `json:"account_plans,omitempty"`
	Match             map[string]any `json:"match,omitempty"`
	NotMatch          map[string]any `json:"not_match,omitempty"`
	Exist             []string       `json:"exist,omitempty"`
	NotExist          []string       `json:"not_exist,omitempty"`
	// Params 动作参数：default/override/append 为 路径→值 map，
	// default_raw/override_raw 值须为合法 JSON 字符串，filter 为路径数组。
	Params json.RawMessage `json:"params"`
}

// PayloadRuleIdentity 承载请求的下游身份，供 api_key_* / group_* / account_* 匹配门
// 使用。由 handler 从鉴权 context 构造后经上游 context 传入（见 WithPayloadRuleIdentity）。
// Account* 字段按 attempt 由 WithSelectedAccount 派生副本填充——重试换号后账号维度
// 会变化，不能在共享实例上原地改。
type PayloadRuleIdentity struct {
	APIKeyID   int64
	APIKeyName string
	GroupIDs   []int64  // 该 Key 允许的账号组 ID（Key.AllowedGroupIDs）
	GroupNames []string // 上述组 ID 解析出的组名

	// 本次 attempt 实际调度选中账号的维度（issue #410）。
	AccountResolved   bool // 账号信息是否已填充；false 时带 account_* 门的规则 fail-closed
	AccountGroupIDs   []int64
	AccountGroupNames []string
	AccountPlan       string
}

// WithSelectedAccount 返回填充了实际调度账号维度的身份副本。原身份跨 attempt 共享，
// 重试换号后账号信息不同，必须派生副本而非原地修改。id 为 nil（无鉴权身份）时保持
// nil——与现有身份门的 fail-closed 语义一致。
func (id *PayloadRuleIdentity) WithSelectedAccount(account *auth.Account, store *auth.Store) *PayloadRuleIdentity {
	if id == nil || account == nil {
		return id
	}
	derived := *id
	derived.AccountResolved = true
	derived.AccountGroupIDs = account.GroupIDSnapshot()
	if store != nil {
		derived.AccountGroupNames = store.ResolveGroupNames(derived.AccountGroupIDs)
	}
	derived.AccountPlan = account.GetPlanType()
	return &derived
}

type payloadRuleIdentityKey struct{}

// WithPayloadRuleIdentity 把身份挂到 context 上（id 为 nil 时原样返回）。
// 必须挂在真正流入 ExecuteRequest 的上游 context 上——client 请求 context 与上游
// context 解耦（newDrainableUpstreamContext 从 background 派生），挂错则传不进引擎。
func WithPayloadRuleIdentity(ctx context.Context, id *PayloadRuleIdentity) context.Context {
	if ctx == nil || id == nil {
		return ctx
	}
	return context.WithValue(ctx, payloadRuleIdentityKey{}, id)
}

// PayloadRuleIdentityFromContext 取出身份，缺失返回 nil。
func PayloadRuleIdentityFromContext(ctx context.Context) *PayloadRuleIdentity {
	if ctx == nil {
		return nil
	}
	id, _ := ctx.Value(payloadRuleIdentityKey{}).(*PayloadRuleIdentity)
	return id
}

// PayloadRuleSet 全部规则组。
type PayloadRuleSet struct {
	Default     []PayloadRule `json:"default,omitempty"`
	DefaultRaw  []PayloadRule `json:"default_raw,omitempty"`
	Override    []PayloadRule `json:"override,omitempty"`
	OverrideRaw []PayloadRule `json:"override_raw,omitempty"`
	Append      []PayloadRule `json:"append,omitempty"`
	Filter      []PayloadRule `json:"filter,omitempty"`
}

// IsEmpty 报告规则集是否没有任何规则。
func (rs *PayloadRuleSet) IsEmpty() bool {
	if rs == nil {
		return true
	}
	return len(rs.Default) == 0 && len(rs.DefaultRaw) == 0 && len(rs.Override) == 0 &&
		len(rs.OverrideRaw) == 0 && len(rs.Append) == 0 && len(rs.Filter) == 0
}

var currentPayloadRuleSet atomic.Value // stores *PayloadRuleSet

// CurrentPayloadRules 返回当前生效的规则集（可能为 nil）。
func CurrentPayloadRules() *PayloadRuleSet {
	if v, ok := currentPayloadRuleSet.Load().(*PayloadRuleSet); ok {
		return v
	}
	return nil
}

// SetPayloadRulesJSON 解析并热更规则集；传空串/空对象清空规则。
// 解析或校验失败时保持现有规则不变并返回错误。
func SetPayloadRulesJSON(raw string) error {
	rs, err := ParsePayloadRulesJSON(raw)
	if err != nil {
		return err
	}
	currentPayloadRuleSet.Store(rs)
	return nil
}

// ParsePayloadRulesJSON 解析规则 JSON 并做结构与保护字段校验。
func ParsePayloadRulesJSON(raw string) (*PayloadRuleSet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" || raw == "null" {
		return &PayloadRuleSet{}, nil
	}
	var rs PayloadRuleSet
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rs); err != nil {
		return nil, fmt.Errorf("payload_rules JSON 解析失败: %w", err)
	}
	groups := []struct {
		name  string
		rules []PayloadRule
		kind  string // map / raw / filter / append
	}{
		{"default", rs.Default, "map"},
		{"default_raw", rs.DefaultRaw, "raw"},
		{"override", rs.Override, "map"},
		{"override_raw", rs.OverrideRaw, "raw"},
		{"append", rs.Append, "append"},
		{"filter", rs.Filter, "filter"},
	}
	for _, g := range groups {
		for i := range g.rules {
			if err := validatePayloadRuleParams(&g.rules[i], g.kind); err != nil {
				return nil, fmt.Errorf("%s[%d]: %w", g.name, i, err)
			}
		}
	}
	return &rs, nil
}

// NormalizePayloadRulesJSON 校验并返回紧凑化的规则 JSON（供设置保存使用）。
func NormalizePayloadRulesJSON(raw string) (string, error) {
	rs, err := ParsePayloadRulesJSON(raw)
	if err != nil {
		return "", err
	}
	if rs.IsEmpty() {
		return "{}", nil
	}
	normalized, err := json.Marshal(rs)
	if err != nil {
		return "", err
	}
	return string(normalized), nil
}

func validatePayloadRuleParams(rule *PayloadRule, kind string) error {
	if len(rule.Params) == 0 {
		return fmt.Errorf("params 不能为空")
	}
	checkPath := func(path string) error {
		path = strings.TrimSpace(path)
		if path == "" {
			return fmt.Errorf("params 路径不能为空")
		}
		if isProtectedPayloadPath(path) {
			return fmt.Errorf("字段 %q 受保护，不允许改写（模型改写请使用模型映射功能）", path)
		}
		return nil
	}
	switch kind {
	case "filter":
		var paths []string
		if err := json.Unmarshal(rule.Params, &paths); err != nil {
			return fmt.Errorf("filter 的 params 须为路径数组: %w", err)
		}
		if len(paths) == 0 {
			return fmt.Errorf("params 不能为空")
		}
		for _, p := range paths {
			if err := checkPath(p); err != nil {
				return err
			}
		}
	case "raw":
		var params map[string]string
		if err := json.Unmarshal(rule.Params, &params); err != nil {
			return fmt.Errorf("raw 规则的 params 须为 路径→JSON字符串 映射: %w", err)
		}
		if len(params) == 0 {
			return fmt.Errorf("params 不能为空")
		}
		for p, v := range params {
			if err := checkPath(p); err != nil {
				return err
			}
			if !json.Valid([]byte(v)) {
				return fmt.Errorf("字段 %q 的值不是合法 JSON: %q", p, v)
			}
		}
	case "append":
		var params map[string]string
		if err := json.Unmarshal(rule.Params, &params); err != nil {
			return fmt.Errorf("append 规则的 params 须为 路径→字符串 映射: %w", err)
		}
		if len(params) == 0 {
			return fmt.Errorf("params 不能为空")
		}
		for p := range params {
			if err := checkPath(p); err != nil {
				return err
			}
		}
	default: // map
		var params map[string]any
		if err := json.Unmarshal(rule.Params, &params); err != nil {
			return fmt.Errorf("params 须为 路径→值 映射: %w", err)
		}
		if len(params) == 0 {
			return fmt.Errorf("params 不能为空")
		}
		for p := range params {
			if err := checkPath(p); err != nil {
				return err
			}
		}
	}
	return nil
}

// isProtectedPayloadPath 判断路径根段是否命中保护清单。
func isProtectedPayloadPath(path string) bool {
	root := path
	if idx := strings.IndexAny(path, ".|#("); idx >= 0 {
		root = path[:idx]
	}
	_, protected := payloadProtectedRoots[strings.TrimSpace(root)]
	return protected
}

// matchPayloadWildcard 大小写不敏感的 * 通配匹配。
func matchPayloadWildcard(pattern, value string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	value = strings.ToLower(strings.TrimSpace(value))
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	segments := strings.Split(pattern, "*")
	if len(segments) == 1 {
		return pattern == value
	}
	if !strings.HasPrefix(value, segments[0]) {
		return false
	}
	rest := value[len(segments[0]):]
	for _, seg := range segments[1 : len(segments)-1] {
		if seg == "" {
			continue
		}
		idx := strings.Index(rest, seg)
		if idx < 0 {
			return false
		}
		rest = rest[idx+len(seg):]
	}
	last := segments[len(segments)-1]
	return last == "" || strings.HasSuffix(rest, last)
}

// anyWildcard 报告 value 是否命中 patterns 中任一通配（patterns 为空视为不设门，返回 true）。
func anyWildcard(patterns []string, value string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if matchPayloadWildcard(pattern, value) {
			return true
		}
	}
	return false
}

// anyWildcardMulti 报告 values 中是否存在某个命中 patterns 中任一通配（patterns 为空返回 true）。
func anyWildcardMulti(patterns, values []string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, value := range values {
		if anyWildcard(patterns, value) {
			return true
		}
	}
	return false
}

// formatInt64IDs 把 ID 列表字符串化，供 id 门通配匹配。
func formatInt64IDs(ids []int64) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.FormatInt(id, 10)
	}
	return out
}

// payloadRuleMatches 判断规则的全部匹配门是否满足。identity 为请求身份（可为 nil）。
func payloadRuleMatches(rule *PayloadRule, body []byte, model string, headers http.Header, identity *PayloadRuleIdentity) bool {
	if !anyWildcard(rule.Models, model) {
		return false
	}
	for name, pattern := range rule.Headers {
		if headers == nil || !matchPayloadWildcard(pattern, headers.Get(name)) {
			return false
		}
	}
	// 身份门：任一门有配置但请求无身份 → fail-closed，不匹配。
	if len(rule.APIKeyIDs) > 0 || len(rule.APIKeyNames) > 0 || len(rule.GroupIDs) > 0 || len(rule.GroupNames) > 0 {
		if identity == nil {
			return false
		}
		if len(rule.APIKeyIDs) > 0 && !anyWildcard(rule.APIKeyIDs, strconv.FormatInt(identity.APIKeyID, 10)) {
			return false
		}
		if len(rule.APIKeyNames) > 0 && !anyWildcard(rule.APIKeyNames, identity.APIKeyName) {
			return false
		}
		if len(rule.GroupIDs) > 0 {
			if !anyWildcardMulti(rule.GroupIDs, formatInt64IDs(identity.GroupIDs)) {
				return false
			}
		}
		if len(rule.GroupNames) > 0 && !anyWildcardMulti(rule.GroupNames, identity.GroupNames) {
			return false
		}
	}
	// 账号门：匹配本次实际调度选中账号的组/套餐。账号身份未解析（identity 为 nil
	// 或 AccountResolved=false）时同样 fail-closed，不匹配。
	if len(rule.AccountGroupIDs) > 0 || len(rule.AccountGroupNames) > 0 || len(rule.AccountPlans) > 0 {
		if identity == nil || !identity.AccountResolved {
			return false
		}
		if len(rule.AccountGroupIDs) > 0 {
			if !anyWildcardMulti(rule.AccountGroupIDs, formatInt64IDs(identity.AccountGroupIDs)) {
				return false
			}
		}
		if len(rule.AccountGroupNames) > 0 && !anyWildcardMulti(rule.AccountGroupNames, identity.AccountGroupNames) {
			return false
		}
		if len(rule.AccountPlans) > 0 && !anyWildcard(rule.AccountPlans, identity.AccountPlan) {
			return false
		}
	}
	for path, want := range rule.Match {
		if !payloadValueEquals(gjson.GetBytes(body, path), want) {
			return false
		}
	}
	for path, want := range rule.NotMatch {
		if payloadValueEquals(gjson.GetBytes(body, path), want) {
			return false
		}
	}
	for _, path := range rule.Exist {
		result := gjson.GetBytes(body, path)
		if !result.Exists() || result.Type == gjson.Null {
			return false
		}
	}
	for _, path := range rule.NotExist {
		result := gjson.GetBytes(body, path)
		if result.Exists() && result.Type != gjson.Null {
			return false
		}
	}
	return true
}

// payloadValueEquals 比较 gjson 结果与配置值（两侧都来自 JSON，类型天然对齐）。
func payloadValueEquals(result gjson.Result, want any) bool {
	if !result.Exists() {
		return want == nil && result.Type == gjson.Null
	}
	return reflect.DeepEqual(result.Value(), want)
}

// ApplyPayloadRulesToBody 按当前规则集改写请求体。model 为映射后的生效模型，
// headers 为入站请求头，identity 为请求身份（可为 nil）。
// 改写失败的单条动作跳过，不影响其余规则。
func ApplyPayloadRulesToBody(body []byte, model string, headers http.Header, identity *PayloadRuleIdentity) []byte {
	rs := CurrentPayloadRules()
	if rs.IsEmpty() || len(body) == 0 || !gjson.ValidBytes(body) {
		return body
	}
	out := body

	applyMapRules := func(rules []PayloadRule, onlyIfMissing bool) {
		for i := range rules {
			rule := &rules[i]
			if !payloadRuleMatches(rule, out, model, headers, identity) {
				continue
			}
			var params map[string]any
			if json.Unmarshal(rule.Params, &params) != nil {
				continue
			}
			for path, value := range params {
				if isProtectedPayloadPath(path) {
					continue
				}
				if onlyIfMissing && gjson.GetBytes(out, path).Exists() {
					continue
				}
				if updated, err := sjson.SetBytes(out, path, value); err == nil {
					out = updated
				}
			}
		}
	}
	applyRawRules := func(rules []PayloadRule, onlyIfMissing bool) {
		for i := range rules {
			rule := &rules[i]
			if !payloadRuleMatches(rule, out, model, headers, identity) {
				continue
			}
			var params map[string]string
			if json.Unmarshal(rule.Params, &params) != nil {
				continue
			}
			for path, value := range params {
				if isProtectedPayloadPath(path) || !json.Valid([]byte(value)) {
					continue
				}
				if onlyIfMissing && gjson.GetBytes(out, path).Exists() {
					continue
				}
				if updated, err := sjson.SetRawBytes(out, path, []byte(value)); err == nil {
					out = updated
				}
			}
		}
	}

	applyMapRules(rs.Default, true)
	applyRawRules(rs.DefaultRaw, true)
	applyMapRules(rs.Override, false)
	applyRawRules(rs.OverrideRaw, false)

	for i := range rs.Append {
		rule := &rs.Append[i]
		if !payloadRuleMatches(rule, out, model, headers, identity) {
			continue
		}
		var params map[string]string
		if json.Unmarshal(rule.Params, &params) != nil {
			continue
		}
		for path, text := range params {
			if isProtectedPayloadPath(path) || text == "" {
				continue
			}
			existing := gjson.GetBytes(out, path)
			// 只对缺失或字符串字段追加；非字符串字段跳过，避免破坏结构。
			if existing.Exists() && existing.Type != gjson.String {
				continue
			}
			value := text
			if prev := existing.String(); strings.TrimSpace(prev) != "" {
				// 幂等保护：重试/续想等路径可能带着已改写的 body 重入规则引擎，
				// 已作为完整追加块存在的文本不再重复拼接，否则请求体随重入次数
				// 膨胀且改变提示词语义。
				if payloadAppendAlreadyApplied(prev, text) {
					continue
				}
				value = prev + "\n\n" + text
			}
			if updated, err := sjson.SetBytes(out, path, value); err == nil {
				out = updated
			}
		}
	}

	for i := range rs.Filter {
		rule := &rs.Filter[i]
		if !payloadRuleMatches(rule, out, model, headers, identity) {
			continue
		}
		var paths []string
		if json.Unmarshal(rule.Params, &paths) != nil {
			continue
		}
		for _, path := range paths {
			if isProtectedPayloadPath(path) {
				continue
			}
			if updated, err := sjson.DeleteBytes(out, path); err == nil {
				out = updated
			}
		}
	}
	return out
}

// payloadAppendAlreadyApplied 判断 text 是否已作为完整追加块存在于 prev 中：
// prev 整体即 text，或 text 以 "\n\n" 分隔符对齐地出现在开头/结尾/中间。
// 下游正文恰好自带同样完整块时跳过追加与追加后去重等价，不改变最终语义。
func payloadAppendAlreadyApplied(prev, text string) bool {
	return prev == text ||
		strings.HasPrefix(prev, text+"\n\n") ||
		strings.HasSuffix(prev, "\n\n"+text) ||
		strings.Contains(prev, "\n\n"+text+"\n\n")
}

// EffectiveRequestedServiceTier 返回请求体经当前规则改写后将真正发往上游的 service_tier，
// 供用量日志按覆写后的值归因 requested/billing tier。输入须与 ExecuteRequest 喂给引擎的一致
// （生图请求旁路规则，与 executor.go 保持一致）。无规则命中时返回原值。
func EffectiveRequestedServiceTier(body []byte, model string, headers http.Header, identity *PayloadRuleIdentity) string {
	if responsesBodyRequestsImageGeneration(body) {
		return extractServiceTier(body)
	}
	return extractServiceTier(ApplyPayloadRulesToBody(body, model, headers, identity))
}
