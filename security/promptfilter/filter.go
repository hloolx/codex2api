package promptfilter

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/url"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

const (
	ActionAllow = "allow"
	ActionWarn  = "warn"
	ActionBlock = "block"

	ModeMonitor = "monitor"
	ModeWarn    = "warn"
	ModeBlock   = "block"

	DefaultThreshold              = 50
	DefaultStrictThreshold        = 90
	DefaultMaxTextLength          = 80 * 1024
	configuredSensitiveWordWeight = 25
	defaultHeadScanLength         = 64 * 1024
	defaultTailScanLength         = 16 * 1024
	HitStartMarker                = "⟦PF_HIT⟧"
	HitEndMarker                  = "⟦/PF_HIT⟧"
	encodedScanIncompleteMatch    = "encoded_scan_incomplete"
	encodedScanIncompleteCategory = "normalization_review"
)

type Config struct {
	Enabled               bool            `json:"enabled"`
	Mode                  string          `json:"mode"`
	Threshold             int             `json:"threshold"`
	StrictThreshold       int             `json:"strict_threshold"`
	StrictTerminalEnabled bool            `json:"strict_terminal_enabled"`
	LogMatches            bool            `json:"log_matches"`
	MaxTextLength         int             `json:"max_text_length"`
	SensitiveWords        string          `json:"sensitive_words"`
	CustomPatterns        []PatternConfig `json:"custom_patterns"`
	DisabledPatterns      []string        `json:"disabled_patterns"`
	Review                ReviewConfig    `json:"review"`
	Advanced              AdvancedConfig  `json:"advanced"`
}

type ReviewConfig struct {
	Enabled        bool                `json:"enabled"`
	APIKey         string              `json:"api_key,omitempty"`
	BaseURL        string              `json:"base_url"`
	Model          string              `json:"model"`
	TimeoutSeconds int                 `json:"timeout_seconds"`
	FailClosed     bool                `json:"fail_closed"`
	Adapter        ReviewAdapterConfig `json:"adapter"`
}

type PatternConfig struct {
	Name            string   `json:"name"`
	Pattern         string   `json:"pattern"`
	Weight          int      `json:"weight"`
	Category        string   `json:"category,omitempty"`
	Strict          bool     `json:"strict,omitempty"`
	SignalOnly      bool     `json:"signal_only,omitempty"`
	Enabled         *bool    `json:"enabled,omitempty"`
	AllPatterns     []string `json:"all_patterns,omitempty"`
	AnyPatterns     []string `json:"any_patterns,omitempty"`
	ExcludePatterns []string `json:"exclude_patterns,omitempty"`
	// AuthorizationExcludePatterns 是"声明式授权"豁免条件,只在
	// Advanced.Enforcement.AuthorizedPentestAllowed 打开时才并入 ExcludePatterns。
	// 与普通排除条件分开存放,使"是否承认无法验证的授权自述"成为显式的部署策略,
	// 而不是硬编码在规则里的隐含行为。
	AuthorizationExcludePatterns []string `json:"authorization_exclude_patterns,omitempty"`
	MinMatches                   int      `json:"min_matches,omitempty"`
}

// resolveAuthorizationExcludes 依据部署策略决定声明式授权是否具有豁免效力。
// 返回的 PatternConfig 始终持有独立的切片,避免污染进程级的 defaultPatternConfigs。
func resolveAuthorizationExcludes(pattern PatternConfig, allowed bool) PatternConfig {
	if len(pattern.AuthorizationExcludePatterns) == 0 {
		return pattern
	}
	authorization := pattern.AuthorizationExcludePatterns
	pattern.AuthorizationExcludePatterns = nil
	if !allowed {
		return pattern
	}
	merged := make([]string, 0, len(pattern.ExcludePatterns)+len(authorization))
	merged = append(merged, pattern.ExcludePatterns...)
	merged = append(merged, authorization...)
	pattern.ExcludePatterns = merged
	return pattern
}

type Match struct {
	Name       string `json:"name"`
	Weight     int    `json:"weight"`
	Category   string `json:"category,omitempty"`
	Strict     bool   `json:"strict,omitempty"`
	SignalOnly bool   `json:"signal_only,omitempty"`
}

type Verdict struct {
	Enabled             bool     `json:"enabled"`
	Mode                string   `json:"mode"`
	Action              string   `json:"action"`
	Score               int      `json:"score"`
	RawScore            int      `json:"raw_score"`
	RiskScore           int      `json:"risk_score,omitempty"`
	Threshold           int      `json:"threshold"`
	SensitiveIntent     bool     `json:"sensitive_intent"`
	StrictHit           bool     `json:"strict_hit"`
	TerminalStrictHit   bool     `json:"terminal_strict_hit"`
	TerminalCategoryHit bool     `json:"terminal_category_hit"`
	Matched             []Match  `json:"matched"`
	Reason              string   `json:"reason,omitempty"`
	TextPreview         string   `json:"text_preview,omitempty"`
	MatchContext        string   `json:"match_context,omitempty"`
	FullText            string   `json:"full_text,omitempty"`
	ExtractedChars      int      `json:"extracted_chars"`
	Reviewed            bool     `json:"reviewed,omitempty"`
	ReviewFlagged       bool     `json:"review_flagged,omitempty"`
	ReviewError         string   `json:"review_error,omitempty"`
	ReviewModel         string   `json:"review_model,omitempty"`
	ReviewConfidence    *float64 `json:"review_confidence,omitempty"`
	ReviewThreshold     *float64 `json:"review_threshold,omitempty"`
	ReviewReason        string   `json:"review_reason,omitempty"`
	ReviewEndpoint      string   `json:"review_endpoint,omitempty"`
	ReviewRequestMode   string   `json:"review_request_mode,omitempty"`
	ReviewLatencyMS     *int64   `json:"review_latency_ms,omitempty"`
}

type Engine struct {
	cfg                    Config
	patterns               []compiledPattern
	sensitiveWords         []string
	literalIndex           *literalIndex
	viewMatchHintAutomaton *decodedSafetyHintAutomaton
	decodedPriorityScanner decodedSafetyPriorityScanner
	exactPrecheckScanner   decodedSafetyPriorityScanner
}

type compiledPattern struct {
	cfg                   PatternConfig
	re                    *regexp.Regexp
	requires              []string
	guaranteedHintClauses [][]string
	all                   []*regexp.Regexp
	any                   []*regexp.Regexp
	exclude               []*regexp.Regexp
}

type literalIndex struct {
	literals []literalNeedle
}

type literalNeedle struct {
	text string
}

func DefaultConfig() Config {
	return Config{
		Enabled:         false,
		Mode:            ModeMonitor,
		Threshold:       DefaultThreshold,
		StrictThreshold: DefaultStrictThreshold,
		LogMatches:      true,
		MaxTextLength:   DefaultMaxTextLength,
		Review:          DefaultReviewConfig(),
		Advanced:        DefaultAdvancedConfig(),
	}
}

// RecommendedConfig returns the safe production preset used for fresh
// installations and UI fallbacks. The master switch remains off so users keep
// explicit control over protection; once enabled, requests are blocked only by
// high-confidence current-user rules with normalization enabled.
func RecommendedConfig() Config {
	cfg := DefaultConfig()
	cfg.Mode = ModeBlock
	cfg.StrictTerminalEnabled = true
	cfg.Advanced = RecommendedAdvancedConfig()
	return cfg
}

func DefaultReviewConfig() ReviewConfig {
	return ReviewConfig{
		BaseURL:        DefaultReviewBaseURL,
		Model:          DefaultReviewModel,
		TimeoutSeconds: DefaultReviewTimeoutSeconds,
		FailClosed:     true,
	}
}

func ParseCustomPatterns(raw string) ([]PatternConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []PatternConfig
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("invalid custom_patterns JSON: %w", err)
	}
	return out, nil
}

func MarshalCustomPatterns(patterns []PatternConfig) string {
	if len(patterns) == 0 {
		return "[]"
	}
	data, err := json.Marshal(patterns)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func ParseDisabledPatterns(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil, fmt.Errorf("invalid disabled_patterns JSON: %w", err)
	}
	return normalizePatternNames(names), nil
}

func MarshalDisabledPatterns(names []string) string {
	names = normalizePatternNames(names)
	if len(names) == 0 {
		return "[]"
	}
	data, err := json.Marshal(names)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func NormalizeConfig(cfg Config) Config {
	defaults := DefaultConfig()
	if strings.TrimSpace(cfg.Mode) == "" {
		cfg.Mode = defaults.Mode
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Mode)) {
	case ModeBlock:
		cfg.Mode = ModeBlock
	case ModeWarn:
		cfg.Mode = ModeWarn
	default:
		cfg.Mode = ModeMonitor
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = defaults.Threshold
	}
	if cfg.Threshold > 500 {
		cfg.Threshold = 500
	}
	if cfg.StrictThreshold <= 0 {
		cfg.StrictThreshold = defaults.StrictThreshold
	}
	if cfg.StrictThreshold < cfg.Threshold {
		cfg.StrictThreshold = cfg.Threshold
	}
	if cfg.StrictThreshold > 1000 {
		cfg.StrictThreshold = 1000
	}
	if cfg.MaxTextLength <= 0 {
		cfg.MaxTextLength = defaults.MaxTextLength
	}
	if cfg.MaxTextLength > 1024*1024 {
		cfg.MaxTextLength = 1024 * 1024
	}
	cfg.DisabledPatterns = normalizePatternNames(cfg.DisabledPatterns)
	cfg.Review = NormalizeReviewConfig(cfg.Review)
	cfg.Advanced = NormalizeAdvancedConfig(cfg.Advanced)
	return cfg
}

var engineCache sync.Map // map[string]*Engine

func engineForConfig(cfg Config) (*Engine, error) {
	key := engineCacheKey(cfg)
	if cached, ok := engineCache.Load(key); ok {
		return cached.(*Engine), nil
	}
	engine, err := NewEngine(cfg)
	if err != nil {
		return nil, err
	}
	actual, _ := engineCache.LoadOrStore(key, engine)
	return actual.(*Engine), nil
}

func engineCacheKey(cfg Config) string {
	cfg = NormalizeConfig(cfg)
	key := struct {
		Enabled               bool            `json:"enabled"`
		Mode                  string          `json:"mode"`
		Threshold             int             `json:"threshold"`
		StrictThreshold       int             `json:"strict_threshold"`
		StrictTerminalEnabled bool            `json:"strict_terminal_enabled"`
		MaxTextLength         int             `json:"max_text_length"`
		SensitiveWords        string          `json:"sensitive_words"`
		CustomPatterns        []PatternConfig `json:"custom_patterns"`
		DisabledPatterns      []string        `json:"disabled_patterns"`
		DetectionAdvanced     struct {
			Normalization   NormalizationConfig   `json:"normalization"`
			ContextDiscount ContextDiscountConfig `json:"context_discount"`
			Enforcement     EnforcementConfig     `json:"enforcement"`
		} `json:"advanced"`
	}{
		Enabled:               cfg.Enabled,
		Mode:                  cfg.Mode,
		Threshold:             cfg.Threshold,
		StrictThreshold:       cfg.StrictThreshold,
		StrictTerminalEnabled: cfg.StrictTerminalEnabled,
		MaxTextLength:         cfg.MaxTextLength,
		SensitiveWords:        cfg.SensitiveWords,
		CustomPatterns:        cfg.CustomPatterns,
		DisabledPatterns:      cfg.DisabledPatterns,
	}
	key.DetectionAdvanced.Normalization = cfg.Advanced.Normalization
	key.DetectionAdvanced.ContextDiscount = cfg.Advanced.ContextDiscount
	key.DetectionAdvanced.Enforcement = cfg.Advanced.Enforcement
	data, err := json.Marshal(key)
	if err != nil {
		detectionAdvanced, _ := json.Marshal(key.DetectionAdvanced)
		return fmt.Sprintf("%t|%s|%d|%d|%t|%d|%s|%s|%s|%s", cfg.Enabled, cfg.Mode, cfg.Threshold, cfg.StrictThreshold, cfg.StrictTerminalEnabled, cfg.MaxTextLength, cfg.SensitiveWords, MarshalCustomPatterns(cfg.CustomPatterns), MarshalDisabledPatterns(cfg.DisabledPatterns), string(detectionAdvanced))
	}
	return string(data)
}

func NewEngine(cfg Config) (*Engine, error) {
	cfg = NormalizeConfig(cfg)
	// The regex engine never uses remote-review credentials. Clear them before
	// the normalized config is retained by the process-wide engine cache so
	// operational credential rotation cannot leave old secrets reachable.
	cfg.Review.APIKey = ""
	disabled := disabledPatternSet(cfg.DisabledPatterns)
	merged := append([]PatternConfig{}, defaultPatternConfigs...)
	merged = append(merged, cfg.CustomPatterns...)
	builtinCount := len(defaultPatternConfigs)

	patterns := make([]compiledPattern, 0, len(merged))
	authorizedPentestAllowed := cfg.Advanced.Enforcement.AuthorizedPentestAllowed
patternLoop:
	for index, pattern := range merged {
		custom := index >= builtinCount
		pattern = resolveAuthorizationExcludes(pattern, authorizedPentestAllowed)
		pattern.Name = strings.TrimSpace(pattern.Name)
		pattern.Pattern = strings.TrimSpace(pattern.Pattern)
		pattern.Category = strings.TrimSpace(pattern.Category)
		if disabled[strings.ToLower(pattern.Name)] {
			continue
		}
		if pattern.Enabled != nil && !*pattern.Enabled {
			continue
		}
		// Built-ins are trusted release artifacts: a compile failure is fatal so
		// tests and startup validation expose the broken release. Custom rules
		// are untrusted persisted input; quarantine one bad or over-broad rule
		// without disabling every built-in detector for the request.
		if custom {
			if issue := AuditPatternConfig(pattern); issue != nil {
				log.Printf("prompt filter: custom rule skipped name=%q code=%s message=%s", pattern.Name, issue.Code, issue.Message)
				continue
			}
		}
		if pattern.Name == "" || (pattern.Pattern == "" && len(pattern.AllPatterns) == 0 && len(pattern.AnyPatterns) == 0) || pattern.Weight <= 0 {
			continue
		}
		var re *regexp.Regexp
		var err error
		if pattern.Pattern != "" {
			re, err = regexp.Compile(pattern.Pattern)
			if err != nil {
				if custom {
					log.Printf("prompt filter: custom rule skipped name=%q code=%s message=%s", pattern.Name, PatternQuarantineInvalidRegex, err)
					continue
				}
				return nil, fmt.Errorf("compile pattern %q: %w", pattern.Name, err)
			}
		}
		compileList := func(items []string) ([]*regexp.Regexp, error) {
			out := make([]*regexp.Regexp, 0, len(items))
			for _, item := range items {
				if strings.TrimSpace(item) == "" {
					return nil, fmt.Errorf("empty regex")
				}
				x, e := regexp.Compile(item)
				if e != nil {
					return nil, e
				}
				out = append(out, x)
			}
			return out, nil
		}
		all, err := compileList(pattern.AllPatterns)
		if err != nil {
			if custom {
				log.Printf("prompt filter: custom rule skipped name=%q code=%s field=all_patterns message=%s", pattern.Name, PatternQuarantineInvalidRegex, err)
				continue patternLoop
			}
			return nil, fmt.Errorf("compile all pattern %q: %w", pattern.Name, err)
		}
		any, err := compileList(pattern.AnyPatterns)
		if err != nil {
			if custom {
				log.Printf("prompt filter: custom rule skipped name=%q code=%s field=any_patterns message=%s", pattern.Name, PatternQuarantineInvalidRegex, err)
				continue patternLoop
			}
			return nil, fmt.Errorf("compile any pattern %q: %w", pattern.Name, err)
		}
		exclude, err := compileList(pattern.ExcludePatterns)
		if err != nil {
			if custom {
				log.Printf("prompt filter: custom rule skipped name=%q code=%s field=exclude_patterns message=%s", pattern.Name, PatternQuarantineInvalidRegex, err)
				continue patternLoop
			}
			return nil, fmt.Errorf("compile exclude pattern %q: %w", pattern.Name, err)
		}
		compiled := compiledPattern{
			cfg:      pattern,
			re:       re,
			requires: patternRequires(pattern.Pattern),
			all:      all, any: any, exclude: exclude,
		}
		compiled.guaranteedHintClauses = decodedSafetyGuaranteedHintClauses(compiled)
		patterns = append(patterns, compiled)
	}
	sensitiveWords := parseSensitiveWords(cfg.SensitiveWords)

	engine := &Engine{
		cfg:                    cfg,
		patterns:               patterns,
		sensitiveWords:         sensitiveWords,
		literalIndex:           buildLiteralIndex(patterns, sensitiveWords),
		viewMatchHintAutomaton: buildGuaranteedPatternHintAutomaton(patterns),
	}
	// Keep decoded-rule prioritization bound to this exact immutable Engine.
	// A process-global scan over cached engines can leak another tenant/config's
	// custom rules into this request's encoded-candidate budget.
	priorityScanner := buildDecodedSafetyPriorityScanner(patterns)
	priorityScanner.sensitiveWords = sensitiveWords
	priorityScanner.hintIndex = buildDecodedSafetyHintIndex(priorityScanner)
	engine.decodedPriorityScanner = priorityScanner
	exactPrecheckScanner := buildDecodedSafetyExactPrecheckScanner(patterns, cfg.Threshold)
	exactPrecheckScanner.sensitiveWords = sensitiveWords
	exactPrecheckScanner.hintIndex = buildDecodedSafetyHintIndex(exactPrecheckScanner)
	engine.exactPrecheckScanner = exactPrecheckScanner
	return engine, nil
}

func BuiltinPatternConfigs() []PatternConfig {
	out := make([]PatternConfig, len(defaultPatternConfigs))
	copy(out, defaultPatternConfigs)
	return out
}

func patternShouldRun(text string, pattern compiledPattern, literalHits map[string]bool) bool {
	if isBuiltinMinorSafetyPattern(pattern) && !minorSafetyHasCandidateHint(text) {
		return false
	}
	for _, required := range pattern.requires {
		if !literalMatched(text, literalHits, required) {
			return false
		}
	}
	return true
}

func isBuiltinMinorSafetyPattern(pattern compiledPattern) bool {
	return pattern.cfg.Name == "minor_exploitation" && pattern.cfg.Category == "minor_safety" && pattern.cfg.Pattern == minorExploitationPattern && pattern.re != nil && len(pattern.all) == 0 && len(pattern.any) == 0 && len(pattern.exclude) == 0
}

func minorSafetyHasCandidateHint(text string) bool {
	for _, hint := range minorSafetyCandidateHints {
		if strings.Contains(text, hint) {
			return true
		}
	}
	return minorSafetyAgeHintPattern.MatchString(text)
}

func literalMatched(text string, literalHits map[string]bool, literal string) bool {
	if literal == "" {
		return true
	}
	if literalHits != nil {
		return literalHits[literal]
	}
	return strings.Contains(text, literal)
}

func (idx *literalIndex) match(text string) map[string]bool {
	if idx == nil || len(idx.literals) == 0 || text == "" {
		return nil
	}
	hits := make(map[string]bool, len(idx.literals))
	for _, literal := range idx.literals {
		if strings.Contains(text, literal.text) {
			hits[literal.text] = true
		}
	}
	return hits
}

func buildLiteralIndex(patterns []compiledPattern, sensitiveWords []string) *literalIndex {
	index := &literalIndex{}
	seen := map[string]int{}
	add := func(text string) int {
		text = strings.TrimSpace(text)
		if text == "" {
			return -1
		}
		if existing, ok := seen[text]; ok {
			return existing
		}
		id := len(index.literals)
		seen[text] = id
		index.literals = append(index.literals, literalNeedle{text: text})
		return id
	}
	for _, pattern := range patterns {
		for _, literal := range pattern.requires {
			add(literal)
		}
	}
	for _, word := range sensitiveWords {
		add(word)
	}
	return index
}

func patternRequires(pattern string) []string {
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil
	}
	return regexpRequiredLiterals(parsed.Simplify())
}

func regexpRequiredLiterals(re *syntax.Regexp) []string {
	// normalizeForScan lowercases text but does not collapse every Unicode
	// SimpleFold equivalent (for example long-s). A plain lowercase literal is
	// therefore not a correctness-safe hard gate for case-insensitive regexps.
	// The separate syntax-proven Aho gate handles fold-stable substrings; keep
	// this legacy fast path only for case-sensitive expressions.
	if regexpHasFoldCaseLiteral(re) {
		return nil
	}
	literals := requiredLiteralSet(re)
	return sortedLiteralSet(literals, 4)
}

func regexpHasFoldCaseLiteral(re *syntax.Regexp) bool {
	if re == nil {
		return false
	}
	if re.Op == syntax.OpLiteral && re.Flags&syntax.FoldCase != 0 {
		return true
	}
	for _, child := range re.Sub {
		if regexpHasFoldCaseLiteral(child) {
			return true
		}
	}
	return false
}

func requiredLiteralSet(re *syntax.Regexp) map[string]struct{} {
	if re == nil {
		return nil
	}
	switch re.Op {
	case syntax.OpLiteral:
		return literalSetFromRunes(re.Rune, 4)
	case syntax.OpCapture, syntax.OpPlus:
		return requiredLiteralSet(re.Sub[0])
	case syntax.OpConcat:
		out := map[string]struct{}{}
		for _, sub := range re.Sub {
			for literal := range requiredLiteralSet(sub) {
				out[literal] = struct{}{}
			}
		}
		return out
	case syntax.OpAlternate:
		var common map[string]struct{}
		for _, sub := range re.Sub {
			literals := requiredLiteralSet(sub)
			if common == nil {
				common = literals
				continue
			}
			for literal := range common {
				if _, ok := literals[literal]; !ok {
					delete(common, literal)
				}
			}
		}
		return common
	}
	return nil
}

func literalSetFromRunes(runes []rune, minRunes int) map[string]struct{} {
	literal := normalizeForScan(string(runes))
	if utf8.RuneCountInString(literal) < minRunes {
		return nil
	}
	return map[string]struct{}{literal: {}}
}

func sortedLiteralSet(literals map[string]struct{}, minRunes int) []string {
	if len(literals) == 0 {
		return nil
	}
	out := make([]string, 0, len(literals))
	seen := map[string]struct{}{}
	for literal := range literals {
		literal = strings.TrimSpace(literal)
		if utf8.RuneCountInString(literal) < minRunes {
			continue
		}
		if _, ok := seen[literal]; ok {
			continue
		}
		seen[literal] = struct{}{}
		out = append(out, literal)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) == len(out[j]) {
			return out[i] < out[j]
		}
		return len(out[i]) > len(out[j])
	})
	return out
}

func Inspect(body []byte, endpoint string, cfg Config) Verdict {
	text := ExtractText(body, endpoint, NormalizeConfig(cfg).MaxTextLength)
	return InspectText(text, cfg)
}

// RequiresRequestText 判断当前配置下是否真的需要从请求正文提取文本。
// 本地 filter 主开关开启后，启用的外部 review 会审核每一个可提取的当前用户
// 输入，不再以本地 warn/block 作为触发条件。risk 只读取聚合分数与身份、
// newapi 只塑形已拦截响应；所有正文能力仍受同一个主开关控制。
//
// 全部相关能力关闭时返回 false，调用方应在提取正文前直接放行，避免对大请求体
// (曾达 16MB) 做无效的 JSON 遍历/UTF-8 解码 (issue #417)。
func RequiresRequestText(cfg Config) bool {
	return cfg.Enabled
}

func InspectText(text string, cfg Config) Verdict {
	cfg = NormalizeConfig(cfg)
	// 关闭或空文本时提前返回，避开 Preview 与全量 UTF-8 计数——大文本上这两步
	// 本身也可观测 (issue #417)。
	if !cfg.Enabled || strings.TrimSpace(text) == "" {
		return Verdict{
			Enabled:   cfg.Enabled,
			Mode:      cfg.Mode,
			Action:    ActionAllow,
			Threshold: cfg.Threshold,
		}
	}
	preview := RedactedPreview(text, 500)
	verdict := Verdict{
		Enabled:        cfg.Enabled,
		Mode:           cfg.Mode,
		Action:         ActionAllow,
		Threshold:      cfg.Threshold,
		TextPreview:    preview,
		ExtractedChars: utf8.RuneCountInString(text),
	}

	engine, err := engineForConfig(cfg)
	if err != nil {
		verdict.Reason = err.Error()
		return verdict
	}
	return engine.InspectTextWithPerformanceBudget(text, cfg.MaxTextLength, cfg.Advanced.Guard.Performance)
}

func (e *Engine) InspectText(text string) Verdict {
	return e.inspectText(text, false)
}

func (e *Engine) inspectText(text string, omitFullEvidence bool) Verdict {
	cfg := e.cfg
	if !cfg.Enabled || strings.TrimSpace(text) == "" {
		return e.newPreparedScanVerdict(text, omitFullEvidence)
	}

	limitedText := limitScanText(text, cfg.MaxTextLength)
	scanViewList := boundedEnforcementScanViews(scanViewsWithRuntimeBudget(limitedText, cfg.Advanced.Normalization, cfg.MaxTextLength, cfg.Advanced.Guard.Performance, e), cfg.MaxTextLength)
	return e.inspectPreparedScanViews(text, limitedText, scanViewList, omitFullEvidence)
}

func (e *Engine) newPreparedScanVerdict(text string, omitFullEvidence bool) Verdict {
	cfg := e.cfg
	verdict := Verdict{
		Enabled:   cfg.Enabled,
		Mode:      cfg.Mode,
		Action:    ActionAllow,
		Threshold: cfg.Threshold,
	}
	if !omitFullEvidence {
		verdict.TextPreview = RedactedPreview(text, 500)
		verdict.FullText = text
		verdict.ExtractedChars = utf8.RuneCountInString(text)
	}
	return verdict
}

// inspectPreparedScanViews is the single scoring/suppression core for both the
// ordinary detector and the over-budget exact CurrentUser precheck. Callers may
// spend different bounded effort discovering normalization views, but every
// match still flows through the same exclusion, defensive-context, scoring,
// strict, terminal, and incomplete-normalization semantics.
func (e *Engine) inspectPreparedScanViews(evidenceText string, policyText string, scanViewList []scanView, omitFullEvidence bool) Verdict {
	cfg := e.cfg
	verdict := e.newPreparedScanVerdict(evidenceText, omitFullEvidence)
	if !cfg.Enabled || strings.TrimSpace(policyText) == "" {
		return verdict
	}
	if len(scanViewList) == 0 {
		return verdict
	}
	scanTexts := make([]string, len(scanViewList))
	normalizationIncomplete := false
	for i, view := range scanViewList {
		scanTexts[i] = view.Text
		normalizationIncomplete = normalizationIncomplete || view.NormalizationIncomplete
	}

	matchContexts := make([]string, 0, 3)
	recordContext := func(context string) {
		context = strings.TrimSpace(context)
		if context == "" || len(matchContexts) >= 3 {
			return
		}
		for _, existing := range matchContexts {
			if existing == context {
				return
			}
		}
		matchContexts = append(matchContexts, context)
	}
	matchesByName := map[string]Match{}
	rawScore := 0
	signalScore := 0
	decisionScore := 0
	strictScore := 0
	maxStrictRuleScore := 0
	terminalCategories := make(map[string]bool, len(cfg.Advanced.Enforcement.TerminalCategories))
	decisionCategoryScores := make(map[string]int)
	strictCategoryScores := make(map[string]int)
	for _, category := range cfg.Advanced.Enforcement.TerminalCategories {
		terminalCategories[strings.ToLower(category)] = true
	}
	for _, view := range scanViewList {
		scanText := view.Text
		if utf8.RuneCountInString(scanText) < 2 {
			continue
		}
		// A decoded fragment that is both quoted and explicitly marked as a
		// non-executing review sample is evidence, not an active request. Keep
		// provenance on the derived view so every detector family—not only the
		// minor-safety rule—avoids turning a safe encoded fixture into a block.
		// If the same decoded text also appears actively, scanViews clears this
		// flag and the active occurrence is still enforced.
		if view.ReviewOnly {
			continue
		}
		literalHits := e.literalIndex.match(scanText)
		var guaranteedHintMatches map[string]struct{}
		if !view.Compacted && e.viewMatchHintAutomaton != nil {
			guaranteedHintMatches = e.viewMatchHintAutomaton.match(scanText)
		}
		if !view.Compacted {
			for _, word := range e.sensitiveWords {
				if word == "" {
					continue
				}
				if loc := sensitiveWordMatchIndex(scanText, literalHits, word); loc != nil {
					// Administrator-configured words are evidence, not a complete policy
					// decision. A standalone product, tool, or security topic such as C2,
					// IDA, CVE, or PowerShell must remain usable in ordinary development
					// and defensive discussion. Explicit operational intent is still
					// enforced by the built-in intent and terminal rules.
					match := Match{
						Name:       "sensitive_word",
						Weight:     configuredSensitiveWordWeight,
						Category:   "sensitive_word",
						SignalOnly: true,
					}
					_, context := regexMatchContext(scanText, loc)
					recordContext(context)
					matchesByName[match.Name+":"+word] = match
				}
			}
		}
		for _, pattern := range e.patterns {
			// A rule contributes at most one request-wide match. Once an
			// unsuppressed match was recorded, evaluating the same regexp and its
			// policy-wide suppressors in later normalization views cannot change the
			// verdict and is especially expensive for near-1MiB prompts.
			if _, exists := matchesByName[pattern.cfg.Name]; exists {
				continue
			}
			if view.Compacted && !isBuiltinMinorSafetyPattern(pattern) {
				continue
			}
			// Only syntax-proven mandatory literals participate in this gate. A
			// regex match cannot exist when none of its proven clauses exists in
			// the same normalized view, so skip the substantially more expensive
			// suppression and regexp work. Heuristic decoded-candidate hints are
			// intentionally excluded because they are useful for prioritization,
			// but are not a correctness proof.
			if !view.Compacted && len(pattern.guaranteedHintClauses) > 0 &&
				!containsDecodedSafetyHintClauseWithMatches("", pattern.guaranteedHintClauses, guaranteedHintMatches) {
				continue
			}
			if !patternShouldRun(scanText, pattern, literalHits) {
				continue
			}
			var loc []int
			if view.Compacted && isBuiltinMinorSafetyPattern(pattern) {
				loc = minorSafetyCompactMaterialMatchIndex(scanText)
			} else {
				loc = compiledPatternMatchIndex(scanText, pattern)
			}
			if loc == nil {
				continue
			}
			// Suppression is meaningful only after the rule has matched this view.
			// Keeping it behind the match prevents every selected rule from running
			// several policy-wide regexes over a near-1MiB request that it never
			// matched, without changing any decision or suppression semantics.
			if patternSuppressedForQuotedPolicyReview(policyText, pattern) ||
				patternSuppressedForDefensiveRuleArtifact(policyText, pattern) ||
				patternSuppressedForAuthorizationBoundary(policyText, scanText, pattern) ||
				patternSuppressedForNegatedPolicyAction(policyText, scanText, pattern) ||
				patternSuppressedForNegatedExfiltrationMention(policyText, scanText, pattern) ||
				patternSuppressedForProtectiveRefusal(policyText, scanText, pattern) ||
				patternSuppressedForNarrativeRefusal(policyText, scanText, pattern) ||
				patternSuppressedForDefensiveDocumentation(policyText, pattern) {
				continue
			}
			match := Match{Name: pattern.cfg.Name, Weight: pattern.cfg.Weight, Category: pattern.cfg.Category, Strict: pattern.cfg.Strict, SignalOnly: pattern.cfg.SignalOnly}
			_, context := regexMatchContext(scanText, loc)
			recordContext(context)
			matchesByName[match.Name] = match
		}
	}
	if normalizationIncomplete {
		// This is an operational review signal, not malicious-rule evidence. It
		// records that an active encoded/compressed payload exceeded a bounded
		// normalization scan. It must never become terminal or strike-eligible.
		matchesByName[encodedScanIncompleteMatch] = Match{
			Name:       encodedScanIncompleteMatch,
			Weight:     1,
			Category:   encodedScanIncompleteCategory,
			SignalOnly: true,
		}
	}

	matches := make([]Match, 0, len(matchesByName))
	for _, match := range matchesByName {
		matches = append(matches, match)
		rawScore += match.Weight
		decisionBearing := !match.SignalOnly || match.Strict
		if !decisionBearing {
			signalScore += match.Weight
		} else {
			decisionScore += match.Weight
		}
		if match.Strict {
			strictScore += match.Weight
			if match.Weight > maxStrictRuleScore {
				maxStrictRuleScore = match.Weight
			}
		}
		category := strings.ToLower(strings.TrimSpace(match.Category))
		if decisionBearing && category != "" {
			decisionCategoryScores[category] += match.Weight
			if match.Strict {
				strictCategoryScores[category] += match.Weight
			}
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Weight == matches[j].Weight {
			return matches[i].Name < matches[j].Name
		}
		return matches[i].Weight > matches[j].Weight
	})

	// Signal-only matches remain visible in RawScore and audit logs, but they
	// never raise the score used for enforcement. High-confidence combinations
	// must be expressed as intent-bearing or terminal rules instead of relying
	// on a pile-up of product names and security topics.
	score := decisionScore
	contextDiscount := 0
	strictEvidenceFloor := cfg.Threshold / 2
	if strictEvidenceFloor < 1 {
		strictEvidenceFloor = 1
	}
	// Defensive context may receive a smaller discount for an operational
	// request, but only when the evidence that claims high confidence is already
	// self-sufficient before discounting. A one-point strict marker in an
	// unrelated category must not remove the discount from an otherwise
	// sub-threshold ordinary rule and thereby manufacture a block.
	strictConfidenceThreshold := cfg.StrictThreshold
	if cfg.StrictTerminalEnabled {
		strictConfidenceThreshold = cfg.Threshold
	}
	highConfidenceContextEvidence := maxStrictRuleScore >= strictConfidenceThreshold
	for category, categoryStrictScore := range strictCategoryScores {
		if categoryStrictScore >= strictConfidenceThreshold ||
			(cfg.StrictTerminalEnabled && categoryStrictScore >= strictEvidenceFloor && decisionCategoryScores[category] >= cfg.Threshold) {
			highConfidenceContextEvidence = true
			break
		}
	}
	if !highConfidenceContextEvidence {
		for category := range terminalCategories {
			if decisionCategoryScores[category] >= cfg.Threshold {
				highConfidenceContextEvidence = true
				break
			}
		}
	}
	if decisionScore == 0 && strictScore == 0 && signalScore > 0 {
		score = signalScore
		signalOnlyCap := cfg.Threshold / 2
		if score > signalOnlyCap {
			score = signalOnlyCap
		}
		if score < 0 {
			score = 0
		}
	}
	if score > 0 {
		contextDiscount = defensiveContextDiscount(policyText, scanTexts, cfg.Advanced.ContextDiscount, highConfidenceContextEvidence)
		score -= contextDiscount
		if score < 0 {
			score = 0
		}
	}
	sensitiveIntent := decisionScore > 0 && score >= cfg.Threshold
	maxStrictEvidenceScore := maxStrictRuleScore
	for _, categoryStrictScore := range strictCategoryScores {
		if categoryStrictScore > maxStrictEvidenceScore {
			maxStrictEvidenceScore = categoryStrictScore
		}
	}
	maxStrictEvidenceScore -= contextDiscount
	if maxStrictEvidenceScore < 0 {
		maxStrictEvidenceScore = 0
	}
	terminalStrictHit := cfg.StrictTerminalEnabled && maxStrictEvidenceScore >= cfg.Threshold
	if cfg.StrictTerminalEnabled && !terminalStrictHit {
		for category, categoryStrictScore := range strictCategoryScores {
			if categoryStrictScore-contextDiscount >= strictEvidenceFloor && decisionCategoryScores[category]-contextDiscount >= cfg.Threshold {
				terminalStrictHit = true
				break
			}
		}
	}
	terminalCategoryHit := false
	for category := range terminalCategories {
		if decisionCategoryScores[category]-contextDiscount >= cfg.Threshold {
			terminalCategoryHit = true
			break
		}
	}
	// With terminal enforcement disabled, a strict rule remains a high-confidence
	// marker but cannot override an effective score that defensive context already
	// reduced below the normal blocking threshold.
	strictHit := terminalStrictHit || (maxStrictEvidenceScore >= cfg.StrictThreshold && sensitiveIntent)
	action := ActionAllow
	if terminalStrictHit || terminalCategoryHit {
		action = ActionBlock
	} else if sensitiveIntent || strictHit {
		switch cfg.Mode {
		case ModeBlock:
			action = ActionBlock
		case ModeWarn:
			action = ActionWarn
		default:
			action = ActionAllow
		}
	}
	if action == ActionAllow && normalizationIncomplete && cfg.Mode != ModeMonitor {
		// Every enforcement profile forwards the request with an explicit warning
		// so ordinary large archives are not falsely punished. Reaching a bounded
		// normalization limit never becomes terminal or strike-eligible by itself.
		action = ActionWarn
	}

	verdict.Action = action
	verdict.Score = score
	verdict.RawScore = rawScore
	verdict.SensitiveIntent = sensitiveIntent
	verdict.StrictHit = strictHit
	verdict.TerminalStrictHit = terminalStrictHit
	verdict.TerminalCategoryHit = terminalCategoryHit
	verdict.Matched = matches
	if len(matches) > 0 {
		verdict.Reason = reasonForVerdict(action, score, cfg.Threshold, matches)
	}
	if normalizationIncomplete && action == ActionWarn && !sensitiveIntent && !terminalStrictHit && !terminalCategoryHit {
		verdict.Reason = "prompt requires review: active encoded content exceeded bounded normalization scan limits"
	}
	if len(matchContexts) > 0 {
		verdict.MatchContext = strings.Join(matchContexts, "\n---\n")
	}
	return verdict
}

// InspectTextWithBudget reuses the immutable compiled detector while applying
// a request-source-specific text and normalization budget. The shallow Engine
// copy shares only read-only matcher state and does not enter the process-wide
// engine cache under a second configuration identity.
func (e *Engine) InspectTextWithBudget(text string, maxBytes int) Verdict {
	if e == nil {
		return Verdict{}
	}
	return e.InspectTextWithPerformanceBudget(text, maxBytes, e.cfg.Advanced.Guard.Performance)
}

// InspectTextWithPerformanceBudget applies the current operational scan
// window explicitly. Guard performance is deliberately excluded from the
// compiled Engine cache key, so hot configuration updates must not read these
// values back from a previously cached Engine instance.
func (e *Engine) InspectTextWithPerformanceBudget(text string, maxBytes int, performance GuardPerformanceConfig) Verdict {
	return e.inspectTextWithPerformanceBudget(text, maxBytes, performance, false)
}

func (e *Engine) inspectTextWithPerformanceBudget(text string, maxBytes int, performance GuardPerformanceConfig, omitFullEvidence bool) Verdict {
	if e == nil {
		return Verdict{}
	}
	if maxBytes <= 0 {
		maxBytes = e.cfg.MaxTextLength
	}
	if maxBytes > MaxGuardCurrentUserBytes {
		maxBytes = MaxGuardCurrentUserBytes
	}
	text = limitScanTextExact(text, maxBytes)
	bounded := *e
	bounded.cfg = e.cfg
	bounded.cfg.MaxTextLength = maxBytes
	bounded.cfg.Advanced.Guard.Performance = performance
	if bounded.cfg.Advanced.Normalization.MaxDecodedBytes > maxBytes {
		bounded.cfg.Advanced.Normalization.MaxDecodedBytes = maxBytes
	}
	return bounded.inspectText(text, omitFullEvidence)
}

// inspectExactCurrentUserPrecheck evaluates the complete CurrentUser view
// without running every regex over the entire source. Normalization views are
// generated at most once under the ordinary byte/block/compression budgets,
// then reused by both hint selection and the shared scoring core. Rules without
// a provable literal remain eligible. Exact candidate retention keeps decoded
// sub-threshold rules available for cumulative scoring without changing their
// configured weights, strictness, suppression, or final enforcement semantics.
func (e *Engine) inspectExactCurrentUserPrecheck(text string, performance GuardPerformanceConfig) Verdict {
	if e == nil || strings.TrimSpace(text) == "" {
		return Verdict{}
	}
	maxBytes := len(text)
	if maxBytes > MaxGuardCurrentUserBytes {
		maxBytes = MaxGuardCurrentUserBytes
	}
	text = limitScanTextExact(text, maxBytes)

	scanner := e.exactPrecheckScanner
	needsDerivedViews := exactPrecheckNeedsDerivedViews(text, e.cfg.Advanced.Normalization, scanner)
	var scanViewList []scanView
	if needsDerivedViews {
		selector := *e
		selector.decodedPriorityScanner = scanner
		selector.cfg = e.cfg
		selector.cfg.MaxTextLength = maxBytes
		selector.cfg.Advanced.Guard.Performance = performance
		if selector.cfg.Advanced.Normalization.MaxDecodedBytes > maxBytes {
			selector.cfg.Advanced.Normalization.MaxDecodedBytes = maxBytes
		}
		scanViewList = boundedEnforcementScanViews(
			scanViewsWithRuntimeBudget(text, selector.cfg.Advanced.Normalization, maxBytes, performance, &selector),
			maxBytes,
		)
	} else {
		scanViewList = []scanView{{Text: normalizeForScan(text)}}
	}

	matchedHints := make(map[string]struct{})
	for _, view := range scanViewList {
		if view.ReviewOnly {
			continue
		}
		viewHints, _ := decodedSafetyPriorityMatchedHints(view.Text, scanner)
		for hint := range viewHints {
			matchedHints[hint] = struct{}{}
		}
	}
	selectedPatterns := make([]compiledPattern, 0, len(scanner.patterns))
	for _, candidate := range scanner.patterns {
		if len(candidate.hintClauses) > 0 && !containsDecodedSafetyHintClauseWithMatches("", candidate.hintClauses, matchedHints) {
			continue
		}
		selectedPatterns = append(selectedPatterns, candidate.pattern)
	}
	selectedWords := make([]string, 0, len(scanner.sensitiveWords))
	for _, word := range scanner.sensitiveWords {
		if _, exists := matchedHints[word]; exists {
			selectedWords = append(selectedWords, word)
		}
	}
	filtered := *e
	filtered.patterns = selectedPatterns
	filtered.sensitiveWords = selectedWords
	filtered.literalIndex = buildLiteralIndex(selectedPatterns, selectedWords)
	filtered.cfg = e.cfg
	filtered.cfg.MaxTextLength = maxBytes
	filtered.cfg.Advanced.Guard.Performance = performance
	return filtered.inspectPreparedScanViews(text, text, scanViewList, true)
}

// inspectWindowedCurrentUserPrecheck keeps oversized CurrentUser inspection on
// the synchronous path without running every rule over every near-1MiB derived
// view. Every source byte remains covered by overlapping windows. Windows stay
// as independent scan views, while the existing shared scoring core performs one
// final request-wide decision. This preserves rule-name deduplication, cumulative
// scoring, strict/category semantics, exclusions, and defensive context without
// manufacturing regex matches across concatenated window boundaries. Non-strict
// signal-only vocabulary is omitted because it cannot affect the current action;
// strict signal-only custom rules remain decision-bearing.
func (e *Engine) inspectWindowedCurrentUserPrecheck(text string, performance GuardPerformanceConfig) Verdict {
	if e == nil || strings.TrimSpace(text) == "" {
		return Verdict{}
	}
	if len(text) > MaxGuardCurrentUserBytes {
		text = limitScanTextExact(text, MaxGuardCurrentUserBytes)
	}
	chunkBytes, overlapBytes := guardScanWindow(performance)
	if chunkBytes < MaxGuardScanChunkBytes {
		chunkBytes = MaxGuardScanChunkBytes
	}
	if overlapBytes <= 0 || overlapBytes >= chunkBytes {
		overlapBytes = DefaultGuardScanOverlapBytes
		if overlapBytes >= chunkBytes {
			overlapBytes = max(1, chunkBytes/16)
		}
	}
	scanner := e.exactPrecheckScanner
	allViews := make([]scanView, 0, len(text)/chunkBytes*4+4)
	matchedHints := make(map[string]struct{})
	type scanViewKey struct {
		text                    string
		reviewOnly              bool
		compacted               bool
		normalizationIncomplete bool
	}
	seenViews := make(map[scanViewKey]struct{})
	appendViews := func(views []scanView) {
		for _, view := range views {
			key := scanViewKey{
				text:                    view.Text,
				reviewOnly:              view.ReviewOnly,
				compacted:               view.Compacted,
				normalizationIncomplete: view.NormalizationIncomplete,
			}
			if _, exists := seenViews[key]; exists {
				continue
			}
			seenViews[key] = struct{}{}
			allViews = append(allViews, view)
			if view.ReviewOnly {
				continue
			}
			viewHints, _ := decodedSafetyPriorityMatchedHints(view.Text, scanner)
			for hint := range viewHints {
				matchedHints[hint] = struct{}{}
			}
		}
	}

	// Keep one complete primary source view. It preserves the original exact
	// semantics for regexes, AllPatterns, and distant ExcludePatterns while the
	// expensive derived normalization work remains window-bounded below.
	primaryText := text
	if e.cfg.Advanced.Normalization.DecodeBase64 || e.cfg.Advanced.Normalization.DecodeHex {
		maxEncodedFragmentBytes := len(text)
		if maxEncodedFragmentBytes < 16*1024 {
			maxEncodedFragmentBytes = 16 * 1024
		}
		if maxEncodedFragmentBytes > MaxGuardCurrentUserBytes {
			maxEncodedFragmentBytes = MaxGuardCurrentUserBytes
		}
		primaryText = collapseRecognizedEncodedPayloads(text, e.cfg.Advanced.Normalization, maxEncodedFragmentBytes, nil)
	}
	appendViews([]scanView{{Text: normalizeForScan(primaryText)}})
	appendViews(fullCurrentUserDerivedViews(text, normalizeForScan(primaryText), e.cfg.Advanced.Normalization, scanner))
	appendViews(e.encodedCurrentUserPrecheckViews(text, chunkBytes, overlapBytes))
	reviewContext := newMinorSafetyScanContext(text)

	for start := 0; start < len(text); {
		end := min(start+chunkBytes, len(text))
		for end > start && end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
		if end <= start {
			_, size := utf8.DecodeRuneInString(text[start:])
			if size <= 0 {
				break
			}
			end = min(start+size, len(text))
		}
		windowStart := max(0, start-overlapBytes)
		for windowStart < start && !utf8.RuneStart(text[windowStart]) {
			windowStart++
		}
		appendViews(e.encodedWindowCurrentUserPrecheckViews(text[windowStart:end], windowStart, chunkBytes, overlapBytes, reviewContext))
		start = end
	}

	selectedPatterns := make([]compiledPattern, 0, len(scanner.patterns))
	for _, candidate := range scanner.patterns {
		if candidate.pattern.cfg.SignalOnly && !candidate.pattern.cfg.Strict {
			continue
		}
		if len(candidate.hintClauses) > 0 && !containsDecodedSafetyHintClauseWithMatches("", candidate.hintClauses, matchedHints) {
			continue
		}
		selectedPatterns = append(selectedPatterns, candidate.pattern)
	}
	filtered := *e
	filtered.patterns = selectedPatterns
	filtered.sensitiveWords = nil
	filtered.literalIndex = buildLiteralIndex(selectedPatterns, nil)
	filtered.cfg = e.cfg
	filtered.cfg.MaxTextLength = len(text)
	filtered.cfg.Advanced.Guard.Performance = performance
	return filtered.inspectPreparedScanViews(text, text, allViews, true)
}

// encodedCurrentUserPrecheckViews closes the gap where one encoded token spans
// multiple source windows. Ordinary decoded blocks enter the same final scoring
// pass as other derived views. Oversized blocks contribute only real bounded
// evidence and an incomplete-normalization review signal; quoted non-executing
// fixtures remain excluded by the full-source review context.
func (e *Engine) encodedCurrentUserPrecheckViews(text string, chunkBytes, overlapBytes int) []scanView {
	cfg := e.cfg.Advanced.Normalization
	if !cfg.Enabled || (!cfg.DecodeBase64 && !cfg.DecodeHex) {
		return nil
	}
	scanner := e.exactPrecheckScanner
	patterns := make([]decodedSafetyPriorityPattern, 0, len(scanner.patterns))
	for _, candidate := range scanner.patterns {
		if candidate.pattern.cfg.SignalOnly && !candidate.pattern.cfg.Strict {
			continue
		}
		// A literal-free custom expression cannot be admitted to an unbounded
		// decoded probe without reintroducing the same CPU cliff. It remains fully
		// active in every ordinary and window-sized normalization view.
		if len(candidate.hintClauses) > 0 {
			patterns = append(patterns, candidate)
		}
	}
	if len(patterns) == 0 {
		return nil
	}
	scanner.patterns = patterns
	scanner.sensitiveWords = nil
	scanner.hintIndex = buildDecodedSafetyHintIndex(scanner)
	reviewContext := newMinorSafetyScanContext(text)
	views := make([]scanView, 0, cfg.MaxEncodedBlocks+1)
	seen := make(map[string]struct{})
	incomplete := false
	remainingDecodedBytes := MaxGuardCurrentUserBytes
	appendView := func(value string, normalizationIncomplete bool) {
		value = normalizeForScan(value)
		if value == "" {
			incomplete = incomplete || normalizationIncomplete
			return
		}
		if _, exists := seen[value]; exists {
			incomplete = incomplete || normalizationIncomplete
			return
		}
		if len(value) > remainingDecodedBytes {
			incomplete = true
			return
		}
		seen[value] = struct{}{}
		remainingDecodedBytes -= len(value)
		views = append(views, scanView{Text: value, NormalizationIncomplete: normalizationIncomplete})
	}
	type compressedCandidate struct {
		raw []byte
	}
	compressed := make([]compressedCandidate, 0, cfg.MaxEncodedBlocks)
	for _, candidate := range encodedCandidates(text, cfg, min(len(text), MaxGuardCurrentUserBytes)) {
		if encodedCandidateCoveredByCurrentUserWindow(candidate, len(text), chunkBytes, overlapBytes) {
			continue
		}
		if encodedBlockReviewOnlyWithContext(reviewContext, candidate.start, candidate.end) {
			continue
		}
		raw, ok := decodeEncodedCandidateRaw(candidate)
		if !ok {
			continue
		}
		if cfg.DecodeCompression && compressedPayload(raw) {
			compressed = append(compressed, compressedCandidate{raw: raw})
			continue
		}
		value, printable := decodedPayloadText(raw, false, len(raw))
		if !printable {
			continue
		}
		priority, evidence := scanPlainTextPriorityWithWindow(value, chunkBytes, overlapBytes, scanner)
		if priority <= 0 {
			continue
		}
		if len(value) <= cfg.MaxDecodedBytes {
			appendView(value, false)
		} else if strings.TrimSpace(evidence) != "" {
			appendView(evidence, true)
		}
		incomplete = incomplete || len(value) > cfg.MaxDecodedBytes
	}
	compressionBudget := MaxGuardCurrentUserBytes
	for index, candidate := range compressed {
		remainingCandidates := len(compressed) - index
		if compressionBudget <= 0 || remainingCandidates <= 0 {
			incomplete = true
			break
		}
		candidateBudget := compressionBudget / remainingCandidates
		if candidateBudget > 384*1024 {
			candidateBudget = 384 * 1024
		}
		if candidateBudget < 1 {
			candidateBudget = 1
		}
		value, expanded, overflow, decoded := decompressCurrentUserPayloadPrefix(candidate.raw, candidateBudget)
		compressionBudget -= min(expanded, candidateBudget)
		if !decoded {
			continue
		}
		priority, evidence := scanPlainTextPriorityWithWindow(value, chunkBytes, overlapBytes, scanner)
		if !overflow && len(value) <= cfg.MaxDecodedBytes && priority > 0 {
			appendView(value, false)
			continue
		}
		incomplete = true
		if priority > 0 && strings.TrimSpace(evidence) != "" {
			appendView(evidence, true)
		}
	}
	if incomplete {
		views = append(views, scanView{NormalizationIncomplete: true})
	}
	return views
}

func decompressCurrentUserPayloadPrefix(raw []byte, limit int) (value string, expanded int, overflow bool, ok bool) {
	if len(raw) < 2 || limit <= 0 {
		return "", 0, false, false
	}
	reader, opened := openCompressedPayload(raw)
	if !opened {
		return "", 0, false, false
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	expanded = len(decoded)
	if err != nil || len(decoded) == 0 {
		return "", expanded, false, false
	}
	overflow = len(decoded) > limit
	if overflow {
		var valid bool
		decoded, valid = boundedValidUTF8Prefix(decoded, limit)
		if !valid {
			return "", expanded, overflow, false
		}
	}
	if !utf8.Valid(decoded) || !mostlyPrintable(string(decoded)) {
		return "", expanded, overflow, false
	}
	return string(decoded), expanded, overflow, true
}

// boundedValidUTF8Prefix accepts at most one incomplete UTF-8 rune at the
// truncation boundary. It performs at most utf8.UTFMax full validations, so an
// invalid byte in the middle of decompressed input cannot turn a request-path
// check into the quadratic retry loop used by the generic string helper.
func boundedValidUTF8Prefix(value []byte, limit int) ([]byte, bool) {
	if limit <= 0 || len(value) == 0 {
		return nil, false
	}
	end := min(len(value), limit)
	for trim := 0; trim < utf8.UTFMax && end-trim > 0; trim++ {
		candidate := value[:end-trim]
		if utf8.Valid(candidate) {
			return candidate, true
		}
	}
	return nil, false
}

func encodedCandidateCoveredByCurrentUserWindow(candidate encodedCandidate, textBytes, chunkBytes, overlapBytes int) bool {
	if candidate.start < 0 || candidate.end <= candidate.start || candidate.end > textBytes {
		return false
	}
	for start := 0; start < textBytes; {
		end := min(start+chunkBytes, textBytes)
		windowStart := max(0, start-overlapBytes)
		if candidate.start >= windowStart && candidate.end <= end {
			return true
		}
		start = end
	}
	return false
}

func (e *Engine) encodedWindowCurrentUserPrecheckViews(window string, absoluteStart, chunkBytes, overlapBytes int, reviewContext *minorSafetyScanContext) []scanView {
	cfg := e.cfg.Advanced.Normalization
	if !cfg.Enabled || (!cfg.DecodeBase64 && !cfg.DecodeHex) || window == "" {
		return nil
	}
	maxFragmentBytes := min(len(window), MaxGuardCurrentUserBytes)
	candidates := encodedCandidates(window, cfg, maxFragmentBytes)
	if len(candidates) == 0 {
		return nil
	}
	hasActiveCandidate := false
	for _, candidate := range candidates {
		if !encodedBlockReviewOnlyWithContext(reviewContext, absoluteStart+candidate.start, absoluteStart+candidate.end) {
			hasActiveCandidate = true
			break
		}
	}
	remainingBytes := cfg.MaxDecodedBytes
	if remainingBytes <= 0 {
		remainingBytes = 32768
	}
	remainingBlocks := cfg.MaxEncodedBlocks
	if remainingBlocks <= 0 {
		remainingBlocks = 16
	}
	batch := decodeEmbeddedBlockBatchWithWindow(window, cfg, remainingBytes, remainingBlocks, maxFragmentBytes, chunkBytes, overlapBytes, e)
	active := make([]string, 0, len(batch.blocks))
	review := make([]string, 0, len(batch.blocks))
	for _, block := range batch.blocks {
		if encodedBlockReviewOnlyWithContext(reviewContext, absoluteStart+block.start, absoluteStart+block.end) {
			review = append(review, block.value)
		} else {
			active = append(active, block.value)
		}
	}
	views := make([]scanView, 0, 5)
	if len(active) > 0 {
		views = append(views, scanView{Text: normalizeForScan(strings.Join(active, " "))})
	}
	if len(review) > 0 {
		views = append(views, scanView{Text: normalizeForScan(strings.Join(review, " ")), ReviewOnly: true})
	}
	if value := normalizeForScan(batch.activeEvidenceText); value != "" {
		views = append(views, scanView{Text: value, ReviewOnly: !hasActiveCandidate})
	}
	if value := normalizeForScan(batch.reviewEvidenceText); value != "" {
		views = append(views, scanView{Text: value, ReviewOnly: true})
	}
	if batch.activeIncomplete && hasActiveCandidate {
		views = append(views, scanView{NormalizationIncomplete: true})
	}
	return views
}

func fullCurrentUserDerivedViews(text string, primaryView string, cfg NormalizationConfig, scanner decodedSafetyPriorityScanner) []scanView {
	if !cfg.Enabled || text == "" {
		return nil
	}
	const (
		maxViews              = 16
		maxDerivedSources     = maxViews * 2
		maxPriorityEvidence   = 8
		priorityEvidenceBytes = 64 * 1024
	)
	remainingBytes := 4 * MaxGuardCurrentUserBytes
	remainingEvidenceBytes := priorityEvidenceBytes
	seenSources := make(map[string]struct{})
	seenViews := make(map[string]struct{})
	seenEvidence := make(map[string]struct{})
	views := make([]scanView, 0, 6)
	priorityEvidenceViews := 0
	canonicalize := func(value string) string {
		return stripInvisible(boundedNFKC(value, MaxGuardCurrentUserBytes))
	}
	appendPriorityEvidence := func(priority int, evidence string) {
		if priorityEvidenceViews >= maxPriorityEvidence || remainingEvidenceBytes <= 0 {
			return
		}
		if priority <= 0 {
			return
		}
		evidence = normalizeForScan(evidence)
		if evidence == "" {
			return
		}
		if _, exists := seenEvidence[evidence]; exists {
			return
		}
		if len(evidence) > remainingEvidenceBytes {
			return
		}
		seenEvidence[evidence] = struct{}{}
		remainingEvidenceBytes -= len(evidence)
		priorityEvidenceViews++
		views = append(views, scanView{Text: evidence, NormalizationIncomplete: true})
	}
	appendSource := func(value string, frontier *[]string) {
		if len(seenSources) >= maxDerivedSources {
			return
		}
		value = canonicalize(value)
		if value == "" {
			return
		}
		if _, exists := seenSources[value]; exists {
			return
		}
		seenSources[value] = struct{}{}
		*frontier = append(*frontier, value)
		maxEncodedFragmentBytes := min(len(value), MaxGuardCurrentUserBytes)
		viewValue := collapseRecognizedEncodedPayloads(value, cfg, maxEncodedFragmentBytes, nil)
		viewText := normalizeForScan(viewValue)
		if viewText == "" {
			return
		}
		if _, exists := seenViews[viewText]; exists {
			return
		}
		seenViews[viewText] = struct{}{}
		if viewText == primaryView {
			return
		}
		if derivedViewRequiresFullSemantics(viewText, scanner) {
			if len(views) < maxViews && len(viewText) <= remainingBytes {
				remainingBytes -= len(viewText)
				views = append(views, scanView{Text: viewText})
			}
			return
		}
		priority, evidence := scanPlainTextPriorityWithWindow(viewText, MaxGuardScanChunkBytes, DefaultGuardScanOverlapBytes, scanner)
		if priority <= 0 {
			return
		}
		if len(views) >= maxViews || len(viewText) > remainingBytes || len(viewText) > MaxGuardScanChunkBytes {
			// Continue deriving later rounds even when ordinary full-view budget is
			// exhausted, and never admit a large derived view back into the final
			// regexp pass. A small, independently bounded evidence lane preserves a
			// real decision-bearing match without multiplying request-path work by
			// the number of normalization transforms.
			appendPriorityEvidence(priority, evidence)
			return
		}
		remainingBytes -= len(viewText)
		views = append(views, scanView{Text: viewText})
		if len(views) < maxViews && minorSafetyShouldInspectCompact(viewValue) {
			compact := compactForScan(viewText)
			if compact != "" && len(compact) <= remainingBytes {
				if _, exists := seenViews[compact]; !exists {
					seenViews[compact] = struct{}{}
					remainingBytes -= len(compact)
					views = append(views, scanView{Text: compact, Compacted: true})
				}
			}
		}
	}
	frontier := make([]string, 0, 4)
	appendSource(text, &frontier)
	rot13MayMatch := func(source string) bool {
		index := scanner.hintIndex
		if index == nil {
			index = buildDecodedSafetyHintIndex(scanner)
		}
		if index.unhinted {
			return true
		}
		if index.automaton == nil {
			return false
		}
		matched := index.automaton.matchNormalizedROT13Source(source)
		return decodedSafetyPriorityMatchedHintSetCanMatch(scanner, matched)
	}
	for run := 0; run < cfg.MaxDecodeRuns && len(frontier) > 0 && len(seenSources) < maxDerivedSources; run++ {
		next := make([]string, 0, len(frontier)*2)
		for _, source := range frontier {
			transformSource := collapseRecognizedEncodedPayloads(source, cfg, min(len(source), MaxGuardCurrentUserBytes), nil)
			if cfg.DecodeROT13 && rot13MayMatch(transformSource) {
				if decoded, changed := decodeROT13Text(transformSource); changed {
					appendSource(decoded, &next)
				}
			}
			if cfg.DecodeURL {
				if decoded, err := url.QueryUnescape(transformSource); err == nil && decoded != transformSource {
					appendSource(decoded, &next)
				}
			}
			if cfg.DecodeHTML {
				if decoded := html.UnescapeString(transformSource); decoded != transformSource {
					appendSource(decoded, &next)
				}
			}
			if cfg.DecodeEscapes {
				if decoded, changed := decodeEscapedText(transformSource); changed {
					appendSource(decoded, &next)
				}
			}
		}
		frontier = next
	}
	return views
}

// derivedViewRequiresFullSemantics identifies configured rules whose result
// cannot be reproduced from a local evidence excerpt. Exclusions and
// multi-expression rules must see the same complete derived view that exposed
// the positive candidate; otherwise a distant exclusion can disappear and
// turn an allow into a block. The positive check intentionally ignores only
// exclusions so both an active match and an excluded match retain full context.
func derivedViewRequiresFullSemantics(text string, scanner decodedSafetyPriorityScanner) bool {
	matchedHints, _ := decodedSafetyPriorityMatchedHints(text, scanner)
	for _, candidate := range scanner.patterns {
		pattern := candidate.pattern
		if len(pattern.exclude) == 0 && len(pattern.all) == 0 && len(pattern.any) == 0 {
			continue
		}
		if len(candidate.hintClauses) > 0 && !containsDecodedSafetyHintClauseWithMatches("", candidate.hintClauses, matchedHints) {
			continue
		}
		if !patternShouldRun(text, pattern, nil) {
			continue
		}
		positive := pattern
		positive.exclude = nil
		if compiledPatternMatchIndex(text, positive) != nil {
			return true
		}
	}
	return false
}

func exactPrecheckNeedsDerivedViews(text string, cfg NormalizationConfig, scanner decodedSafetyPriorityScanner) bool {
	if !cfg.Enabled || text == "" {
		return false
	}
	for index := 0; index < len(text); index++ {
		if text[index] >= utf8.RuneSelf {
			// NFKC and invisible-character removal can change any non-ASCII source.
			return true
		}
	}
	if cfg.DecodeURL && strings.ContainsAny(text, "%+") {
		return true
	}
	if cfg.DecodeHTML && strings.Contains(text, "&") {
		return true
	}
	if cfg.DecodeEscapes && strings.Contains(text, "\\") {
		return true
	}
	if (cfg.DecodeBase64 || cfg.DecodeHex) && len(encodedCandidates(text, cfg, min(len(text), MaxGuardCurrentUserBytes))) > 0 {
		return true
	}
	if !cfg.DecodeROT13 {
		return false
	}
	index := scanner.hintIndex
	if index == nil {
		index = buildDecodedSafetyHintIndex(scanner)
	}
	if index.unhinted {
		return true
	}
	matched := index.automaton.matchNormalizedROT13Source(text)
	return decodedSafetyPriorityMatchedHintSetCanMatch(scanner, matched)
}

func limitScanTextExact(text string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxTextLength
	}
	if len(text) <= maxBytes {
		return text
	}
	if maxBytes == 1 {
		return safeUTF8Prefix(text, 1)
	}
	payloadBytes := maxBytes - 1
	headBytes := payloadBytes * 4 / 5
	tailBytes := payloadBytes - headBytes
	return safeUTF8Prefix(text, headBytes) + "\n" + safeUTF8Suffix(text, tailBytes)
}

// boundedEnforcementScanViews keeps the original, already-bounded request view
// and gives all normalization-derived views one shared synchronous byte
// budget. Decoding may discover more material for audit purposes, but content
// beyond this prefix budget cannot become a blocking signal on the request
// path. This is intentionally prefix-based: head/tail sampling is useful for
// raw request coverage, while applying it after decompression would promote an
// arbitrarily distant decoded tail into synchronous enforcement.
func boundedEnforcementScanViews(views []scanView, maxBytes int) []scanView {
	if len(views) <= 1 {
		return views
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxTextLength
	}
	out := make([]scanView, 0, len(views))
	out = append(out, views[0])
	remaining := maxBytes
	for _, view := range views[1:] {
		if remaining <= 0 {
			break
		}
		view.Text = safeUTF8Prefix(view.Text, remaining)
		if view.Text == "" {
			continue
		}
		remaining -= len(view.Text)
		out = append(out, view)
	}
	return out
}

var (
	quotedPolicyReviewContextPattern          = regexp.MustCompile(`(?i)\b(?:classify|categorize|label|analy[sz]e|review|inspect|evaluate|detect|scan|test)\b.{0,120}\b(?:quoted|phrase|prompt[-\s]?injection|policy|unsafe)\b|(?:分类|归类|标注|分析|审查|审核|复核|检查|评估|识别|检测|扫描|测试).{0,80}(?:引用|引号|恶意句子|恶意提示词|提示词注入|安全策略|违规)|(?:需要|用于).{0,20}(?:检测|识别|分类|审查|审核|复核).{0,24}(?:恶意句子|恶意提示词|破限提示词)`)
	quotedPolicyNonExecutionPattern           = regexp.MustCompile(`(?i)\b(?:do\s+not|never)\s+(?:follow|obey|execute|apply)\b|(?:不要|不得|禁止|避免|不).{0,12}(?:遵循|服从|执行|应用)`)
	quotedPolicyReactivationPattern           = regexp.MustCompile(`(?i)\b(?:then|now|but|instead)\b.{0,24}\b(?:follow|obey|execute|apply)\b|\b(?:then|now|but|instead)\b.{0,20}\b(?:build|create|generate|make|provide|give|implement|develop|produce|design|use|do|perform|carry\s+out)\b.{0,16}\b(?:it|that|one|the\s+quoted\s+(?:request|sentence|content)|the\s+(?:request|sentence|content)\s+above)\b|(?:然后|现在|但是|改为).{0,16}(?:遵循|服从|执行|应用)|(?:然后|现在|但是|改为).{0,12}(?:生成|制作|做出|做出来|创建|构造|提供|实现|开发|执行|照做|完成).{0,10}(?:它|这个|该内容|上述内容|上述请求|引用内容)|(?:然后|现在|但是|改为).{0,8}(?:把)?(?:它|这个|该内容|上述内容|上述请求|引用内容).{0,8}(?:生成|制作|做出|做出来|创建|构造|提供|实现|开发|执行|完成)`)
	defensiveRuleArtifactPattern              = regexp.MustCompile(`(?i)\b(?:write|create|generate|build|design)\b.{0,56}\b(?:yara|sigma)\b.{0,72}\b(?:rule|signature|concept|detect(?:ion|or)?|alert)\b`)
	defensiveRuleContinuationPattern          = regexp.MustCompile(`(?i)(?:[,;.!?]\s*|\b(?:then|next|but|also|after\s+that)\s+)\b(?:write|create|build|generate|execute|run|deploy|provide|give|show|craft|make|spawn)\b.{0,80}\b(?:reverse[-\s]?shell|ransomware|infostealer|keylogger|credential\s+stealer)\b|\b(?:then|next|but|also|after\s+that)\b.{0,16}\b(?:actually\s+)?(?:write|create|build|generate|execute|run|deploy|provide|give|show|craft|make|spawn)\b.{0,24}\b(?:it|one|that|the\s+(?:payload|sample|tool|code|script))\b|(?:然后|接着|但是|再).{0,12}(?:实际|直接|立即)?(?:编写|生成|创建|构造|制作|执行|运行|部署).{0,20}(?:它|这个|该载荷|该样本|代码|脚本)`)
	explicitStrictNegationPattern             = regexp.MustCompile(`(?i)\b(?:do\s+not|don't|never|avoid|must\s+not|should\s+not|refuse\s+to)\s+(?:write|create|build|generate|execute|run|deploy|provide|give|show|craft|make|spawn)\b|(?:不要|不得|禁止|避免|不应|不)(?:再)?(?:编写|生成|创建|构造|制作|执行|运行|部署)`)
	defensiveDocumentationContextPattern      = regexp.MustCompile(`(?i)\b(?:policy|policies|rule|guideline|standard|moderation|safety|security|detector|classifier|filter|audit|incident\s+report|analysis|documentation|test\s+(?:fixture|case)|example|sample)\b|(?:政策|策略|规则|准则|标准|审核|安全|检测器|分类器|过滤器|审计|事件报告|分析报告|文档|测试(?:夹具|用例)|示例|样例)`)
	defensiveDocumentationTreatmentPattern    = regexp.MustCompile(`(?i)\b(?:blocks?|reject(?:s|ed|ing)?|den(?:y|ies|ied|ying)|forbid(?:s|den|ding)?|prohibit(?:s|ed|ing)?|disallow(?:s|ed|ing)?|exclude(?:s|d|ing)?|detect(?:s|ed|ing)?|flags?|classif(?:y|ies|ied|ying)|redact(?:s|ed|ing)?|prevent(?:s|ed|ing)?|mitigate(?:s|d|ing)?|reported?|observed|attempted?|unsafe|malicious)\b|(?:拦截|拒绝|禁止|不允许|排除|检测|识别|标记|分类|脱敏|预防|缓解|观察到|尝试|不安全|恶意)`)
	defensiveDocumentationReactivationPattern = regexp.MustCompile(`(?i)\b(?:ignore|bypass|override|disregard)\b.{0,48}\b(?:policy|rule|guideline|detector|filter|warning)\b|\b(?:then|now|instead|actually|but)\b.{0,40}\b(?:reveal|show|dump|read|build|deploy|run|create|set\s*up|teach|provide|give)\b|(?:忽略|绕过|覆盖|无视).{0,30}(?:政策|规则|准则|检测|过滤|警告)|(?:然后|现在|改为|实际|但是).{0,24}(?:展示|泄露|输出|读取|建立|创建|搭建|部署|运行|教我|提供|给我)`)
	selfHarmProtectiveDocumentationPattern    = regexp.MustCompile(`(?i)\b(?:do\s+not|don't|never|must\s+not|should\s+not|avoid|refuse\s+to)\b.{0,24}\b(?:name|list|describe|provide|give|recommend|suggest|teach)\b.{0,60}\b(?:methods?|steps?|instructions?|ways?|suicide|self[-\s]?harm|cutting)\b|\b(?:methods?|steps?|instructions?|ways?)\b.{0,40}\b(?:must\s+not|should\s+not|cannot|can't|are\s+not\s+to\s+be)\b.{0,32}\b(?:provided|listed|described|recommended|taught)\b|(?:不要|不得|禁止|不能|不应|切勿|避免|不)(?:再)?(?:命名|列出|描述|提供|教授|说明|推荐|建议).{0,32}(?:方法|步骤|方式|替代|自杀|自残|自我伤害)|(?:方法|步骤|方式|替代).{0,32}(?:不要|不得|禁止|不能|不应|切勿|避免|不建议).{0,32}(?:自杀|自残|自我伤害)`)
	selfHarmBoundPreventionPattern            = regexp.MustCompile(`(?i)\b(?:how\s+to|ways?\s+to|methods?\s+to|steps?\s+to)\s+(?:prevent|avoid|reduce|mitigate|intervene|screen\s+for|treat)\b[^.!?;\n]{0,24}\b` + selfHarmTargetEN + `\b|\b(?:methods?|steps?|strategies?|resources?)\b[^.!?;\n]{0,24}\b(?:to|for)\s+(?:prevent(?:ing)?|avoid(?:ing)?|reduc(?:e|ing)|mitigat(?:e|ing)|interven(?:e|ing)|screen(?:ing)?\s+for|treat(?:ing)?)\b[^.!?;\n]{0,24}\b` + selfHarmTargetEN + `\b|\b(?:methods?|steps?|strategies?|resources?)\b[^.!?;\n]{0,20}\bfor\s+` + selfHarmTargetEN + `\s+(?:prevention|intervention|support|risk\s+reduction)\b|\b(?:prevention|intervention|coping|support|crisis|harm[-\s]?reduction)\s+(?:methods?|steps?|strategies?|resources?)\b[^.!?;\n]{0,24}\b(?:for\s+)?` + selfHarmTargetEN + `\b|\b` + selfHarmTargetEN + `\s+(?:prevention|intervention|support|risk\s+reduction)\s+(?:methods?|strategies?|resources?)\b|(?:如何|怎么)(?:预防|防止|避免|降低|减少|缓解|干预|筛查|治疗)` + selfHarmTargetZH + `|(?:方法|步骤|策略|资源)(?:来|用于|以便)?(?:预防|防止|避免|降低|减少|缓解|干预|筛查|治疗)` + selfHarmTargetZH + `|(?:方法|步骤|策略|资源)[^。！？；\n]{0,12}(?:用于|关于)?` + selfHarmTargetZH + `(?:预防|干预|支持|风险降低)|(?:预防|防止|避免|降低|减少|缓解|干预|筛查|治疗)` + selfHarmTargetZH + `(?:的)?(?:具体|有效|循证)?(?:方法|步骤|策略|资源)|` + selfHarmTargetZH + `(?:预防|干预|支持|风险降低)(?:方法|策略|资源)`)
	selfHarmResearchMethodsPattern            = regexp.MustCompile(`(?i)\b(?:statistical|research|clinical|epidemiological|intervention)\s+(?:analysis\s+)?methods?\b|(?:统计|研究|临床|流行病学|干预)(?:分析)?方法`)
	selfHarmResearchOutcomePattern            = regexp.MustCompile(`(?i)\b` + selfHarmTargetEN + `\b[^.!?;\n]{0,24}\b(?:mortality|status|risk|rates?|prevalence|incidence|outcomes?|results?|ideation)\b|\b(?:outcomes?|results?|endpoints?|measures?|mortality|status|rates?|risk)\b[^.!?;\n]{0,24}\b(?:(?:is|was|are|were)\s+)?` + selfHarmTargetEN + `\b|` + selfHarmTargetZH + `(?:率|死亡率|风险|状态|结局|结果|意念)|(?:结局|结果|指标|死亡率|状态|风险)[^。！？；\n]{0,20}(?:是|为)?` + selfHarmTargetZH + ``)
	selfHarmResearchCausationPattern          = regexp.MustCompile(`(?i)\b(?:ensure|guarantee|cause|induce|trigger|facilitate|promote|bring\s+about|lead\s+to|result\s+in|make)\b|(?:确保|导致|造成|诱发|促成|使[^。！？；\n]{0,16}(?:成为|变成|达到))`)
	selfHarmPreventionCancellationPattern     = regexp.MustCompile(`(?i)\b(?:ignore|reject|skip|without|instead\s+of|not)\b[^.!?;\n]{0,24}\b(?:prevention|support|coping|intervention|safety)\b|(?:忽略|拒绝|跳过|不要|无需|不做|而不是)[^。！？；\n]{0,20}(?:预防|支持|干预|安全|求助)`)
	selfHarmDocumentationReactivationPattern  = regexp.MustCompile(selfHarmReactivationPattern)
	negatedPolicyActionProtectiveBoundPattern = regexp.MustCompile(`(?i)^(?:(?:\b(?:prevent|stop)\b.{0,48}\b(?:from|attempts?\s+to|requests?\s+to)\b)|(?:\b(?:block|detect)\b.{0,48}\b(?:attempts?|requests?|efforts?|users?|the\s+(?:model|assistant))\b.{0,24}\b(?:to|from)\b)|(?:\b(?:test|verify)\b.{0,40}\b(?:whether|that|attempts?|requests?)\b)|(?:防止|阻止|拦截|检测|测试|验证).{0,48}).{0,16}(?:\b(?:bypass|evade|disable|ignore|remove|reveal|show|dump|read|print)\b|(?:绕过|规避|关闭|忽略|移除|泄露|展示|输出|读取|查看))$`)
	negatedPolicyActionCancellationPattern    = regexp.MustCompile(`(?i)\b(?:do\s+not|don't|never|must\s+not|should\s+not|cannot|can't)\b.{0,32}\b(?:prevent|block|stop|detect)\b|(?:不要|不得|禁止|不能|不应|切勿).{0,24}(?:防止|阻止|拦截|检测)`)
	negatedPolicyActionDirectCancellation     = regexp.MustCompile(`(?i)^\s*(?:please\s+)?(?:refuse|decline|fail|hesitate|avoid)\b.{0,32}$|^\s*(?:拒绝|避免|犹豫|迟疑|不敢|未能).{0,16}$`)
	negatedPolicyActionSharedBridgePattern    = regexp.MustCompile(`(?i)^\s*(?:(?:bypass|evade|disable|ignore|remove|reveal|show|dump|read|print)\s*(?:or|/)\s*)+$|^\s*(?:(?:绕过|规避|关闭|忽略|移除|泄露|展示|输出|读取|查看)\s*(?:或|或者|/)\s*)+$`)
	protectiveWarningSuppressionPattern       = regexp.MustCompile(`(?i)^\s*no\s+(?:warning|disclaimer|refusal)s?\s+(?:suppression|removal|bypass)\b|^\s*(?:禁止|不允许|不要|不得).{0,8}(?:警告|拒绝|免责声明).{0,8}(?:抑制|移除|删除)\b`)
	protectiveRefusalSuppressionPattern       = regexp.MustCompile(`(?i)\b(?:never|do\s+not|don't|must\s+not|should\s+not)\s+(?:refuse|decline|fail|hesitate|avoid)\b.{0,32}\b(?:block|prevent|stop|detect|reject|deny)\b.{0,64}\b(?:attempts?|requests?|efforts?|users?)\b.{0,48}\b(?:bypass|evade|disable|ignore|remove|reveal|show|dump|read|print)\b|(?:不要|不得|不能|切勿|不应).{0,12}(?:拒绝|遗漏|回避|避免|犹豫).{0,16}(?:拦截|阻止|防止|检测|拒绝).{0,48}(?:泄露|绕过|规避|关闭|忽略|移除).{0,24}(?:系统提示词|开发者消息|安全规则|安全策略|安全过滤|安全校验|内容审核)`)
	narrativeRefusalContextPattern            = regexp.MustCompile(`(?i)\b(?:character|story|novel|plot|dialogue|quote|lyrics?|fiction|scene|relationship|he\s+said|she\s+said)\b|(?:角色|人物|剧情|小说|故事|台词|对白|引语|歌词|动漫|动画|电影|场景|关系|状态|他说|她说|它说|喜羊羊|猫化)`)
	refusalPolicyContextPattern               = regexp.MustCompile(`(?i)\b(?:assistant|model|chatbot|llm|system|developer|prompt|instruction|policy|safety|moderation|filter|refusal|response|reply)\b|(?:助手|模型|聊天机器人|系统|开发者|提示词|指令|规则|政策|安全|审核|过滤|拒答|回答|回复)`)
	refusalDirectiveContextPattern            = regexp.MustCompile(`(?i)(?:^|[.!?;:。！？；：\n]\s*)(?:please|you\s+must|must|always|never|do\s+not|don't|no\s+(?:warnings?|refusals?|disclaimers?)|请|请你|你必须|必须|务必|始终|永远|不要|不得|禁止|不允许)`)
)

var (
	authorizationBoundaryAccessMatchPattern   = regexp.MustCompile(`(?i)\baccess\b|访问`)
	authorizationBoundaryHighRiskMatchPattern = regexp.MustCompile(`(?i)\b(?:exploit|compromise|take\s*over|takeover)\b|(?:利用|入侵|接管)`)
	authorizationBoundaryRequestCuePattern    = regexp.MustCompile(
		`(?i)(?:^|[.!?;:。！？；：\n]\s*)(?:(?:please|then|next|now|directly|immediately|help\s+me|i\s+(?:want|need))\b|(?:请|请你|帮我|我要|需要你|然后|接着|直接|立即|马上))`,
	)
	authorizationBoundaryActiveTargetPattern = regexp.MustCompile(
		`(?i)\baccess\s+(?:(?:to\s+)?(?:the|an?|this|that)\s+)?(?:admin(?:istration|istrator)?|console|dashboard|backend|system|host|server|account|target|internal\s+(?:network|service|system))\b|` +
			`访问\s*(?:(?:该|这个|目标|内部|未经授权的?)\s*)?(?:管理后台|后台|管理端|系统|主机|服务器|账户|目标|内部网络|内部服务)`,
	)
	authorizationBoundaryExplicitDenialPattern = regexp.MustCompile(
		`(?i)(?:` +
			`\b(?:unauthorized\s+access|access\s+without\s+(?:permission|authorization)|access[^.!?;:\n]{0,48}\b(?:without\s+permission|with\s+no\s+authorization|unauthorized\s+target)|without\s+(?:permission|authorization)[^.!?;:\n]{0,32}\baccess)\b[^.!?;:\n]{0,64}(?:(?:must|should|shall|will|is|are)\s+(?:be\s+)?(?:denied|blocked|rejected|forbidden|prohibited)\b|(?:must|should|shall)?\s*(?:return|respond\s+with)\s+(?:http\s*)?(?:401|403)\b)|` +
			`(?:未授权|未经授权|未经许可|没有(?:获得|取得|得到)?授权)(?:的)?[^。！？；：.!?;:\n]{0,32}访问[^。！？；：.!?;:\n]{0,64}(?:(?:必须|应当|应该|需要|将|会)?被?(?:拒绝|阻止|禁止|拦截)|(?:必须|应当|应该|需要)?(?:返回|响应)[^。！？；：.!?;:\n]{0,8}(?:401|403))|` +
			`访问[^。！？；：.!?;:\n]{0,48}(?:未授权|未经授权|未经许可|没有(?:获得|取得|得到)?授权)(?:的)?[^。！？；：.!?;:\n]{0,64}(?:(?:必须|应当|应该|需要|将|会)?被?(?:拒绝|阻止|禁止|拦截)|(?:必须|应当|应该|需要)?(?:返回|响应)[^。！？；：.!?;:\n]{0,8}(?:401|403))` +
			`)`,
	)
	authorizationBoundaryRestrictionPattern = regexp.MustCompile(
		`(?i)(?:` +
			`\b(?:unauthorized\s+access|access\s+without\s+(?:permission|authorization)|without\s+(?:permission|authorization)[^.!?;:\n]{0,32}\baccess)\b[^.!?;:\n]{0,96}(?:(?:may\s+)?only[^.!?;:\n]{0,24}\b(?:view|read|browse|access\s+(?:public|read[-\s]?only))\b|(?:cannot|can't|must\s+not|may\s+not|is\s+not\s+allowed\s+to|are\s+not\s+allowed\s+to)[^.!?;:\n]{0,32}\b(?:access|modify|change|delete|execute|trigger|manage|control)\b|(?:must|should|shall|will|is|are)\s+(?:be\s+)?(?:denied|blocked|rejected|forbidden|prohibited|restricted)\b|(?:must|should|shall)\s+(?:return|respond\s+with)\s+(?:http\s*)?(?:401|403)\b)|` +
			`(?:未授权|未经授权|未经许可|没有(?:获得|取得|得到)?授权)(?:的)?[^。！？；：.!?;:\n]{0,96}(?:(?:只能|仅能|仅可)[^。！？；：.!?;:\n]{0,16}(?:浏览|查看|读取|只读|访问(?:公开|只读)?(?:页面|内容|日报|报告|资源)?)|(?:不能|不得|无权|不允许)[^。！？；：.!?;:\n]{0,24}(?:访问|触发|修改|删除|执行|导出|管理|控制)|(?:必须|应当|应该|需要|将|会)?被?(?:拒绝|阻止|禁止|拦截|限制)|(?:必须|应当|应该|需要)(?:返回|响应)[^。！？；：.!?;:\n]{0,8}(?:401|403|拒绝|禁止))` +
			`)`,
	)
	authorizationBoundaryCancellationPattern = regexp.MustCompile(
		`(?i)(?:` +
			`\b(?:unauthorized\s+access|access\s+without\s+(?:permission|authorization)|without\s+(?:permission|authorization)[^.!?;:\n]{0,24}\baccess)\b\s*(?:itself\s*)?(?:must\s+not|should\s+not|cannot|can't|is\s+not|are\s+not|will\s+not)\s+(?:be\s+)?(?:denied|blocked|rejected|forbidden|prohibited|restricted)\b|` +
			`\b(?:do\s+not|don't|never)\s+(?:deny|block|reject|prevent|forbid|prohibit|restrict)\b[^.!?;:\n]{0,40}\b(?:unauthorized\s+access|access\s+without\s+(?:permission|authorization))\b|` +
			`(?:未授权|未经授权|未经许可|没有(?:获得|取得|得到)?授权)(?:的)?[^。！？；：.!?;:\n]{0,24}访问[^。！？；：，,.!?;:\n]{0,32}(?:(?:不应|不能|不得|无需)\s*(?:被)?(?:拒绝|阻止|禁止|拦截|限制)|(?:不是|并非|不再)[^。！？；：，,.!?;:\n]{0,12}(?:只能|仅能|仅可|只读))|` +
			`(?:不要|不得|不能|不应|无需)[^。！？；：.!?;:\n]{0,12}(?:拒绝|阻止|禁止|拦截|限制)[^。！？；：.!?;:\n]{0,32}(?:未授权|未经授权|未经许可|没有(?:获得|取得|得到)?授权)(?:的)?[^。！？；：.!?;:\n]{0,24}访问` +
			`)`,
	)
	authorizationBoundaryReactivationPattern = regexp.MustCompile(
		`(?i)(?:` +
			`(?:^|[.!?;:,\n]\s*|\b(?:then|next|now|instead|but|however|also|and(?:\s+then)?|after\s+that|afterwards|so\s+that|in\s+order\s+to)\s+)(?:(?:please|kindly|directly|immediately|actually|continue\s+to|go\s+ahead\s+and|help(?:\s+me)?(?:\s+to)?|i\s+(?:want|need)(?:\s+you)?\s+to|(?:can|could|would)\s+you)\s+)?(?:ignore|bypass|circumvent|override|disregard)\b[^.!?;:\n]{0,64}\b(?:control|restriction|authorization|permission|policy|access)\b|` +
			`(?:^|[.!?;:,\n]\s*|\b(?:then|next|now|instead|but|however|also|and(?:\s+then)?|after\s+that|afterwards|so\s+that|in\s+order\s+to)\s+)(?:(?:please|kindly|directly|immediately|actually|continue\s+to|go\s+ahead\s+and|help(?:\s+me)?(?:\s+to)?|i\s+(?:want|need)(?:\s+you)?\s+to|(?:can|could|would)\s+you)\s+)?(?:exploit|compromise|take\s*over|access\s+(?:the\s+)?(?:admin|system|host|server|account|target)|(?:modify|change)[^.!?;:\n]{0,16}(?:user|administrator|account)[^.!?;:\n]{0,8}(?:permissions?|roles?)|(?:grant|assign|give)[^.!?;:\n]{0,16}(?:me|user|administrator|account)[^.!?;:\n]{0,16}(?:admin(?:istrator)?|root|elevated)?[^.!?;:\n]{0,8}(?:permissions?|roles?|access)|(?:modify|delete)[^.!?;:\n]{0,12}(?:user\s+data|audit\s+logs?|account\s+records?)|execute\s+commands?|extract|steal|exfiltrate|elevate)\b|` +
			`(?:^|[。！？；：，,\n]\s*|(?:然后|接着|随后|之后|后续|现在|改为|但是|不过|另外|同时|并(?:且)?|再|以便|从而|进而)\s*)(?:(?:请(?:你)?|帮(?:我|忙)?(?:去|来)?|麻烦(?:你)?(?:替我|帮我)?|替我|我要|我需要|需要你|务必|也(?:要|请)?|需要|允许|可以|直接|立即|继续|实际)\s*)?(?:绕过|规避|无视|忽略)[^。！？；：.!?;:\n]{0,40}(?:限制|权限|鉴权|授权|访问控制|规则)|` +
			`(?:^|[。！？；：，,\n]\s*|(?:然后|接着|随后|之后|后续|现在|改为|但是|不过|另外|同时|并(?:且)?|再|以便|从而|进而)\s*)(?:(?:请(?:你)?|帮(?:我|忙)?(?:去|来)?|麻烦(?:你)?(?:替我|帮我)?|替我|我要|我需要|需要你|务必|也(?:要|请)?|需要|允许|可以|直接|立即|继续|实际)\s*)?(?:利用|入侵|接管|控制|绕过|提权|窃取|外传|执行命令|访问[^。！？；：.!?;:\n]{0,16}(?:后台|系统|主机|服务器|账户|目标|管理端)|修改[^。！？；：.!?;:\n]{0,12}(?:用户|管理员|账户)(?:的)?(?:权限|角色)|(?:把|将)?[^。！？；：.!?;:\n]{0,8}(?:用户|账户|账号|我)(?:的)?(?:权限|角色)[^。！？；：.!?;:\n]{0,8}(?:改成|改为|设置为|提升为|变更为)[^。！？；：.!?;:\n]{0,8}(?:管理员|最高权限|超级用户|root)|(?:授予|赋予|给)[^。！？；：.!?;:\n]{0,8}(?:我|用户|账户|账号)[^。！？；：.!?;:\n]{0,8}(?:管理员|最高|超级用户|root)?(?:权限|角色|访问权)|(?:删除|导出)[^。！？；：.!?;:\n]{0,12}(?:用户数据|数据|审计日志|安全日志|账户))` +
			`)`,
	)
	authorizationBoundaryReactivationActionPattern = regexp.MustCompile(
		`(?i)(?:` +
			`\b(?:ignore|bypass|circumvent|override|disregard)\b[^.!?;:\n]{0,64}\b(?:control|restriction|authorization|permission|policy|access)\b|` +
			`\b(?:exploit|compromise|take\s*over|access\s+(?:the\s+)?(?:admin|system|host|server|account|target)|(?:modify|change)[^.!?;:\n]{0,16}(?:user|administrator|account)[^.!?;:\n]{0,8}(?:permissions?|roles?)|(?:grant|assign|give)[^.!?;:\n]{0,16}(?:me|user|administrator|account)[^.!?;:\n]{0,16}(?:admin(?:istrator)?|root|elevated)?[^.!?;:\n]{0,8}(?:permissions?|roles?|access)|(?:modify|delete)[^.!?;:\n]{0,12}(?:user\s+data|audit\s+logs?|account\s+records?)|execute\s+commands?|extract|steal|exfiltrate|elevate)\b|` +
			`(?:绕过|规避|无视|忽略)[^。！？；：.!?;:\n]{0,40}(?:限制|权限|鉴权|授权|访问控制|规则)|` +
			`(?:利用|入侵|接管|控制|绕过|提权|窃取|外传|执行命令|访问[^。！？；：.!?;:\n]{0,16}(?:后台|系统|主机|服务器|账户|目标|管理端)|修改[^。！？；：.!?;:\n]{0,12}(?:用户|管理员|账户)(?:的)?(?:权限|角色)|(?:把|将)?[^。！？；：.!?;:\n]{0,8}(?:用户|账户|账号|我)(?:的)?(?:权限|角色)[^。！？；：.!?;:\n]{0,8}(?:改成|改为|设置为|提升为|变更为)[^。！？；：.!?;:\n]{0,8}(?:管理员|最高权限|超级用户|root)|(?:授予|赋予|给)[^。！？；：.!?;:\n]{0,8}(?:我|用户|账户|账号)[^。！？；：.!?;:\n]{0,8}(?:管理员|最高|超级用户|root)?(?:权限|角色|访问权)|(?:删除|导出)[^。！？；：.!?;:\n]{0,12}(?:用户数据|数据|审计日志|安全日志|账户))` +
			`)`,
	)
	authorizationBoundaryEngineeringTailPattern = regexp.MustCompile(
		`(?i)^\s*(?:校验|验证|检查|判定|鉴权|授权检查|逻辑|代码|测试|模块|中间件|接口|api|策略|规则|流程|实现|函数|方法|服务|文档|说明|\b(?:validation|verification|check|checking|logic|code|tests?|module|middleware|api|endpoint|handler|policy|rule|flow|implementation|function|method|service|documentation)\b)`,
	)
	authorizationBoundaryPrivilegeOutcomePattern = regexp.MustCompile(
		`(?i)(?:\b(?:(?:by\s+)?default(?:ly)?|automatically|ultimately)?\s*(?:grant|give|assign|set|make|elevate)[^.!?;:\n]{0,24}(?:me|user|administrator|account)[^.!?;:\n]{0,16}(?:admin(?:istrator)?|root|elevated)(?:\s+(?:permissions?|roles?|access))?\b|(?:(?:默认|自动|最终)\s*)?(?:给|授予|赋予|设置|设为|改为|提升为)[^。！？；：.!?;:\n]{0,16}(?:我|用户|账户|账号|自己)[^。！？；：.!?;:\n]{0,12}(?:管理员|最高权限|超级用户|root)(?:权限|角色|访问权)?)`,
	)
	authorizationBoundaryDirectPrivilegeRequestPattern = regexp.MustCompile(
		`(?i)(?:(?:^|[.!?;:,\n]\s*|\b(?:then|next|now|instead|but|however|also|and(?:\s+then)?|after\s+that|afterwards)\s+)(?:(?:please|kindly|directly|immediately|actually|help(?:\s+me)?(?:\s+to)?|i\s+(?:want|need)(?:\s+you)?\s+to|(?:can|could|would)\s+you|(?:by\s+)?default(?:ly)?|automatically|ultimately)\s+)?(?:grant|give|assign|set|make|elevate)[^.!?;:\n]{0,24}(?:me|user|administrator|account)[^.!?;:\n]{0,16}(?:admin(?:istrator)?|root|elevated)(?:\s+(?:permissions?|roles?|access))?\b|(?:^|[。！？；：，,\n]\s*|(?:然后|接着|随后|之后|后续|现在|改为|但是|不过|另外|同时|并(?:且)?|再)\s*)(?:(?:请(?:你)?|帮(?:我|忙)?|麻烦(?:你)?(?:替我|帮我)?|替我|我要|我需要|需要你|务必|默认|自动|最终|直接|立即)\s*)?(?:给|授予|赋予|设置|设为|改为|提升为)[^。！？；：.!?;:\n]{0,16}(?:我|用户|账户|账号|自己)[^。！？；：.!?;:\n]{0,12}(?:管理员|最高权限|超级用户|root)(?:权限|角色|访问权)?)`,
	)
	authorizationBoundaryReactivationCancellationTailPattern = regexp.MustCompile(
		`(?i)^\s*[,，]?\s*(?:(?:the\s+)?(?:request|attempt|action|operation)\s+)?(?:(?:must|should|shall|will|is|are)?\s*(?:not|never)\s+(?:be\s+)?(?:denied|blocked|rejected|forbidden|prohibited|restricted)\b|(?:cannot|can't|need\s+not)\s+(?:be\s+)?(?:denied|blocked|rejected|forbidden|prohibited|restricted)\b|(?:的)?(?:请求|操作|行为|尝试|动作)?\s*(?:无需|不需要|没必要|不必|不应|不能|不得|不要)\s*(?:被)?(?:拒绝|阻止|禁止|拦截|限制))`,
	)
	authorizationBoundaryReactivationDeniedPattern = regexp.MustCompile(
		`(?i)^\s*[,，]?\s*(?:(?:the\s+)?(?:request|attempt|action|operation)\s+|(?:的)?(?:请求|操作|行为|尝试|动作)\s*)?(?:(?:(?:must|should|shall|will|is|are)\s+(?:be\s+)?(?:denied|blocked|rejected|forbidden|prohibited|restricted)\b)|(?:(?:must|should|shall)\s+(?:return|respond\s+with)\s+(?:http\s*)?(?:401|403)\b)|(?:(?:may\s+)?only[^.!?;:\n]{0,24}\b(?:view|read|browse)\b)|(?:(?:cannot|can't|must\s+not|may\s+not|is\s+not\s+allowed\s+to|are\s+not\s+allowed\s+to)[^.!?;:\n]{0,32}\b(?:access|modify|change|delete|execute|trigger|manage|control)\b)|(?:(?:(?:必须|应当|应该|需要|将|会)\s*(?:被)?|被)(?:拒绝|阻止|禁止|拦截|限制))|(?:(?:必须|应当|应该|需要)?\s*(?:返回|响应)[^。！？；：.!?;:\n]{0,8}(?:401|403))|(?:(?:只能|仅能|仅可)[^。！？；：.!?;:\n]{0,16}(?:浏览|查看|读取|只读))|(?:(?:不能|不得|无权|不允许)[^。！？；：.!?;:\n]{0,24}(?:访问|触发|修改|删除|执行|导出|管理|控制)))`,
	)
)

var (
	benignPersistenceDocumentationPattern = regexp.MustCompile(`(?i)(?:\b(?:incident|audit|forensic|malware|attack|attacker|sample|detector|report)\b.{0,96}\b(?:persistence|c2|command\s+and\s+control|botnet|backdoor)\b|\b(?:persistence|c2|command\s+and\s+control|botnet|backdoor)\b.{0,96}\b(?:detected|blocked|prevented|reported|observed|attempted|indicator|evidence)\b)|(?:事件|审计|取证|恶意软件|攻击者|样本|检测器|报告).{0,64}(?:持久化|长期驻留|c2|命令与控制|僵尸网络|后门)|(?:持久化|长期驻留|c2|命令与控制|僵尸网络|后门).{0,64}(?:检测到|拦截|阻止|报告|观察到|尝试|指标|证据)`)
	minorSafetyAgeHintPattern             = regexp.MustCompile(`(?i)(?:^|[^0-9])(?:[0-9]|1[0-7])\s*(?:[- ]?\s*year\s*[- ]?\s*old|岁)`)
	minorSafetyDecodedPriorityPattern     = regexp.MustCompile(minorExploitationPattern)
	minorSafetySentenceBoundary           = regexp.MustCompile(`[!?。！？]|\.(?:\s|$)|\n`)
	minorSafetySafeArtifactTail           = regexp.MustCompile(`(?i)^\s*(?:(?:(?:detection|classification|moderation|policy|incident|audit)\s+reports?|detectors?|classifiers?|hash(?:es)?(?:\s+lists?)?|signatures?|scanners?|filters?|moderation\s+rules?|policy\s+rules?|benchmarks?|fixtures?|research\s+(?:papers?|resources?)|policy\s+resources?|legal\s+resources?|statistics)\b|(?:检测报告|识别报告|分类报告|审核报告|治理报告|事件报告|检测器|识别器|分类器|检测模型|识别模型|分类模型|审核模型|治理模型|哈希列表|摘要列表|签名|扫描器|过滤器|审核规则|治理规则|策略规则|检测基准|测试夹具|研究论文|研究资料|政策资料|法律资料|统计数据))`)
	minorSafetyUnsafeArtifactTail         = regexp.MustCompile(`(?i)(?:\b(?:contain(?:s|ing)?|include(?:s|d|ing)?|attach(?:es|ed|ing)?|embed(?:s|ded|ding)?|bundle(?:s|d|ing)?|with|from|using|built\s+from|trained\s+on)\b.{0,48}\b(?:(?:actual|real|raw|original|explicit|illegal)\s+)?(?:csam|child\s+sexual\s+abuse\s+material|files?|images?|photos?|videos?|content|material|samples?|training\s+corpus)\b|\blinks?\s+to\b.{0,32}\b(?:the\s+)?(?:files?|images?|photos?|videos?|content|material)\b|\b(?:send|share|upload|publish|distribute|retrieve|fetch|download)\b.{0,32}\b(?:the\s+)?(?:csam|material|files?|images?|photos?|videos?|content|samples?)\b|(?:包含|附带|嵌入|打包|带有|来自|使用|基于).{0,32}(?:真实|原始|露骨|非法)?(?:儿童性虐待材料|儿童色情|文件|图像|影像|视频|内容|材料|样本|训练语料)|(?:发送|分享|上传|发布|传播|检索|获取|下载).{0,24}(?:儿童性虐待材料|儿童色情|材料|文件|图像|影像|视频|内容|样本))`)
	minorSafetyUnsafeArtifactAnaphora     = regexp.MustCompile(`(?i)(?:\b(?:it|this|that|the\s+(?:report|classifier|detector|benchmark|fixture))\b.{0,40}\b(?:should|must|will|needs?\s+to|is\s+to)?\s*(?:contain|include|attach|embed|bundle|use)\b.{0,48}\b(?:(?:actual|real|raw|original|explicit|illegal)\s+)?(?:csam|child\s+sexual\s+abuse\s+material|files?|images?|photos?|videos?|content|material|samples?|training\s+corpus)\b|\btrain\b.{0,32}\b(?:it|this|that|the\s+(?:classifier|detector|model))\b.{0,24}\bon\b.{0,32}\b(?:(?:actual|real|raw|original|illegal)\s+)?(?:csam|child\s+sexual\s+abuse\s+material|files?|images?|content|material|samples?|training\s+corpus)\b|\b(?:next|then|afterwards|in\s+the\s+next\s+sentence|please)\b.{0,24}\b(?:contain|include|attach|embed|bundle|use)\b.{0,40}\b(?:(?:actual|real|raw|original|illegal)\s+)?(?:csam|child\s+sexual\s+abuse\s+material)\b|(?:它|该报告|这个报告|该分类器|这个分类器|该检测器|这个检测器).{0,24}(?:应|必须|需要|将)?(?:包含|附带|附上|嵌入|打包|使用).{0,32}(?:真实|原始|露骨|非法)?(?:儿童性虐待材料|儿童色情|文件|图像|影像|视频|内容|材料|样本)|(?:训练|微调).{0,16}(?:它|该分类器|这个分类器|该检测器|这个检测器).{0,20}(?:使用|基于|采用).{0,24}(?:真实|原始|非法)?(?:儿童性虐待材料|儿童色情|文件|图像|内容|材料|样本)|(?:下一句|然后|接着|随后|请|请你).{0,16}(?:包含|附带|附上|加入|嵌入|打包|使用).{0,24}(?:真实|原始|非法)?(?:儿童性虐待材料|儿童色情))`)
	minorSafetySafeVisualArtifactContext  = regexp.MustCompile(`(?i)\b(?:actual|real|raw|original)\s+(?:images?|photos?|videos?|screenshots?)\s+(?:of|from|showing|depicting)\s+(?:the\s+)?(?:dashboard|ui|user\s+interface|interface|application|app|website|web\s+page|product|mockup|design|chart|graph|screen)\b|(?:真实|实际|原始)(?:图像|图片|截图|视频).{0,16}(?:仪表盘|界面|用户界面|应用|网站|网页|产品|原型|设计稿|图表|屏幕)`)
	minorSafetyIllegalMaterialHit         = regexp.MustCompile(`(?i)\b(?:csam|child\s+sexual\s+abuse\s+material|child\s+porn(?:ography)?|child\s+pornographic\s+(?:content|material|images?|videos?))\b|(?:儿童性虐待材料|儿童色情|未成年人?(?:色情|露骨|性虐待)|(?:未成年人|儿童|孩子|小孩|小学生|幼童|青少年)裸照)`)
	minorSafetyMediaCandidate             = regexp.MustCompile(`(?i)\b(?:images?|photos?|videos?|content|material)\s+(?:of|involving|featuring|depicting)\s+(?:an?\s+|the\s+)?(?:(?:nude|sexualized|sexually\s+exploited|pornographic)\s+)?(?:children|child|minors?|kids?|teens?)\s*$`)
	minorSafetyEngineeringEntityTail      = regexp.MustCompile(`(?i)^\s+(?:tasks?|process(?:es)?|versions?|releases?|components?|nodes?|dialogs?|windows?|records?|classes?|modules?|branches?|elements?|workers?|threads?|objects?|fixtures?|compatibility)\b`)
	minorSafetyAmbiguousTargetSuffix      = regexp.MustCompile(`(?i)(?:\bminor|(?:[0-9]|1[0-7])\s*[- ]?\s*year\s*[- ]?\s*old)\s*$`)
	minorSafetyTrailingWord               = regexp.MustCompile(`(?i)^\s+([a-z][a-z-]*)\b`)
	minorSafetyNonHumanTargetWord         = regexp.MustCompile(`(?i)^(?:changes?|updates?|versions?|releases?|process(?:es)?|tasks?|components?|nodes?|dialogs?|windows?|records?|classes?|modules?|branches?|elements?|workers?|threads?|objects?|fixtures?|compatibility|buildings?|structures?|fractures?|injur(?:y|ies)|issues?|bugs?|differences?|modifications?|adjustments?|degrees?|amounts?|revisions?|patches?|features?|refactors?)$`)
	minorSafetyExplicitHumanTail          = regexp.MustCompile(`(?i)(?:\b(?:subject|target|victim|person|human|girl|boy|student|child|kid|teen|minor|actor|athlete)\b.{0,40}\b(?:actual|real)?\s*(?:[0-9]|1[0-7])\s*[- ]?\s*year\s*[- ]?\s*old\b|\b(?:actual|real)?\s*(?:[0-9]|1[0-7])\s*[- ]?\s*year\s*[- ]?\s*old\b.{0,24}\b(?:person|human|girl|boy|student|child|kid|teen|minor|victim|subject|actor|athlete)\b|\b(?:use|using|involve|involving|feature|featuring|depict|depicting|show|showing|with|subject(?:s)?\s+(?:are|is))\b.{0,32}\b(?:actual|real)\s+(?:children|child|minors?|kids?|teens?|teenagers?|adolescents?|youths?|underage\s+(?:people|persons?|users?))\b)`)
	minorSafetyDefensiveLead              = regexp.MustCompile(`(?i)\b(?:analy[sz]e|detect|prevent|classify|review|explain|discuss|study|monitor|identify|audit|test)\b|(?:分析|检测|预防|识别|分类|审核|说明|解释|研究|监控|测试)`)
	minorSafetyDefensiveObject            = regexp.MustCompile(`(?i)\b(?:how|whether|attempts?\s+to|requests?\s+to|examples?\s+(?:of|like)|phrases?\s+(?:such\s+as|like)|classifiers?|detectors?|criminals?|attackers?|indicators?|risks?|prevention|detection|moderation|policy)\b|(?:如何|是否|行为|尝试|请求|示例|样例|特征|风险|预防|检测|识别|分类|审核|治理)`)
	minorSafetyExecutionBridge            = regexp.MustCompile(`(?i)(?:[,;:]\s*(?:and|then)?\s*|\b(?:and|then)\s+|\b(?:you|we|the\s+(?:assistant|model|system))\s+(?:can|could|will|would|should|must|need\s+to|have\s+to)\s+|(?:并|然后|接着|随后|再)\s*|(?:能否|可以|能不能|请直接|请立即).{0,8})$`)
	minorSafetyHowToLead                  = regexp.MustCompile(`(?i)\bhow\s+to\b.{0,24}$|(?:如何|怎么).{0,12}$`)
	minorSafetyReferentialReactivation    = regexp.MustCompile(`(?i)(?:^|[,;.!?]\s*|\b(?:then|next|after\s+that|afterwards|subsequently|but|instead|and\s+then|so|therefore)\b.{0,24})(?:(?:do(?:\s+exactly)?|execute|apply|follow|obey|perform|generate|create|share|send|provide|give|produce|make|download|upload)\s+(?:it|that|what\s+it\s+says|the\s+(?:material|files?|images?|photos?|videos?|content|request|example|phrase))|carry\s+(?:it|that)\s+out|proceed\s+with\s+(?:it|that)|go\s+ahead|carry\s+on|answer\s+with\s+(?:it|that)|respond\s+with\s+(?:it|that))\b|(?:^|[，；。！？]\s*|(?:然后|接着|之后|随后|再|但是|改为|所以|因此).{0,12})(?:照做|照着做|按它回答|继续执行|执行它|生成它|制作它|传播它|分享它|发送它|上传它|下载它|继续做|继续进行)`)
	minorSafetyDirectReactivation         = regexp.MustCompile(`(?i)(?:^|[,;.!?]\s*)(?:just|actually|now|instead|rather\s+)?\s*(?:(?:generate|create|produce|make|send|share|upload|download|provide|give|forward|relay|fulfil(?:l)?|honou?r|answer)\s+(?:it|that|the\s+(?:material|files?|images?|photos?|videos?|content|request|example|phrase))|respond\s+with\s+(?:it|that)|(?:reproduce|replicate)\s+(?:it|that|the\s+(?:behavior|request|content|material))|(?:give|send|provide)\s+(?:me|us)\s+(?:with\s+)?(?:a\s+)?copy)\b|(?:^|[，；。！？]\s*)(?:只要|改为|实际|直接|立即|现在)?\s*(?:(?:生成|制作|发送|分享|上传|下载|提供|转发|转交|复现|复制|完成|回答)(?:它|这个|这些|该行为|该请求|该材料|该文件|该图像|该内容)|(?:给|发)(?:我|我们)(?:一份|副本|拷贝))`)
	minorSafetyReactivationNegated        = regexp.MustCompile(`(?i)\b(?:do\s+not|don't|never|must\s+not|should\s+not|cannot|can't)\b.{0,32}\b(?:do|execute|apply|follow|obey|perform|generate|create|share|send|provide|give|produce|make|download|upload|forward|relay|reproduce|replicate)\b|(?:不要|不得|禁止|切勿|不应|不能).{0,24}(?:照做|执行|遵循|服从|生成|制作|分享|发送|上传|下载|转发|转交|复现)`)
	minorSafetySequencedReactivation      = regexp.MustCompile(`(?i)\b(?:then|next|after\s+that|afterwards|subsequently|and\s+then|so|therefore)\b.{0,32}(?:(?:do(?:\s+exactly)?|execute|apply|follow|obey|perform|generate|create|share|send|provide|give|produce|make|download|upload)\s+(?:it|that|what\s+it\s+says|the\s+(?:material|files?|images?|photos?|videos?|content|request|example|phrase))|carry\s+(?:it|that)\s+out|proceed\s+with\s+(?:it|that)|go\s+ahead|carry\s+on|answer\s+with\s+(?:it|that)|respond\s+with\s+(?:it|that))\b|(?:然后|接着|之后|随后|所以|因此).{0,20}(?:照做|照着做|按它回答|继续执行|执行它|生成它|制作它|传播它|分享它|发送它|上传它|下载它|继续做|继续进行)`)
	minorSafetyExplicitReviewReactivation = regexp.MustCompile(`(?i)\b(?:comply\s+with|fulfil(?:l)?|honou?r|use|follow|obey|execute|perform|carry\s+out|act\s+on)\b.{0,64}\b(?:(?:the\s+)?quoted\s+(?:request|instruction|phrase|example|prompt)|the\s+(?:request|instruction|example|phrase)\s+above|what\s+(?:the\s+)?quote\s+says)\b|(?:遵循|服从|执行|照做|使用|完成).{0,40}(?:引用的|引号中的|上述)(?:请求|指令|示例|句子|提示词)`)
	minorSafetyTopicReset                 = regexp.MustCompile(`(?i)(?:^|[.!?;。！？；\n]\s*)(?:(?:separately|unrelated(?:ly)?|in\s+(?:an?|the)\s+separate\s+(?:task|request|section|topic)|for\s+(?:an?|the)\s+unrelated\s+(?:task|request|section|topic)|as\s+another\s+(?:task|request|topic))\b|(?:另(?:外|行|一个)(?:独立)?(?:任务|请求|话题|部分)|与此无关(?:地|的)?|单独(?:处理|完成|执行)))`)
	minorSafetyImmediateCrossResetRef     = regexp.MustCompile(`(?i)^\s*[,;:，；：]?\s*(?:(?:please|kindly|just|now|actually|instead)\s+)?(?:(?:generate|create|produce|make|send|share|upload|download|provide|give|forward|relay|execute|follow|obey|apply|fulfil(?:l)?|honou?r|answer)\s+(?:it|that|what\s+it\s+says|the\s+(?:material|files?|images?|photos?|videos?|content|request|instruction|example|phrase|quoted\s+(?:request|instruction|phrase|example|prompt)))|respond\s+with\s+(?:it|that)|do\s+(?:it|that)|(?:reproduce|replicate)\s+(?:it|that|the\s+(?:behavior|request|content|material))|(?:carry\s+(?:it|that)\s+out|proceed\s+with\s+(?:it|that)))\b|^\s*[，；：]?\s*(?:请|请你|直接|立即|现在|实际|改为)?\s*(?:(?:生成|制作|发送|分享|上传|下载|提供|执行|遵循|服从|转发|转交|复现|完成|回答)(?:它|这个|这些|该行为|该请求|该材料|该文件|该图像|该内容|引用的请求|引用的指令|上述请求|上述指令)|(?:照做|照办)\s*(?:[。！？]|$))`)
	minorSafetyQuotedProhibitionContext   = regexp.MustCompile(`(?i)\b(?:policy|rule|guideline|law|standard)\b.{0,96}\b(?:says?|states?|defines?|marks?|lists?)\b.{0,160}\b(?:prohibited|forbidden|disallowed|unsafe|illegal|violation)\b|(?:策略|规则|准则|法律|标准).{0,64}(?:说明|规定|指出|标记|定义).{0,96}(?:禁止|不允许|违规|违法|不安全)`)
	minorSafetyStandaloneProhibition      = regexp.MustCompile(`(?i)^\s*(?:(?:this|that|it|the\s+quoted\s+(?:request|instruction|phrase|example|prompt))\s+(?:is|was)\s+)?(?:prohibited|forbidden|disallowed|unsafe|illegal|a\s+violation)\b|^\s*(?:(?:这|该|上述)(?:请求|指令|句子|示例|提示词)?(?:属于|是))?(?:禁止|不允许|违规|违法|不安全)`)
	minorSafetyQuotedExecutionLead        = regexp.MustCompile(`(?i)(?:\b(?:follow|obey|execute|perform|apply|implement|adopt|fulfil(?:l)?|honou?r|complete|carry\s+out|act\s+on)\b.{0,72}\b(?:this|the|following|below|quoted)?\s*(?:instruction|request|example|fixture|case|sample|phrase|quote|prompt)\b.{0,12}|(?:遵循|服从|执行|落实|采纳|完成|照做).{0,48}(?:这个|该|以下|下述|引用的|引号中的)?(?:指令|请求|示例|夹具|用例|样例|句子|提示词).{0,8})$`)
	minorSafetyLocalReviewLead            = regexp.MustCompile(`(?i)\b(?:classify|categorize|analy[sz]e|review|inspect|evaluate|detect|scan|test)\b|(?:分类|归类|分析|审查|审核|复核|检查|评估|识别|检测|扫描|测试)`)
	minorSafetyNegationLead               = regexp.MustCompile(`(?i)(?:\b(?:do\s+not\s+(?:(?:intend|plan|aim|mean|want|wish)\s+to\s+)?|don't\s+(?:(?:intend|plan|aim|mean|want|wish)\s+to\s+)?|will\s+not\s+|won't\s+|never\s+|must\s+not\s+|should\s+not\s+|cannot\s+|can't\s+|avoid\s+|refuse(?:\s+requests?)?\s+to\s+|(?:is\s+)?illegal\s+to\s+|prohibited\s+to\s+)|(?:不要|不得|禁止|切勿|严禁|避免|不应|不能|拒绝(?:此类|这些|相关)?(?:请求)?(?:去|来)?))$`)
	minorSafetyNegationCancellation       = regexp.MustCompile(`(?i)(?:\b(?:do\s+not|don't|never|cannot|can't|must\s+not|should\s+not|may\s+not)\s+(?:refuse|decline|avoid|fail)\s+to\s+|(?:不要|不得|不能|不许|禁止|切勿).{0,12}(?:拒绝|回避|避免).{0,8}(?:去|来)?\s*)$`)
	minorSafetyNegationExceptionTail      = regexp.MustCompile(`(?i)^\s*(?:[,;:，；：]?\s*)?(?:except\b|unless\b|but\s+(?:now|actually|instead|if)\b|除非|例外(?:是|为)?|但(?:现在|实际|如果)|不过(?:现在|如果))`)
	minorSafetyDirectRequestLead          = regexp.MustCompile(`(?i)(?:\b(?:please|kindly|now|immediately|directly)\s+|\b(?:you\s+(?:must|should|need\s+to|have\s+to)|i\s+(?:want|need)\s+you\s+to|help\s+me\s+to|go\s+ahead\s+and)\s+|(?:请|请你|帮我|给我|现在|立即|直接|必须|务必|需要你)\s*)$`)
	minorSafetyNarrativeLead              = regexp.MustCompile(`(?i)(?:\b(?:police|reports?|articles?|logs?|audits?|news|researchers?|moderators?|stud(?:y|ies)|papers?)\b.{0,96}\b(?:reported|reports?|found|observed|documented|said(?:\s+that)?|says?(?:\s+that)?|shows?|describes?|records?|discusses?)\b.{0,56}|\b(?:we|the\s+(?:system|filter|classifier|detector)|logs?|audits?)\s+(?:detected|flagged|blocked|recorded|identified)\b.{0,56}\b(?:request|prompt|message|attempt)\s+to\s+|\b(?:protect|safeguard|defend)\b.{0,72}\b(?:from|against)\b.{0,56}\b(?:adults?|people|users?|criminals?|attackers?)\s+(?:who|that)\s+|(?:警方|报告|文章|日志|审计|新闻|研究|审核员).{0,64}(?:报告|显示|记录|发现|报道|指出|描述|提到).{0,40}|(?:我们|系统|过滤器|分类器|检测器).{0,24}(?:检测到|识别到|拦截了|记录了).{0,32}(?:请求|提示词|消息|尝试)|(?:保护|保障|防止).{0,48}(?:儿童|未成年人|孩子).{0,32}(?:免受|避免|防范).{0,32}(?:成年人|犯罪者|攻击者).{0,8})$`)
	minorSafetyDirectGroomAbuseCandidate  = regexp.MustCompile(`(?i)\b(?:groom|abuse)\b` + minorDirectHarmTargetEN)
	minorSafetyMaterialLinkCandidate      = regexp.MustCompile(`(?i)\b(?:send|share|provide|give|forward|relay|upload|publish|distribute)\b(?:\s+(?:me|us))?(?:\s+(?:the|some|those|these))?\s+links?\s+(?:to|for)\s+(?:the\s+)?` + minorIllegalMaterialEN + `\b`)
	minorSafetyBenignGroomingTail         = regexp.MustCompile(`(?i)^\s+for\s+(?:the\s+)?(?:leadership|management|presentation|performance|competition|show|livestock|animal|agricultural)\b`)
	minorSafetyFixtureLabel               = regexp.MustCompile(`(?i)(?:(?:detector|classifier|policy|moderation|safety|test|regression(?:\s+test)?)\s+(?:fixture|example|case|sample|input)|expected\s+(?:block|violation)|(?:检测|分类|策略|审核|安全|回归|测试)(?:夹具|示例|样例|用例|输入)|预期(?:拦截|违规))\s*[:：]\s*(?:\n\s*)?(?:["'` + "`" + `]\s*)?$`)
	minorSafetyFixtureExecutionLead       = regexp.MustCompile(`(?i)\b(?:comply\s+with|fulfil(?:l)?|honou?r|implement|adopt|complete|follow|obey|execute|perform|apply|use|answer|respond\s+to|carry\s+out|act\s+on)\b[^.!?。！？\n]{0,40}\b(?:this|the|following|below|quoted)?\s*(?:policy\s+)?(?:instruction|request|fixture|example|case|sample|input)\b|\b(?:execute|follow|obey|implement|apply|fulfil(?:l)?|honou?r|complete)\s+(?:this|the|following|below)\s+(?:instruction|request)\b|(?:执行|照做|遵循|服从|落实|采纳|完成|应用|按照)[^。！？\n]{0,32}(?:这个|该|以下|下述|引用的)?(?:策略)?(?:指令|请求|夹具|示例|样例|用例|输入)`)
	minorSafetyFixtureExplicitExecution   = regexp.MustCompile(`(?i)\b(?:comply\s+with|fulfil(?:l)?|honou?r|implement|adopt|complete|follow|obey|execute|perform|apply|use|answer|respond\s+to|carry\s+out|act\s+on)\b[^.!?。！？\n]{0,64}\b(?:this|the|following|below|quoted)?\s*(?:policy\s+)?(?:fixture|example|case|sample|input)\b|(?:执行|照做|遵循|服从|落实|采纳|完成|应用|按照)[^。！？\n]{0,48}(?:这个|该|以下|下述|引用的)?(?:策略)?(?:夹具|示例|样例|用例|输入)`)
	minorSafetyFixtureNonExecutionLead    = regexp.MustCompile(`(?i)\b(?:do\s+not|don't|never|must\s+not|should\s+not|cannot|can't)\b[^.!?。！？\n]{0,48}\b(?:comply\s+with|fulfil(?:l)?|honou?r|implement|adopt|complete|follow|obey|execute|perform|apply|use|answer|respond\s+to|carry\s+out|act\s+on)\b|(?:不要|不得|禁止|切勿|不应|不能)[^。！？\n]{0,28}(?:执行|照做|遵循|服从|落实|采纳|完成|应用|按照)`)
	minorSafetyFixtureRemainder           = regexp.MustCompile(`^\s*(?:["'` + "`" + `]\s*)?[.!?。！？；;:]?\s*$`)
	minorSafetyFixtureExpectedRemainder   = regexp.MustCompile(`(?i)^\s*(?:["'` + "`" + `]\s*)?[.!?。！？；;:]?\s*(?:(?:expected(?:\s+result)?\s*(?:is|=|:)?\s*(?:block(?:ed)?|reject(?:ed)?|violation|unsafe)|should\s+be\s+(?:block(?:ed)?|reject(?:ed)?|flagged))|(?:预期(?:结果)?(?:为|是|：|:)?(?:拦截|拒绝|违规|不安全)|应当?被?(?:拦截|拒绝|标记)))[.!?。！？；;:]?\s*$`)
	minorSafetyJSONFixtureLead            = regexp.MustCompile(`(?i)[{,]\s*["']?(?:input|prompt|text|request)["']?\s*:\s*["'][^"']*$`)
	minorSafetyJSONFixtureExpected        = regexp.MustCompile(`(?i)^[^}\n]{0,256}["']\s*,?\s*["']?(?:expected|action|result)["']?\s*:\s*["']?(?:block(?:ed)?|deny|denied|reject(?:ed)?|violation|unsafe)["']?`)
	minorSafetyJSONFixtureExecution       = regexp.MustCompile(`(?i)\b(?:comply\s+with|fulfil(?:l)?|honou?r|follow|obey|execute|perform|apply|use|answer|respond\s+to|carry\s+out|act\s+on)\b.{0,40}\b(?:the\s+)?(?:input|prompt|request|text)\b|(?:遵循|服从|执行|照做|应用|按照|完成).{0,24}(?:输入|提示词|请求|文本)`)
	minorSafetyCompactMaterialAction      = regexp.MustCompile(`(?i)(?:create|generate|produce|make|provide|give|share|distribute|publish|upload|sell|trade|send|forward|relay|reproduce|replicate|download|obtain|request|receive|fetch|find|acquire|locate|source|procure)(?:csam|childsexualabusematerial|childporn(?:ography)?)`)
	minorSafetyCompactSafeArtifactTail    = regexp.MustCompile(`(?i)^(?:detectionreport|classificationreport|moderationreport|policyreport|incidentreport|auditreport|detector|classifier|hashlist|signature|scanner|filter|moderationrule|policyrule|benchmark|fixture)`)
	minorSafetyCompactUnsafeTail          = regexp.MustCompile(`(?i)(?:(?:actual|real|raw|original|explicit|illegal)(?:files?|images?|photos?|videos?|content|material|samples?)|(?:send|share|upload|publish|distribute|retrieve|fetch|download)(?:the)?(?:material|files?|images?|photos?|videos?|content|samples?))`)
	minorSafetyCompactReviewLead          = regexp.MustCompile(`(?i)(?:(?:classif|analy[sz]|detect|review).{0,160}(?:quotedphrase|quotedrequest|quote|example)|(?:classifier|detector|policy|moderation|safety|test|regression)(?:fixture|example|case|sample|input)|(?:policy|rule|guideline|law).{0,96}(?:says|states|defines|marks|lists))$`)
	minorSafetyCompactNonExecutionTail    = regexp.MustCompile(`(?i)^.{0,96}(?:donotexecute|neverexecute|isprohibited|isforbidden|isdisallowed|isunsafe|isillegal|isaviolation)`)
	minorSafetyCompactReactivation        = regexp.MustCompile(`(?i)(?:thendoit|thenexecuteit|thengenerateit|thenshareit|thenuploadit|thensendit|thendownloadit|proceedwithit|carryitout|(?:complywith|execute|follow|obey)thequoted(?:request|instruction|phrase|example|prompt)|sendittome|shareitwithme)`)
	minorSafetyCompactCrossResetReference = regexp.MustCompile(`(?i)(?:(?:complywith|execute|follow|obey)thequoted(?:request|instruction|phrase|example|prompt))`)
	minorSafetyCompactTopicReset          = regexp.MustCompile(`(?i)(?:separately|unrelatedly|in(?:an|the)separate(?:task|request|section|topic)|for(?:an|the)unrelated(?:task|request|section|topic)|asanother(?:task|request|topic))`)
	minorSafetyCompactImmediateResetRef   = regexp.MustCompile(`(?i)^(?:(?:please|kindly|just|now|actually|instead))?(?:doit|dothat|fulfillit|fulfilit|honorit|honourit|answerit|respondwithit|generateit|createit|produceit|makeit|sendit|shareit|forwardit|relayit|reproduceit|replicatethatbehavior|uploadit|downloadit|provideit|giveit|sendthematerial|sharethematerial|uploadthematerial|downloadthematerial|(?:complywith|execute|follow|obey)thequoted(?:request|instruction|phrase|example|prompt)|carryitout|proceedwithit|照做|照办)`)
	minorSafetyCompactDefensiveCodeLead   = regexp.MustCompile(`(?i)(?:if|else(?:if)?|case|switch|when|match|contains|regex|pattern|prompt|input).{0,80}$`)
	minorSafetyCompactDefensiveCodeTail   = regexp.MustCompile(`(?i)^.{0,80}(?:reject|block|deny|flag|quarantine|returnfalse|actionblock|policyviolation|unsafe)`)
)

func minorSafetyShouldInspectCompact(text string) bool {
	for start := 0; start < len(text); start++ {
		first := asciiLower(text[start])
		if first < 'a' || first > 'z' || start > 0 && isASCIIAlphaNumeric(text[start-1]) {
			continue
		}
		for _, pair := range minorSafetyCompactSourcePairs[first] {
			if _, fragmented := matchMinorSafetyFragmentedSource(text, start, pair); fragmented {
				return true
			}
		}
	}
	return false
}

type minorSafetyCompactSourcePair struct {
	value     string
	actionEnd int
}

var minorSafetyCompactSourcePairs = func() map[byte][]minorSafetyCompactSourcePair {
	actions := []string{
		"create", "generate", "produce", "make", "provide", "give", "share", "distribute", "publish",
		"upload", "sell", "trade", "send", "forward", "relay", "reproduce", "replicate", "download", "obtain", "request", "receive", "fetch",
		"find", "acquire", "locate", "source", "procure",
	}
	targets := []string{"csam", "childsexualabusematerial", "childporn", "childpornography"}
	pairs := make(map[byte][]minorSafetyCompactSourcePair, len(actions))
	for _, action := range actions {
		for _, target := range targets {
			pair := minorSafetyCompactSourcePair{value: action + target, actionEnd: len(action)}
			pairs[action[0]] = append(pairs[action[0]], pair)
		}
	}
	return pairs
}()

func matchMinorSafetyFragmentedSource(text string, start int, pair minorSafetyCompactSourcePair) (int, bool) {
	position := start
	fragmented := false
	for index := 0; index < len(pair.value); index++ {
		if index > 0 {
			separatorStart := position
			for position < len(text) {
				next, ok := consumeMinorSafetyFragmentSeparator(text, position)
				if !ok {
					break
				}
				position = next
			}
			// A lone separator exactly between action and target is common in
			// source identifiers (generate_csam) and is not obfuscation evidence.
			if position > separatorStart && index != pair.actionEnd {
				fragmented = true
			}
		}
		if position >= len(text) || asciiLower(text[position]) != pair.value[index] {
			return 0, false
		}
		position++
	}
	if position < len(text) && isASCIIAlphaNumeric(text[position]) {
		return 0, false
	}
	return position, fragmented
}

func consumeMinorSafetyFragmentSeparator(text string, position int) (int, bool) {
	if position < 0 || position >= len(text) {
		return position, false
	}
	r, size := utf8.DecodeRuneInString(text[position:])
	if r == utf8.RuneError && size == 1 {
		return position, false
	}
	if unicode.IsSpace(r) || unicode.Is(unicode.Bidi_Control, r) || r == '.' || r == ',' || r == '/' || r == '\\' || r == '_' || r == '-' || r == ':' || r == '|' || r == '\u200b' || r == '\u200c' || r == '\u200d' || r == '\u2060' || r == '\ufeff' {
		return position + size, true
	}
	return position, false
}

func asciiLower(value byte) byte {
	if value >= 'A' && value <= 'Z' {
		return value + ('a' - 'A')
	}
	return value
}

var minorSafetyCandidateHints = []string{
	"minor", "child", "underage", "kid", "teen", "adolescent", "youth", "csam",
	"preteen", "toddler", "infant", "未成年", "儿童", "孩子", "小孩", "小学生", "幼童", "幼儿", "青少年", "少年", "岁",
}

// patternSuppressedForQuotedPolicyReview handles a narrow false-positive
// boundary: a policy phrase is quoted as detector/classifier input and the
// user explicitly says not to follow it. Every occurrence must remain quoted,
// so adding an unquoted override or a later instruction to execute the quote
// restores the normal strict match.
func patternSuppressedForQuotedPolicyReview(original string, pattern compiledPattern) bool {
	switch pattern.cfg.Name {
	case "prompt_policy_override", "safety_bypass_request", "operational_security_artifact_request":
	default:
		return false
	}
	if pattern.re == nil || !quotedPolicyReviewContextPattern.MatchString(original) || !quotedPolicyNonExecutionPattern.MatchString(original) || quotedPolicyReactivationPattern.MatchString(original) {
		return false
	}
	matches := pattern.re.FindAllStringIndex(original, -1)
	if len(matches) == 0 {
		return false
	}
	for _, loc := range matches {
		if len(loc) != 2 || !operationalRequestIsQuoted(original, loc[0]) {
			return false
		}
	}
	return true
}

func patternSuppressedForDefensiveRuleArtifact(original string, pattern compiledPattern) bool {
	switch pattern.cfg.Name {
	case "operational_remote_access_request", "reverse_shell_execution", "malware_creation_request", "ransomware_deployment":
	default:
		return false
	}
	if pattern.re == nil || defensiveRuleContinuationPattern.MatchString(original) {
		return false
	}
	matches := pattern.re.FindAllStringIndex(original, -1)
	if len(matches) == 0 {
		return false
	}
	for _, loc := range matches {
		if len(loc) != 2 {
			return false
		}
		sentence := matchSentence(original, loc[0], loc[1])
		if !defensiveArtifactText(sentence) && !explicitStrictNegationPattern.MatchString(sentence) {
			return false
		}
	}
	return true
}

// patternSuppressedForDefensiveDocumentation covers narrow quoted, policy,
// detector, and incident-report uses of otherwise strict rules. Suppression is
// local to every matched sentence and is cancelled by a later instruction to
// perform the dangerous action. This avoids whole-prompt exclusion tokens that
// an attacker could append to an operational request as a bypass.
func patternSuppressedForDefensiveDocumentation(original string, pattern compiledPattern) bool {
	switch pattern.cfg.Name {
	case "prompt_system_exfiltration", "malicious_persistence_c2", "self_harm_facilitation",
		"safety_bypass_request", "operational_security_technique_request",
		"reverse_engineering_secret_extraction", "credential_theft":
	default:
		return false
	}
	if pattern.re == nil {
		return false
	}
	if pattern.cfg.Name == "self_harm_facilitation" {
		// Self-harm prevention guidance often contains ordinary transitions such
		// as "however, provide prevention methods". Only the target-bound
		// self-harm reactivation detector may cancel its protective suppression;
		// the generic documentation reactivation pattern is intentionally broader
		// and would turn prevention guidance into a strict false positive.
		if selfHarmHasActiveReactivation(original) {
			return false
		}
		if selfHarmProtectiveDocumentationMatches(original, pattern) {
			return true
		}
	} else if defensiveDocumentationReactivationPattern.MatchString(original) {
		return false
	}
	matches := pattern.re.FindAllStringIndex(original, -1)
	if len(matches) == 0 {
		return false
	}
	for _, loc := range matches {
		if len(loc) != 2 {
			return false
		}
		window := localMatchWindow(original, loc[0], loc[1], 180)
		if !defensiveDocumentationContextPattern.MatchString(window) || !defensiveDocumentationTreatmentPattern.MatchString(window) {
			return false
		}
		if pattern.cfg.Name == "malicious_persistence_c2" && !benignPersistenceDocumentationPattern.MatchString(window) {
			return false
		}
	}
	return true
}

// patternSuppressedForNarrativeRefusal prevents ordinary dialogue and character
// analysis such as "舍不得拒绝我" from becoming a prompt-injection decision. The
// exception is intentionally unavailable when the local sentence talks about an
// assistant/model, instructions, policy, moderation, or contains an imperative
// request. Those cases remain fully enforceable.
func patternSuppressedForNarrativeRefusal(original string, scanText string, pattern compiledPattern) bool {
	if pattern.cfg.Name != "prompt_refusal_suppression" || pattern.re == nil {
		return false
	}
	if scanText == "" {
		scanText = normalizeForScan(original)
	}
	matches := pattern.re.FindAllStringIndex(scanText, -1)
	if len(matches) == 0 {
		return false
	}
	for _, loc := range matches {
		if len(loc) != 2 || loc[0] < 0 || loc[1] < loc[0] || loc[1] > len(scanText) {
			return false
		}
		window := localMatchWindow(scanText, loc[0], loc[1], 120)
		if !narrativeRefusalContextPattern.MatchString(window) ||
			refusalPolicyContextPattern.MatchString(window) ||
			refusalDirectiveContextPattern.MatchString(window) {
			return false
		}
	}
	return true
}

func selfHarmProtectiveDocumentationMatches(original string, pattern compiledPattern) bool {
	if pattern.re == nil || selfHarmHasActiveReactivation(original) {
		return false
	}
	matches := pattern.re.FindAllStringIndex(original, -1)
	if len(matches) == 0 {
		return false
	}
	for _, loc := range matches {
		if len(loc) != 2 || loc[0] < 0 || loc[1] < loc[0] || loc[1] > len(original) {
			return false
		}
		window := matchSentence(original, loc[0], loc[1])
		matched := original[loc[0]:loc[1]]
		protective := selfHarmProtectiveDocumentationPattern.MatchString(window) || selfHarmBoundPreventionPattern.MatchString(matched)
		research := selfHarmResearchMethodsPattern.MatchString(matched) &&
			selfHarmResearchOutcomePattern.MatchString(matched) &&
			!selfHarmResearchCausationPattern.MatchString(matched)
		if !protective && !research {
			return false
		}
	}
	return true
}

func selfHarmHasActiveReactivation(text string) bool {
	for _, loc := range selfHarmDocumentationReactivationPattern.FindAllStringIndex(text, -1) {
		if len(loc) != 2 || loc[0] < 0 || loc[1] < loc[0] || loc[1] > len(text) {
			return true
		}
		windowEnd := len(text)
		if boundary := strings.IndexAny(text[loc[1]:], ".!?;。！？；\n"); boundary >= 0 {
			windowEnd = loc[1] + boundary
		}
		tail := text[loc[0]:windowEnd]
		if selfHarmBoundPreventionPattern.MatchString(tail) && !selfHarmPreventionCancellationPattern.MatchString(tail) {
			continue
		}
		return true
	}
	return false
}

// patternSuppressedForAuthorizationBoundary distinguishes a neutral access-
// control requirement from a request to perform unauthorized access. The
// unauthorized_exploitation rule intentionally remains strict for exploit,
// compromise, takeover, and control verbs; only matches whose actual evidence
// contains the neutral access verb may enter this narrow suppression path.
// Every matched sentence must impose a concrete read-only, denial, or
// restriction boundary, and any later instruction to bypass that boundary
// restores enforcement for the whole request.
func patternSuppressedForAuthorizationBoundary(original string, scanText string, pattern compiledPattern) bool {
	if pattern.cfg.Name != "unauthorized_exploitation" || pattern.re == nil {
		return false
	}
	if scanText == "" {
		scanText = normalizeForScan(original)
	}
	if !authorizationBoundaryTextIsRestricted(scanText, pattern) {
		return false
	}

	// A derived URL/HTML/Base64/NFKC view can contain the only readable copy of
	// a safe authorization requirement. Do not require the opaque source to
	// match the rule, but fail closed when either the raw or canonical source
	// contains a separate active instruction or an unsafe authorization match.
	for _, source := range []string{original, normalizeForScan(original)} {
		if source == "" || source == scanText {
			continue
		}
		if pattern.re.MatchString(source) {
			if !authorizationBoundaryTextIsRestricted(source, pattern) {
				return false
			}
			continue
		}
		if authorizationBoundaryHasActiveReactivation(source) {
			return false
		}
	}
	return true
}

// patternSuppressedForNegatedPolicyAction handles explicit prevention and
// denial requirements such as "do not bypass the safety policy" or "never
// reveal the system prompt". Suppression is bound to every concrete regex
// match and is cancelled by a later instruction that reactivates the action;
// a harmless negation elsewhere in the prompt cannot launder an active bypass.
func patternSuppressedForNegatedPolicyAction(original string, scanText string, pattern compiledPattern) bool {
	switch pattern.cfg.Name {
	case "safety_bypass_request", "prompt_system_exfiltration":
	default:
		return false
	}
	if pattern.re == nil {
		return false
	}
	if scanText == "" {
		scanText = normalizeForScan(original)
	}
	if !policyActionMatchesAreNegated(scanText, pattern) {
		return false
	}
	normalizedOriginal := normalizeForScan(original)
	if normalizedOriginal == scanText {
		return true
	}
	for _, source := range []string{original, normalizedOriginal} {
		if source == "" || source == scanText {
			continue
		}
		if pattern.re.MatchString(source) && !policyActionMatchesAreNegated(source, pattern) {
			return false
		}
	}
	return true
}

var (
	exfiltrationMentionPattern = regexp.MustCompile(`(?i)\b(?:exfiltrate|exfiltration|data\s+theft|steal\s+data|siphon\s+data)\b`)
	// "no exfiltration", "no network or exfiltration", "without data theft".
	// Only "or"/"nor" keep the negation distributive; "no limits and
	// exfiltration ..." does not negate the second term.
	exfiltrationNounNegationPattern = regexp.MustCompile(`(?i)\b(?:no|without)\s+(?:[a-z][a-z-]*\s+n?or\s+)?$`)
	// Active transfer wording inside the matched span means the regex gap
	// between the negated noun and the data target carries a live request,
	// as in "no exfiltration component; upload the customer database".
	exfiltrationActiveTransferPattern = regexp.MustCompile(`(?i)\b(?:upload(?:s|ed|ing)?|send(?:s|ing)?|sent|transfer(?:s|red|ring)?|cop(?:y|ies|ied|ying)|export(?:s|ed|ing)?|extract(?:s|ed|ing)?|dump(?:s|ed|ing)?|leak(?:s|ed|ing)?|download(?:s|ed|ing)?|mov(?:e|es|ed|ing)|forward(?:s|ed|ing)?|post(?:s|ed|ing)?|push(?:es|ed|ing)?|sync(?:s|ed|ing)?|e-?mail(?:s|ed|ing)?|pip(?:e|es|ed|ing)|shar(?:e|es|ed|ing)|steal(?:s|ing)?|stol(?:e|en)|siphon(?:s|ed|ing)?|smuggl(?:e|es|ed|ing)|grab(?:s|bed|bing)?|giv(?:e|es|en|ing)|gave|sell(?:s|ing)?|sold|publish(?:es|ed|ing)?|scrap(?:e|es|ed|ing)|harvest(?:s|ed|ing)?|collect(?:s|ed|ing)?|retriev(?:e|es|ed|ing)|fetch(?:es|ed|ing)?|exfil)\b`)
)

// patternSuppressedForNegatedExfiltrationMention covers policy text that lists
// exfiltration among excluded properties, such as "with no network or
// exfiltration component, no credential access, and no file deletion". Every
// exfiltration term inside every match must be the noun form directly governed
// by "no"/"without"; a verb form, any un-negated mention, or active transfer
// wording anywhere in the match keeps the rule strict, so one negated mention
// cannot launder a request. Composite patterns are never suppressed because
// their all/any clauses are evidence this check does not inspect.
func patternSuppressedForNegatedExfiltrationMention(original string, scanText string, pattern compiledPattern) bool {
	if pattern.cfg.Name != "data_exfiltration" || pattern.re == nil || len(pattern.all) > 0 || len(pattern.any) > 0 {
		return false
	}
	if scanText == "" {
		scanText = normalizeForScan(original)
	}
	if !exfiltrationMatchesAreNegated(scanText, pattern) {
		return false
	}
	for _, source := range []string{original, normalizeForScan(original)} {
		if source == "" || source == scanText {
			continue
		}
		if pattern.re.MatchString(source) && !exfiltrationMatchesAreNegated(source, pattern) {
			return false
		}
	}
	return true
}

func exfiltrationMatchesAreNegated(text string, pattern compiledPattern) bool {
	matches := pattern.re.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return false
	}
	for _, loc := range matches {
		if len(loc) != 2 || loc[0] < 0 || loc[1] < loc[0] || loc[1] > len(text) {
			return false
		}
		if exfiltrationActiveTransferPattern.MatchString(text[loc[0]:loc[1]]) {
			return false
		}
		mentions := exfiltrationMentionPattern.FindAllStringIndex(text[loc[0]:loc[1]], -1)
		if len(mentions) == 0 {
			return false
		}
		for _, mention := range mentions {
			start, end := loc[0]+mention[0], loc[0]+mention[1]
			term := strings.ToLower(text[start:end])
			if !strings.HasPrefix(term, "exfiltration") && !strings.HasPrefix(term, "data") {
				return false
			}
			if !exfiltrationNounNegationPattern.MatchString(text[max(0, start-48):start]) {
				return false
			}
		}
	}
	return true
}

// patternSuppressedForProtectiveRefusal keeps a narrow class of positive
// protection requirements out of the generic refusal-suppression rule. For
// example, "do not fail to block attempts to reveal the system prompt" asks
// the guard to keep blocking; it is not an instruction for the assistant to
// suppress a refusal. Both the outer double negation and the concrete
// protective action must be present in the same short clause.
func patternSuppressedForProtectiveRefusal(original string, scanText string, pattern compiledPattern) bool {
	if pattern.cfg.Name != "prompt_refusal_suppression" || pattern.re == nil {
		return false
	}
	if scanText == "" {
		scanText = normalizeForScan(original)
	}
	if !protectiveRefusalMatchesAreBounded(scanText, pattern) {
		return false
	}
	normalizedOriginal := normalizeForScan(original)
	for _, source := range []string{original, normalizedOriginal} {
		if source == "" || source == scanText {
			continue
		}
		if pattern.re.MatchString(source) && !protectiveRefusalMatchesAreBounded(source, pattern) {
			return false
		}
	}
	return true
}

func protectiveRefusalMatchesAreBounded(text string, pattern compiledPattern) bool {
	matches := pattern.re.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return false
	}
	for _, loc := range matches {
		if len(loc) != 2 || loc[0] < 0 || loc[1] < loc[0] || loc[1] > len(text) {
			return false
		}
		warningWindowEnd := min(len(text), loc[1]+32)
		if protectiveWarningSuppressionPattern.MatchString(text[loc[0]:warningWindowEnd]) {
			continue
		}
		clause := policySemanticClauseWindow(text, loc[0], loc[1])
		if clause == "" || !protectiveRefusalSuppressionPattern.MatchString(clause) {
			return false
		}
	}
	return true
}

func policySemanticClauseWindow(text string, matchStart int, matchEnd int) string {
	if matchStart < 0 || matchEnd < matchStart || matchEnd > len(text) {
		return ""
	}
	start := 0
	if delimiter := strings.LastIndexAny(text[:matchStart], ".!?;。！？；\n"); delimiter >= 0 {
		_, size := utf8.DecodeRuneInString(text[delimiter:])
		start = delimiter + size
	}
	if matchStart-start > 192 {
		start = matchStart - 192
		for start < matchStart && !utf8.RuneStart(text[start]) {
			start++
		}
	}
	end := len(text)
	if relative := strings.IndexAny(text[matchEnd:], ".!?;。！？；\n"); relative >= 0 {
		end = matchEnd + relative
	}
	if end-matchEnd > 192 {
		end = matchEnd + 192
		for end > matchEnd && end < len(text) && !utf8.RuneStart(text[end]) {
			end--
		}
	}
	return text[start:end]
}

func policyActionMatchesAreNegated(text string, pattern compiledPattern) bool {
	matches := pattern.re.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return false
	}
	index := buildPolicyActionNegationIndex(text)
	for _, loc := range matches {
		if len(loc) != 2 || loc[0] < 0 || loc[1] < loc[0] || loc[1] > len(text) {
			return false
		}
		actions := policyActionVerbLocations(index.lower, loc[0], loc[1])
		if len(actions) == 0 {
			return false
		}
		for _, action := range actions {
			if !policyActionVerbIsNegated(text, action[0], action[1], index) {
				return false
			}
		}
	}
	return true
}

type negatedPolicyCueKind uint8

const (
	negatedPolicyCueNone negatedPolicyCueKind = iota
	negatedPolicyCueDirect
	negatedPolicyCueProtective
)

type policyActionCue struct {
	kind       negatedPolicyCueKind
	start, end int
}

type policyActionNegationIndex struct {
	lower      string
	boundaries []int
}

func buildPolicyActionNegationIndex(text string) policyActionNegationIndex {
	index := policyActionNegationIndex{lower: asciiLowerPreservingBytes(text)}
	for byteIndex, r := range text {
		switch r {
		case '.', '!', '?', ';', ',', ':', '。', '！', '？', '；', '，', '：', '\n':
			index.boundaries = append(index.boundaries, byteIndex+utf8.RuneLen(r))
		}
	}
	return index
}

func policyActionVerbIsNegated(text string, actionStart int, actionEnd int, index policyActionNegationIndex) bool {
	if actionStart < 0 || actionEnd <= actionStart || actionEnd > len(text) {
		return false
	}
	clauseStart := 0
	if boundaryIndex := sort.SearchInts(index.boundaries, actionStart+1) - 1; boundaryIndex >= 0 {
		clauseStart = index.boundaries[boundaryIndex]
	}
	if clauseStart >= actionStart {
		return false
	}
	windowStart := clauseStart
	if actionStart-windowStart > 192 {
		windowStart = actionStart - 192
		for windowStart < actionStart && !utf8.RuneStart(text[windowStart]) {
			windowStart++
		}
	}
	latest := latestPolicyActionCue(index.lower, windowStart, actionStart)
	if latest.kind == negatedPolicyCueNone {
		return false
	}
	if latest.start < clauseStart || latest.end > actionStart || actionStart-latest.end > 128 {
		return false
	}
	// A second action or a coordinating clause after the negation means the
	// current verb is no longer governed by that negation. This closes comma and
	// "and then" laundering without expanding the scan to unrelated sentences.
	bridge := index.lower[latest.end:actionStart]
	if latest.kind == negatedPolicyCueDirect && negatedPolicyActionDirectCancellation.MatchString(bridge) {
		return false
	}
	sharedDirectNegation := latest.kind == negatedPolicyCueDirect && negatedPolicyActionSharedBridgePattern.MatchString(bridge)
	if containsPolicyActionVerb(bridge) && !sharedDirectNegation {
		return false
	}
	if containsPolicyActionCoordination(bridge) {
		return false
	}
	if latest.kind == negatedPolicyCueProtective {
		directBefore := latestPolicyActionCueOfKind(index.lower, windowStart, latest.start, negatedPolicyCueDirect)
		if directBefore.kind != negatedPolicyCueNone {
			betweenCues := index.lower[directBefore.end:latest.start]
			if directBefore.start >= clauseStart && latest.start-directBefore.end <= 48 && !containsPolicyActionCoordination(betweenCues) {
				cancellation := safeUTF8Suffix(text[directBefore.start:actionStart], 128) + text[actionStart:actionEnd]
				if negatedPolicyActionCancellationPattern.MatchString(cancellation) && !negatedPolicyActionDirectCancellation.MatchString(betweenCues) {
					return false
				}
			}
		}
	}
	contextStart := latest.start
	if actionEnd-contextStart > 160 {
		contextStart = actionEnd - 160
		for contextStart < actionEnd && !utf8.RuneStart(text[contextStart]) {
			contextStart++
		}
	}
	context := text[contextStart:actionEnd]
	switch latest.kind {
	case negatedPolicyCueDirect:
		return actionStart-latest.end <= 80
	case negatedPolicyCueProtective:
		return actionStart-latest.end <= 80 && negatedPolicyActionProtectiveBoundPattern.MatchString(context)
	default:
		return false
	}
}

type policyActionLiteral struct {
	text string
	word bool
}

var policyActionVerbLiterals = []policyActionLiteral{
	{text: "bypass", word: true}, {text: "evade", word: true}, {text: "disable", word: true},
	{text: "ignore", word: true}, {text: "remove", word: true}, {text: "reveal", word: true},
	{text: "show", word: true}, {text: "dump", word: true}, {text: "read", word: true},
	{text: "print", word: true}, {text: "绕过"}, {text: "规避"}, {text: "关闭"}, {text: "忽略"},
	{text: "移除"}, {text: "泄露"}, {text: "展示"}, {text: "输出"}, {text: "读取"}, {text: "查看"},
}

var policyActionCueLiterals = []struct {
	policyActionLiteral
	kind negatedPolicyCueKind
}{
	{policyActionLiteral: policyActionLiteral{text: "do not", word: true}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "don't", word: true}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "never", word: true}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "must not", word: true}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "should not", word: true}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "will not", word: true}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "does not", word: true}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "cannot", word: true}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "can't", word: true}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "不要"}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "不得"}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "禁止"}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "不能"}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "不应"}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "切勿"}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "严禁"}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "不会"}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "无法"}, kind: negatedPolicyCueDirect},
	{policyActionLiteral: policyActionLiteral{text: "prevent", word: true}, kind: negatedPolicyCueProtective},
	{policyActionLiteral: policyActionLiteral{text: "block", word: true}, kind: negatedPolicyCueProtective},
	{policyActionLiteral: policyActionLiteral{text: "stop", word: true}, kind: negatedPolicyCueProtective},
	{policyActionLiteral: policyActionLiteral{text: "detect", word: true}, kind: negatedPolicyCueProtective},
	{policyActionLiteral: policyActionLiteral{text: "test", word: true}, kind: negatedPolicyCueProtective},
	{policyActionLiteral: policyActionLiteral{text: "verify", word: true}, kind: negatedPolicyCueProtective},
	{policyActionLiteral: policyActionLiteral{text: "防止"}, kind: negatedPolicyCueProtective},
	{policyActionLiteral: policyActionLiteral{text: "阻止"}, kind: negatedPolicyCueProtective},
	{policyActionLiteral: policyActionLiteral{text: "拦截"}, kind: negatedPolicyCueProtective},
	{policyActionLiteral: policyActionLiteral{text: "检测"}, kind: negatedPolicyCueProtective},
	{policyActionLiteral: policyActionLiteral{text: "测试"}, kind: negatedPolicyCueProtective},
	{policyActionLiteral: policyActionLiteral{text: "验证"}, kind: negatedPolicyCueProtective},
}

func asciiLowerPreservingBytes(text string) string {
	var lowered []byte
	for index := 0; index < len(text); index++ {
		value := text[index]
		if value < 'A' || value > 'Z' {
			continue
		}
		if lowered == nil {
			lowered = []byte(text)
		}
		lowered[index] = value + ('a' - 'A')
	}
	if lowered == nil {
		return text
	}
	return string(lowered)
}

func policyActionVerbLocations(lower string, start int, end int) [][2]int {
	locations := make([][2]int, 0, 2)
	for _, literal := range policyActionVerbLiterals {
		for offset := start; offset < end; {
			relative := strings.Index(lower[offset:end], literal.text)
			if relative < 0 {
				break
			}
			locStart := offset + relative
			locEnd := locStart + len(literal.text)
			if !literal.word || asciiLiteralHasWordBoundaries(lower, locStart, locEnd) {
				locations = append(locations, [2]int{locStart, locEnd})
			}
			offset = locStart + 1
		}
	}
	sort.Slice(locations, func(i, j int) bool {
		if locations[i][0] != locations[j][0] {
			return locations[i][0] < locations[j][0]
		}
		return locations[i][1] < locations[j][1]
	})
	return locations
}

func latestPolicyActionCue(lower string, start int, end int) policyActionCue {
	return latestPolicyActionCueOfKind(lower, start, end, negatedPolicyCueNone)
}

func latestPolicyActionCueOfKind(lower string, start int, end int, wanted negatedPolicyCueKind) policyActionCue {
	latest := policyActionCue{kind: negatedPolicyCueNone}
	for _, cue := range policyActionCueLiterals {
		if wanted != negatedPolicyCueNone && cue.kind != wanted {
			continue
		}
		locStart := lastPolicyLiteralIndex(lower, start, end, cue.policyActionLiteral)
		if locStart < 0 {
			continue
		}
		locEnd := locStart + len(cue.text)
		if latest.kind == negatedPolicyCueNone || locEnd > latest.end {
			latest = policyActionCue{kind: cue.kind, start: locStart, end: locEnd}
		}
	}
	return latest
}

func lastPolicyLiteralIndex(text string, start int, end int, literal policyActionLiteral) int {
	searchEnd := end
	for searchEnd > start {
		relative := strings.LastIndex(text[start:searchEnd], literal.text)
		if relative < 0 {
			return -1
		}
		index := start + relative
		literalEnd := index + len(literal.text)
		if !literal.word || asciiLiteralHasWordBoundaries(text, index, literalEnd) {
			return index
		}
		searchEnd = index
	}
	return -1
}

func containsPolicyActionVerb(lower string) bool {
	for _, literal := range policyActionVerbLiterals {
		for offset := 0; offset < len(lower); {
			relative := strings.Index(lower[offset:], literal.text)
			if relative < 0 {
				break
			}
			locStart := offset + relative
			locEnd := locStart + len(literal.text)
			if !literal.word || asciiLiteralHasWordBoundaries(lower, locStart, locEnd) {
				return true
			}
			offset = locStart + 1
		}
	}
	return false
}

func containsPolicyActionCoordination(lower string) bool {
	if strings.ContainsAny(lower, ",:，：") {
		return true
	}
	for _, literal := range []policyActionLiteral{
		{text: "and", word: true}, {text: "but", word: true}, {text: "then", word: true},
		{text: "now", word: true}, {text: "instead", word: true}, {text: "actually", word: true},
		{text: "however", word: true}, {text: "also", word: true}, {text: "并且"}, {text: "并"},
		{text: "且"}, {text: "但是"}, {text: "但"}, {text: "然后"}, {text: "现在"},
		{text: "改为"}, {text: "实际"}, {text: "不过"}, {text: "同时"}, {text: "接着"},
	} {
		for offset := 0; offset < len(lower); {
			relative := strings.Index(lower[offset:], literal.text)
			if relative < 0 {
				break
			}
			locStart := offset + relative
			locEnd := locStart + len(literal.text)
			if !literal.word || asciiLiteralHasWordBoundaries(lower, locStart, locEnd) {
				return true
			}
			offset = locStart + 1
		}
	}
	return false
}

func asciiLiteralHasWordBoundaries(text string, start int, end int) bool {
	return (start == 0 || !isASCIIWordByte(text[start-1])) && (end == len(text) || !isASCIIWordByte(text[end]))
}

func isASCIIWordByte(value byte) bool {
	return value == '_' || value >= '0' && value <= '9' || value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func authorizationBoundaryTextIsRestricted(text string, pattern compiledPattern) bool {
	matches := pattern.re.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return false
	}
	for _, loc := range matches {
		if len(loc) != 2 || loc[0] < 0 || loc[1] < loc[0] || loc[1] > len(text) {
			return false
		}
		matched := text[loc[0]:loc[1]]
		accessLoc := authorizationBoundaryAccessMatchPattern.FindStringIndex(matched)
		if len(accessLoc) != 2 || authorizationBoundaryHighRiskMatchPattern.MatchString(matched) {
			return false
		}
		sentence := matchSentence(text, loc[0], loc[1])
		explicitDenial := authorizationBoundaryExplicitDenialPattern.MatchString(sentence)
		if (authorizationBoundaryRequestCuePattern.MatchString(matched) || authorizationBoundaryActiveTargetPattern.MatchString(matched)) && !explicitDenial {
			return false
		}
		restricted := authorizationBoundaryRestrictionPattern.MatchString(sentence)
		if (!restricted && !explicitDenial) || authorizationBoundaryCancellationPattern.MatchString(sentence) {
			return false
		}
		// The built-in rule has bounded greedy spans and can consume a later
		// action into the same regex match. Resume after the first neutral access
		// token, not after loc[1], so a later permission change cannot disappear
		// inside that greedy match.
		if authorizationBoundaryHasActiveReactivation(text[loc[0]+accessLoc[1]:]) {
			return false
		}
	}
	return true
}

func authorizationBoundaryHasActiveReactivation(text string) bool {
	for _, loc := range authorizationBoundaryDirectPrivilegeRequestPattern.FindAllStringIndex(text, -1) {
		if len(loc) != 2 || loc[0] < 0 || loc[1] < loc[0] || loc[1] > len(text) {
			return true
		}
		tail := safeUTF8Prefix(text[loc[1]:], 128)
		if authorizationBoundaryReactivationCancellationTailPattern.MatchString(tail) {
			return true
		}
		if authorizationBoundaryReactivationDeniedPattern.MatchString(tail) {
			continue
		}
		return true
	}
	for _, loc := range authorizationBoundaryReactivationPattern.FindAllStringIndex(text, -1) {
		if len(loc) != 2 || loc[0] < 0 || loc[1] < loc[0] || loc[1] > len(text) {
			return true
		}
		actionLoc := authorizationBoundaryReactivationActionPattern.FindStringIndex(text[loc[0]:loc[1]])
		if len(actionLoc) != 2 {
			return true
		}
		// Bind engineering and denial semantics to the exact action just matched.
		// Looking arbitrarily farther into the sentence would let a later denied
		// action launder an earlier malicious permission change.
		tail := safeUTF8Prefix(text[loc[0]+actionLoc[1]:], 128)
		if authorizationBoundaryHasActivePrivilegeOutcome(tail) {
			return true
		}
		if authorizationBoundaryEngineeringTailPattern.MatchString(tail) {
			continue
		}
		if authorizationBoundaryReactivationCancellationTailPattern.MatchString(tail) {
			return true
		}
		if authorizationBoundaryReactivationDeniedPattern.MatchString(tail) {
			continue
		}
		return true
	}
	return false
}

func authorizationBoundaryHasActivePrivilegeOutcome(text string) bool {
	for _, loc := range authorizationBoundaryPrivilegeOutcomePattern.FindAllStringIndex(text, -1) {
		if len(loc) != 2 || loc[0] < 0 || loc[1] < loc[0] || loc[1] > len(text) {
			return true
		}
		tail := safeUTF8Prefix(text[loc[1]:], 128)
		if authorizationBoundaryReactivationCancellationTailPattern.MatchString(tail) {
			return true
		}
		if authorizationBoundaryReactivationDeniedPattern.MatchString(tail) {
			continue
		}
		return true
	}
	return false
}

func localMatchWindow(text string, start int, end int, radius int) string {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if end > len(text) {
		end = len(text)
	}
	windowStart := start - radius
	if windowStart < 0 {
		windowStart = 0
	}
	windowEnd := end + radius
	if windowEnd > len(text) {
		windowEnd = len(text)
	}
	return text[windowStart:windowEnd]
}

func defensiveArtifactText(text string) bool {
	if defensiveRuleArtifactPattern.MatchString(text) {
		return true
	}
	for _, pattern := range benignOperationalArtifactPatterns {
		if pattern.MatchString(text) {
			return true
		}
	}
	return false
}

func matchSentence(text string, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end < start {
		end = start
	}
	if end > len(text) {
		end = len(text)
	}
	left := 0
	if boundary := strings.LastIndexAny(text[:start], ".!?;。！？；\n"); boundary >= 0 {
		left = boundary + 1
	}
	right := len(text)
	if boundary := strings.IndexAny(text[end:], ".!?;。！？；\n"); boundary >= 0 {
		right = end + boundary
	}
	return text[left:right]
}

func compiledPatternMatchIndex(text string, pattern compiledPattern) []int {
	if isBuiltinMinorSafetyPattern(pattern) {
		return minorExploitationMatchIndex(text, pattern.re)
	}
	return compositePatternMatchIndex(text, pattern.re, pattern.all, pattern.any, pattern.exclude, pattern.cfg.MinMatches)
}

func compositePatternMatchIndex(text string, primary *regexp.Regexp, all, any, exclude []*regexp.Regexp, minimum int) []int {
	for _, re := range exclude {
		if re.MatchString(text) {
			return nil
		}
	}
	first := []int(nil)
	if primary != nil {
		first = primary.FindStringIndex(text)
		if first == nil {
			return nil
		}
	}
	for _, re := range all {
		loc := re.FindStringIndex(text)
		if loc == nil {
			return nil
		}
		if first == nil || loc[0] < first[0] {
			first = loc
		}
	}
	matchedAny := 0
	for _, re := range any {
		loc := re.FindStringIndex(text)
		if loc != nil {
			matchedAny++
			if first == nil || loc[0] < first[0] {
				first = loc
			}
		}
	}
	if minimum <= 0 && len(any) > 0 {
		minimum = 1
	}
	if matchedAny < minimum {
		return nil
	}
	return first
}

func minorExploitationMatchIndex(text string, re *regexp.Regexp) []int {
	if re == nil {
		return nil
	}
	scanContext := newMinorSafetyScanContext(text)
	for offset := 0; offset < len(text); {
		loc := minorSafetyNextCandidate(text[offset:], re)
		if len(loc) != 2 {
			return nil
		}
		start := offset + loc[0]
		end := offset + loc[1]
		skipTo := minorSafetyCandidateSuppressionEnd(scanContext, start, end)
		if skipTo == 0 {
			return []int{start, end}
		}
		if skipTo < end {
			skipTo = end
		}
		if skipTo > offset {
			offset = skipTo
			continue
		}
		offset++
	}
	return nil
}

// minorSafetyNextCandidate augments the built-in grammar with a few terse,
// high-confidence imperatives that should not require extra wording to become
// actionable. Keep this narrow: generic software phrases such as "create new
// children" must never be reintroduced as terminal matches.
func minorSafetyNextCandidate(text string, primary *regexp.Regexp) []int {
	var earliest []int
	for _, candidate := range []*regexp.Regexp{
		primary,
		minorSafetyDirectGroomAbuseCandidate,
		minorSafetyMaterialLinkCandidate,
	} {
		if candidate == nil {
			continue
		}
		loc := candidate.FindStringIndex(text)
		if len(loc) != 2 {
			continue
		}
		// Prefer the longest candidate at the same start. The primary grammar
		// may include a high-risk continuation (for example private messages)
		// that a terse prefix candidate does not, and suppression decisions need
		// to see that complete context.
		if earliest == nil || loc[0] < earliest[0] || loc[0] == earliest[0] && loc[1] > earliest[1] {
			earliest = loc
		}
	}
	return earliest
}

func minorSafetyCompactMaterialMatchIndex(text string) []int {
	scanContext := newMinorSafetyCompactScanContext(text)
	for offset := 0; offset < len(text); {
		loc := minorSafetyCompactMaterialAction.FindStringIndex(text[offset:])
		if len(loc) != 2 {
			return nil
		}
		start := offset + loc[0]
		end := offset + loc[1]
		tail := text[end:]
		relatedEnd := scanContext.relatedEnd(end)
		if safe := minorSafetyCompactSafeArtifactTail.FindStringIndex(tail); len(safe) == 2 && !scanContext.hasUnsafe(end, relatedEnd) && !scanContext.hasReactivation(end, relatedEnd) {
			offset = end + safe[1]
			continue
		}
		beforeStart := start - 256
		if beforeStart < 0 {
			beforeStart = 0
		}
		before := text[beforeStart:start]
		after := text[end:relatedEnd]
		if nonExecution := minorSafetyCompactNonExecutionTail.FindStringIndex(after); minorSafetyCompactReviewLead.MatchString(before) && len(nonExecution) == 2 && !scanContext.hasReactivation(end, relatedEnd) {
			offset = end + nonExecution[1]
			continue
		}
		if defensive := minorSafetyCompactDefensiveCodeTail.FindStringIndex(after); minorSafetyCompactDefensiveCodeLead.MatchString(before) && len(defensive) == 2 && !scanContext.hasReactivation(end, relatedEnd) {
			offset = end + defensive[1]
			continue
		}
		return []int{start, end}
	}
	return nil
}

type minorSafetyCompactScanContext struct {
	text                string
	reactivationStarts  []int
	crossResetRefStarts []int
	unsafeStarts        []int
	topicResetRanges    []minorSafetyQuoteRange
}

func newMinorSafetyCompactScanContext(text string) *minorSafetyCompactScanContext {
	context := &minorSafetyCompactScanContext{text: text}
	for _, match := range minorSafetyCompactReactivation.FindAllStringIndex(text, -1) {
		if len(match) == 2 {
			context.reactivationStarts = append(context.reactivationStarts, match[0])
		}
	}
	for _, match := range minorSafetyCompactCrossResetReference.FindAllStringIndex(text, -1) {
		if len(match) == 2 {
			context.crossResetRefStarts = append(context.crossResetRefStarts, match[0])
		}
	}
	for _, match := range minorSafetyCompactUnsafeTail.FindAllStringIndex(text, -1) {
		if len(match) == 2 {
			context.unsafeStarts = append(context.unsafeStarts, match[0])
		}
	}
	for _, match := range minorSafetyCompactTopicReset.FindAllStringIndex(text, -1) {
		if len(match) == 2 {
			context.topicResetRanges = append(context.topicResetRanges, minorSafetyQuoteRange{open: match[0], close: match[1]})
		}
	}
	return context
}

func (context *minorSafetyCompactScanContext) hasReactivation(start, limit int) bool {
	if context == nil || limit <= start {
		return false
	}
	index := sort.SearchInts(context.reactivationStarts, start)
	return index < len(context.reactivationStarts) && context.reactivationStarts[index] < limit
}

func (context *minorSafetyCompactScanContext) hasUnsafe(start, limit int) bool {
	if context == nil || limit <= start {
		return false
	}
	index := sort.SearchInts(context.unsafeStarts, start)
	return index < len(context.unsafeStarts) && context.unsafeStarts[index] < limit
}

func (context *minorSafetyCompactScanContext) hasCrossResetReference(start, limit int) bool {
	if context == nil || limit <= start {
		return false
	}
	index := sort.SearchInts(context.crossResetRefStarts, start)
	return index < len(context.crossResetRefStarts) && context.crossResetRefStarts[index] < limit
}

func (context *minorSafetyCompactScanContext) relatedEnd(end int) int {
	if context == nil || end < 0 || end > len(context.text) {
		return end
	}
	resetIndex := sort.Search(len(context.topicResetRanges), func(index int) bool {
		return context.topicResetRanges[index].open >= end
	})
	if resetIndex >= len(context.topicResetRanges) {
		return len(context.text)
	}
	reset := context.topicResetRanges[resetIndex]
	if minorSafetyCompactImmediateResetRef.MatchString(context.text[reset.close:]) || context.hasCrossResetReference(reset.close, len(context.text)) {
		return len(context.text)
	}
	return reset.open
}

func minorSafetyCompactRelatedTail(tail string) string {
	context := newMinorSafetyCompactScanContext(tail)
	return tail[:context.relatedEnd(0)]
}

func minorSafetyCandidateSuppressionEnd(scanContext *minorSafetyScanContext, start, end int) int {
	if scanContext == nil {
		return 0
	}
	text := scanContext.text
	if start < 0 || end < start || end > len(text) {
		return 0
	}
	tail := text[end:]
	if minorSafetyQuotedPolicyReview(scanContext, start, end) {
		// Suppression applies only to this quoted candidate. Jumping to the
		// closing quote can hide another active candidate embedded later in the
		// same quote, while reactivation after the quote is handled separately.
		return end
	}
	if minorSafetyNegatedOrNarrated(scanContext, start, end) {
		return end
	}
	if minorSafetyFixtureExample(scanContext, start, end) {
		return end
	}
	if minorSafetyDefensiveDiscussion(scanContext, start, end) {
		return end
	}
	relatedTail := scanContext.relatedTail(end)
	match := text[start:end]
	if minorSafetyDirectGroomAbuseCandidate.MatchString(match) {
		// "groom child tasks" and "abuse child processes" are ordinary
		// engineering phrases. The longer primary grammar still wins when the
		// text continues into an actual grooming method such as private messages.
		if minorSafetyEngineeringEntityTail.MatchString(tail) || strings.HasPrefix(strings.ToLower(strings.TrimSpace(tail)), "processes") {
			return end
		}
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(match)), "groom") && minorSafetyBenignGroomingTail.MatchString(tail) {
			return end
		}
	}
	if minorSafetyAmbiguousTargetModifier(text[start:end], tail) && !minorSafetyExplicitHumanTail.MatchString(relatedTail) {
		return end
	}
	if minorSafetyMediaCandidate.MatchString(text[start:end]) && minorSafetyEngineeringEntityTail.MatchString(tail) && !minorSafetyExplicitHumanTail.MatchString(relatedTail) {
		return end
	}
	if minorSafetyIllegalMaterialHit.MatchString(text[start:end]) && minorSafetySafeArtifactTail.MatchString(tail) && !minorSafetyUnsafeArtifactTail.MatchString(minorSafetySameSentenceTail(text, end)) && !minorSafetyUnsafeArtifactReference(relatedTail) && !scanContext.hasReactivation(end) {
		return end
	}
	return 0
}

func minorSafetySentenceScopeEnd(text string, end int) int {
	if end < 0 || end > len(text) {
		return end
	}
	if boundary := minorSafetySentenceBoundary.FindStringIndex(text[end:]); len(boundary) == 2 {
		return end + boundary[1]
	}
	return len(text)
}

func minorSafetyLineScopeEnd(text string, end int) int {
	if end < 0 || end > len(text) {
		return end
	}
	if newline := strings.IndexByte(text[end:], '\n'); newline >= 0 {
		return end + newline + 1
	}
	return len(text)
}

func minorSafetyUnsafeArtifactReference(text string) bool {
	if !minorSafetyUnsafeArtifactAnaphora.MatchString(text) {
		return false
	}
	if minorSafetySafeVisualArtifactContext.MatchString(text) && !minorSafetyIllegalMaterialHit.MatchString(text) {
		return false
	}
	return true
}

func minorSafetyAmbiguousTargetModifier(match, tail string) bool {
	if !minorSafetyAmbiguousTargetSuffix.MatchString(match) {
		return false
	}
	parts := minorSafetyTrailingWord.FindStringSubmatch(tail)
	if len(parts) != 2 {
		return false
	}
	return minorSafetyNonHumanTargetWord.MatchString(parts[1])
}

func minorSafetySameSentenceTail(text string, end int) string {
	if end < 0 || end > len(text) {
		return ""
	}
	tail := text[end:]
	if boundary := minorSafetySentenceBoundary.FindStringIndex(tail); len(boundary) == 2 {
		return tail[:boundary[0]]
	}
	return tail
}

func minorSafetyRelatedTail(text string, end int) string {
	return newMinorSafetyScanContext(text).relatedTail(end)
}

func minorSafetyDefensiveDiscussion(scanContext *minorSafetyScanContext, start, end int) bool {
	text := scanContext.text
	left := 0
	if boundary := strings.LastIndexAny(text[:start], ".!?;。！？；\n"); boundary >= 0 {
		left = boundary + 1
	}
	right := len(text)
	if boundary := strings.IndexAny(text[end:], ".!?;。！？；\n"); boundary >= 0 {
		right = end + boundary
	}
	before := text[left:start]
	if !minorSafetyDefensiveLead.MatchString(before) || minorSafetyExecutionBridge.MatchString(before) || minorSafetyHowToLead.MatchString(before) {
		return false
	}
	after := text[end:right]
	if !minorSafetyDefensiveObject.MatchString(before + " " + after) {
		return false
	}
	return !scanContext.hasReactivation(end)
}

type minorSafetyQuoteRange struct {
	open  int
	close int
}

type minorSafetyQuoteReview struct {
	known    bool
	eligible bool
}

type minorSafetyScanContext struct {
	text                string
	quoteRanges         []minorSafetyQuoteRange
	reactivationStarts  []int
	crossResetRefStarts []int
	reactivationIndexed bool
	relationIndexed     bool
	blankLineStarts     []int
	topicResetRanges    []minorSafetyQuoteRange
	quoteReviewCache    map[minorSafetyQuoteRange]minorSafetyQuoteReview
}

func newMinorSafetyScanContext(text string) *minorSafetyScanContext {
	context := &minorSafetyScanContext{text: text}
	if strings.ContainsAny(text, "\"'`“”「」『』") {
		for _, pair := range [][2]string{{"“", "”"}, {"「", "」"}, {"『", "』"}} {
			for offset := 0; offset < len(text); {
				openRelative := strings.Index(text[offset:], pair[0])
				if openRelative < 0 {
					break
				}
				open := offset + openRelative
				closeRelative := strings.Index(text[open+len(pair[0]):], pair[1])
				if closeRelative < 0 {
					break
				}
				close := open + len(pair[0]) + closeRelative + len(pair[1])
				context.quoteRanges = append(context.quoteRanges, minorSafetyQuoteRange{open: open, close: close})
				offset = close
			}
		}
		for _, quote := range []byte{'"', '\'', '`'} {
			open := -1
			for index := 0; index < len(text); index++ {
				if text[index] != quote || minorSafetyQuoteEscaped(text, index) || quote == '\'' && !minorSafetySingleQuoteDelimiter(text, index) {
					continue
				}
				if open < 0 {
					open = index
					continue
				}
				context.quoteRanges = append(context.quoteRanges, minorSafetyQuoteRange{open: open, close: index + 1})
				open = -1
			}
		}
		sort.Slice(context.quoteRanges, func(i, j int) bool {
			if context.quoteRanges[i].open == context.quoteRanges[j].open {
				return context.quoteRanges[i].close < context.quoteRanges[j].close
			}
			return context.quoteRanges[i].open < context.quoteRanges[j].open
		})
	}
	return context
}

func minorSafetySingleQuoteDelimiter(text string, index int) bool {
	if index < 0 || index >= len(text) || text[index] != '\'' {
		return false
	}
	previousIsWord := index > 0 && isASCIIAlphaNumeric(text[index-1])
	nextIsWord := index+1 < len(text) && isASCIIAlphaNumeric(text[index+1])
	// Apostrophes in contractions and possessives (don't, user's) are not quote
	// delimiters. A quote next to a word on only one side remains valid.
	return !previousIsWord || !nextIsWord
}

func minorSafetyQuoteEscaped(text string, index int) bool {
	backslashes := 0
	for index--; index >= 0 && text[index] == '\\'; index-- {
		backslashes++
	}
	return backslashes%2 == 1
}

func (context *minorSafetyScanContext) quoteSpan(start, end int) (int, int, bool) {
	if context == nil || start <= 0 || end < start || end > len(context.text) || len(context.quoteRanges) == 0 {
		return 0, 0, false
	}
	index := sort.Search(len(context.quoteRanges), func(index int) bool {
		return context.quoteRanges[index].open >= start
	}) - 1
	for ; index >= 0; index-- {
		span := context.quoteRanges[index]
		if span.close >= end {
			return span.open, span.close, true
		}
		// Earlier non-nested spans cannot contain a candidate after this one.
		if span.close <= span.open || span.close <= start {
			break
		}
	}
	return 0, 0, false
}

func (context *minorSafetyScanContext) quoteReviewWindow(open, close int) (string, string, bool) {
	if context == nil || open < 0 || close <= open || close > len(context.text) {
		return "", "", false
	}
	text := context.text
	left := 0
	if boundary := strings.LastIndexAny(text[:open], ".!?。！？\n"); boundary >= 0 {
		left = boundary + 1
	}
	if open-left > 256 {
		left = open - 256
	}
	right := len(text)
	if boundary := minorSafetySentenceBoundary.FindStringIndex(text[close:]); len(boundary) == 2 {
		right = close + boundary[1]
		if right < len(text) {
			nextRight := len(text)
			if nextBoundary := minorSafetySentenceBoundary.FindStringIndex(text[right:]); len(nextBoundary) == 2 {
				nextRight = right + nextBoundary[1]
			}
			nextSentence := text[right:nextRight]
			if quotedPolicyNonExecutionPattern.MatchString(nextSentence) || minorSafetyStandaloneProhibition.MatchString(nextSentence) {
				right = nextRight
			}
		}
	}
	if right-close > 256 {
		right = close + 256
	}
	return text[left:right], text[left:open], true
}

func (context *minorSafetyScanContext) quotedReviewEligible(start, end int) bool {
	open, close, ok := context.quoteSpan(start, end)
	if !ok {
		return false
	}
	key := minorSafetyQuoteRange{open: open, close: close}
	if cached, exists := context.quoteReviewCache[key]; exists && cached.known {
		return cached.eligible
	}
	window, lead, ok := context.quoteReviewWindow(open, close)
	eligible := false
	if ok && !minorSafetyQuotedExecutionLead.MatchString(lead) {
		reviewWithNonExecution := (quotedPolicyReviewContextPattern.MatchString(window) || minorSafetyLocalReviewLead.MatchString(lead)) && quotedPolicyNonExecutionPattern.MatchString(window)
		if reviewWithNonExecution || minorSafetyQuotedProhibitionContext.MatchString(window) {
			// Content inside the quote is evidence. Reactivation must occur after
			// the closing quote before it can turn the sample back into a request.
			eligible = !context.hasReactivation(close)
		}
	}
	if context.quoteReviewCache == nil {
		context.quoteReviewCache = make(map[minorSafetyQuoteRange]minorSafetyQuoteReview)
	}
	context.quoteReviewCache[key] = minorSafetyQuoteReview{known: true, eligible: eligible}
	return eligible
}

func (context *minorSafetyScanContext) ensureReactivations() {
	if context == nil || context.reactivationIndexed {
		return
	}
	context.reactivationIndexed = true
	patterns := []*regexp.Regexp{
		minorSafetyExplicitReviewReactivation,
		minorSafetySequencedReactivation,
		minorSafetyDirectReactivation,
		minorSafetyReferentialReactivation,
	}
	for _, pattern := range patterns {
		for _, match := range pattern.FindAllStringIndex(context.text, -1) {
			if len(match) != 2 || match[0] < 0 || match[1] > len(context.text) {
				continue
			}
			if minorSafetyReactivationNegated.MatchString(context.text[match[0]:match[1]]) {
				continue
			}
			context.reactivationStarts = append(context.reactivationStarts, match[0])
			if pattern == minorSafetyExplicitReviewReactivation {
				context.crossResetRefStarts = append(context.crossResetRefStarts, match[0])
			}
		}
	}
	sort.Ints(context.reactivationStarts)
	if len(context.reactivationStarts) < 2 {
		return
	}
	write := 1
	for read := 1; read < len(context.reactivationStarts); read++ {
		if context.reactivationStarts[read] == context.reactivationStarts[write-1] {
			continue
		}
		context.reactivationStarts[write] = context.reactivationStarts[read]
		write++
	}
	context.reactivationStarts = context.reactivationStarts[:write]
}

func (context *minorSafetyScanContext) ensureRelations() {
	if context == nil || context.relationIndexed {
		return
	}
	context.relationIndexed = true
	for offset := 0; offset < len(context.text); {
		relative := strings.Index(context.text[offset:], "\n\n")
		if relative < 0 {
			break
		}
		absolute := offset + relative
		context.blankLineStarts = append(context.blankLineStarts, absolute)
		offset = absolute + 2
	}
	for _, match := range minorSafetyTopicReset.FindAllStringIndex(context.text, -1) {
		if len(match) == 2 {
			context.topicResetRanges = append(context.topicResetRanges, minorSafetyQuoteRange{open: match[0], close: match[1]})
		}
	}
}

func (context *minorSafetyScanContext) relatedTail(end int) string {
	if context == nil || end < 0 || end > len(context.text) {
		return ""
	}
	context.ensureRelations()
	cut := len(context.text)
	blankIndex := sort.SearchInts(context.blankLineStarts, end)
	if blankIndex < len(context.blankLineStarts) {
		cut = context.blankLineStarts[blankIndex]
	}
	resetIndex := sort.Search(len(context.topicResetRanges), func(index int) bool {
		return context.topicResetRanges[index].open >= end
	})
	var reset minorSafetyQuoteRange
	hasReset := false
	if resetIndex < len(context.topicResetRanges) {
		reset = context.topicResetRanges[resetIndex]
		hasReset = true
	}
	// A reset can begin exactly at the candidate tail through the regexp's ^
	// branch, while its full-text match would otherwise need preceding punctuation.
	localEnd := end + 512
	if localEnd > len(context.text) {
		localEnd = len(context.text)
	}
	if local := minorSafetyTopicReset.FindStringIndex(context.text[end:localEnd]); len(local) == 2 {
		localReset := minorSafetyQuoteRange{open: end + local[0], close: end + local[1]}
		if !hasReset || localReset.open < reset.open {
			reset = localReset
			hasReset = true
		}
	}
	if hasReset && reset.open < cut {
		crossResetReference := minorSafetyImmediateCrossResetRef.MatchString(context.text[reset.close:]) || context.hasIndexedCrossResetReferenceBetween(reset.close, cut)
		if !crossResetReference {
			cut = reset.open
		}
	}
	if cut < end {
		cut = end
	}
	return context.text[end:cut]
}

func (context *minorSafetyScanContext) hasReactivation(end int) bool {
	if context == nil || end < 0 || end > len(context.text) {
		return false
	}
	tail := context.relatedTail(end)
	if tail == "" {
		return false
	}
	limit := end + len(tail)
	context.ensureReactivations()
	index := sort.SearchInts(context.reactivationStarts, end)
	if index < len(context.reactivationStarts) && context.reactivationStarts[index] < limit {
		return true
	}
	// Patterns with a ^ alternative can match exactly at the candidate tail even
	// when the same text has no standalone boundary in the full request. Keep a
	// small local fallback for that case; the expensive long-range scan is shared.
	anchored := tail
	if len(anchored) > 256 {
		anchored = anchored[:256]
	}
	if !minorSafetyAnchoredReactivationPossible(anchored) {
		return false
	}
	return minorSafetyHasActivePattern(anchored, minorSafetyExplicitReviewReactivation) ||
		minorSafetyHasActivePattern(anchored, minorSafetySequencedReactivation) ||
		minorSafetyHasActivePattern(anchored, minorSafetyDirectReactivation) ||
		minorSafetyHasActivePattern(anchored, minorSafetyReferentialReactivation)
}

func (context *minorSafetyScanContext) hasIndexedReactivationBetween(start, limit int) bool {
	if context == nil || start < 0 || limit <= start {
		return false
	}
	context.ensureReactivations()
	index := sort.SearchInts(context.reactivationStarts, start)
	return index < len(context.reactivationStarts) && context.reactivationStarts[index] < limit
}

func (context *minorSafetyScanContext) hasIndexedCrossResetReferenceBetween(start, limit int) bool {
	if context == nil || start < 0 || limit <= start {
		return false
	}
	context.ensureReactivations()
	index := sort.SearchInts(context.crossResetRefStarts, start)
	return index < len(context.crossResetRefStarts) && context.crossResetRefStarts[index] < limit
}

func minorSafetyAnchoredReactivationPossible(text string) bool {
	text = strings.TrimLeft(text, " \t\r\n,;.!?:，；。！？：")
	if text == "" {
		return false
	}
	lower := strings.ToLower(text)
	for _, negative := range []string{"do not ", "don't ", "never ", "must not ", "should not ", "cannot ", "can't ", "不要", "不得", "禁止", "切勿", "不应", "不能"} {
		if strings.HasPrefix(lower, negative) {
			return false
		}
	}
	for _, prefix := range []string{
		"then ", "next ", "after ", "afterwards ", "subsequently ", "but ", "instead ", "and then ", "so ", "therefore ",
		"just ", "actually ", "now ", "do ", "execute ", "apply ", "follow ", "obey ", "perform ", "generate ", "create ",
		"share ", "send ", "provide ", "give ", "produce ", "make ", "download ", "upload ", "forward ", "relay ",
		"reproduce ", "replicate ", "carry ", "proceed ", "go ahead", "answer ", "respond ", "comply ", "fulfill ",
		"fulfil ", "honor ", "honour ", "use ", "然后", "接着", "之后", "随后", "再", "但是", "改为", "所以", "因此",
		"只要", "实际", "直接", "立即", "现在", "照做", "照办", "执行", "遵循", "服从", "生成", "制作", "发送", "分享",
		"上传", "下载", "提供", "转发", "转交", "复现", "复制", "完成", "回答",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func minorSafetyQuotedPolicyReview(scanContext *minorSafetyScanContext, start, end int) bool {
	return scanContext != nil && scanContext.quotedReviewEligible(start, end)
}

func encodedBlockReviewOnly(text string, start, end int) bool {
	return encodedBlockReviewOnlyWithContext(newMinorSafetyScanContext(text), start, end)
}

func encodedBlockReviewOnlyWithContext(scanContext *minorSafetyScanContext, start, end int) bool {
	return scanContext != nil && scanContext.quotedReviewEligible(start, end)
}

func minorSafetyQuotedReviewContext(scanContext *minorSafetyScanContext, start, end int) (string, string, bool) {
	if scanContext == nil {
		return "", "", false
	}
	open, close, ok := scanContext.quoteSpan(start, end)
	if !ok {
		return "", "", false
	}
	return scanContext.quoteReviewWindow(open, close)
}

func minorSafetyQuoteSpan(text string, start, end int) (int, int, bool) {
	return newMinorSafetyScanContext(text).quoteSpan(start, end)
}

func minorSafetyHasReactivation(text string, end int) bool {
	return newMinorSafetyScanContext(text).hasReactivation(end)
}

func minorSafetyHasActivePattern(text string, pattern *regexp.Regexp) bool {
	for _, match := range pattern.FindAllStringIndex(text, -1) {
		if len(match) != 2 || match[0] < 0 || match[1] > len(text) {
			continue
		}
		if !minorSafetyReactivationNegated.MatchString(text[match[0]:match[1]]) {
			return true
		}
	}
	return false
}

func minorSafetyNegatedOrNarrated(scanContext *minorSafetyScanContext, start, end int) bool {
	text := scanContext.text
	left := 0
	if boundary := strings.LastIndexAny(text[:start], ".!?;。！？；\n"); boundary >= 0 {
		left = boundary + 1
	}
	before := text[left:start]
	if minorSafetyNegationCancellation.MatchString(before) {
		return false
	}
	if minorSafetyNegationLead.MatchString(before) {
		if minorSafetyNegationExceptionTail.MatchString(minorSafetySameSentenceTail(text, end)) {
			return false
		}
		return !scanContext.hasReactivation(end)
	}
	if !minorSafetyNarrativeLead.MatchString(before) || minorSafetyDirectRequestLead.MatchString(before) {
		return false
	}
	return !scanContext.hasReactivation(end)
}

func minorSafetyFixtureExample(scanContext *minorSafetyScanContext, start, end int) bool {
	text := scanContext.text
	if fixture, active := minorSafetyJSONFixtureStatus(text, start, end); fixture {
		if active {
			return false
		}
		return !scanContext.hasReactivation(end)
	}
	jsonStart := start - 512
	if jsonStart < 0 {
		jsonStart = 0
	}
	jsonEnd := end + 512
	if jsonEnd > len(text) {
		jsonEnd = len(text)
	}
	if minorSafetyJSONFixtureLead.MatchString(text[jsonStart:start]) && minorSafetyJSONFixtureExpected.MatchString(text[end:jsonEnd]) {
		return !scanContext.hasReactivation(end)
	}
	prefixStart := 0
	if start > 4096 {
		prefixStart = start - 4096
	}
	prefix := text[prefixStart:start]
	if paragraph := strings.LastIndex(prefix, "\n\n"); paragraph >= 0 {
		prefix = prefix[paragraph+2:]
	}
	if !minorSafetyFixtureLabel.MatchString(prefix) {
		return false
	}
	if minorSafetyFixtureHasActiveExecution(prefix) {
		return false
	}
	lineEnd := len(text)
	if newline := strings.IndexByte(text[end:], '\n'); newline >= 0 {
		lineEnd = end + newline
	}
	remainder := text[end:lineEnd]
	if !minorSafetyFixtureRemainder.MatchString(remainder) && !minorSafetyFixtureExpectedRemainder.MatchString(remainder) {
		return false
	}
	return !scanContext.hasReactivation(end)
}

func minorSafetyJSONFixtureStatus(text string, start, end int) (fixture, active bool) {
	if start < 0 || end < start || end > len(text) {
		return false, false
	}
	left := strings.LastIndexByte(text[:start], '{')
	if left < 0 {
		return false, false
	}
	rightRelative := strings.IndexByte(text[end:], '}')
	if rightRelative < 0 {
		return false, false
	}
	right := end + rightRelative + 1
	if right-left > 4096 {
		return false, false
	}

	var object map[string]any
	if err := json.Unmarshal([]byte(text[left:right]), &object); err != nil {
		return false, false
	}
	needle := strings.ToLower(text[start:end])
	inputFound := false
	for _, key := range []string{"input", "prompt", "text", "request"} {
		value, ok := object[key].(string)
		if ok && strings.Contains(strings.ToLower(value), needle) {
			inputFound = true
			break
		}
	}
	if !inputFound {
		return false, false
	}
	expectedBlock := false
	for _, key := range []string{"expected", "action", "result"} {
		value, ok := object[key].(string)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "block", "blocked", "deny", "denied", "reject", "rejected", "violation", "unsafe":
			expectedBlock = true
		}
	}
	if !expectedBlock {
		return false, false
	}
	for _, key := range []string{"instruction", "instructions", "directive"} {
		value, ok := object[key].(string)
		if ok && minorSafetyExecutionScopeActive(value, minorSafetyJSONFixtureExecution) {
			return true, true
		}
	}
	return true, false
}

func minorSafetyFixtureHasActiveExecution(prefix string) bool {
	label := minorSafetyFixtureLabel.FindStringIndex(prefix)
	if len(label) != 2 {
		return false
	}
	localStart := 0
	if boundary := strings.LastIndexAny(prefix[:label[0]], ".!?。！？\n"); boundary >= 0 {
		localStart = boundary + 1
	}
	if minorSafetyExecutionScopeActive(prefix[localStart:], minorSafetyFixtureExecutionLead) {
		return true
	}
	return minorSafetyExecutionScopeActive(prefix, minorSafetyFixtureExplicitExecution)
}

func minorSafetyExecutionScopeActive(scope string, executionPattern *regexp.Regexp) bool {
	executions := executionPattern.FindAllStringIndex(scope, -1)
	if len(executions) == 0 {
		return false
	}
	nonExecutions := minorSafetyFixtureNonExecutionLead.FindAllStringIndex(scope, -1)
	for i := len(executions) - 1; i >= 0; i-- {
		execution := executions[i]
		governed := false
		for _, nonExecution := range nonExecutions {
			if nonExecution[0] <= execution[0] && nonExecution[1] >= execution[0] {
				governed = true
				break
			}
		}
		if !governed {
			return true
		}
	}
	return false
}

func ExtractText(body []byte, endpoint string, maxLen int) string {
	envelope := BuildEnvelope(body, endpoint, "", TransportHTTP, maxLen)
	parts := make([]string, 0, 2)
	for _, segment := range envelope.Segments {
		if segment.Origin != OriginCurrentUser && !(segment.Origin == OriginHistory && segment.Linked) {
			continue
		}
		if text := strings.TrimSpace(segment.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return limitScanText(strings.Join(parts, "\n"), maxLen)
}

var continuationOnlyPattern = regexp.MustCompile(`(?i)^(?:(?:(?:请|麻烦)\s*)?继续(?:一下|吧|做|处理|执行|完成|生成|写)?(?:它|这个|上面(?:的)?内容|之前(?:的)?内容)?|接着(?:做|处理|执行)?|照做|按(?:上面|之前|刚才)(?:的)?(?:要求|内容|方案)?(?:继续)?(?:做|执行|处理)?|就这样做|continue(?:\s+(?:please|with\s+(?:that|it)))?|go\s+ahead|do\s+it|proceed(?:\s+with\s+it)?|carry\s+on|same\s+as\s+above|продолж(?:и|ай|айте)(?:\s+(?:это|с\s+этим))?|сделай\s+это|действуй|contin[uú]a(?:\s+con\s+(?:eso|esto))?|sigue(?:\s+adelante)?|hazlo|continuez?|vas[-\s]?y|fais[-\s]?le|mach\s+weiter|mach\s+es|weiter(?:machen)?|続けて|続行して|そのまま続けて|계속해(?:요)?|계속하세요|진행해(?:요)?)[。.!！?？…\s]*$`)

// IsContinuationOnly reports whether text is only a short instruction to
// continue the immediately preceding user intent. Protocol adapters and
// session correlation share this classifier so their language coverage cannot
// drift apart.
func IsContinuationOnly(text string) bool {
	return isContinuationOnly(text)
}

func isContinuationOnly(text string) bool {
	text = strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(text)), " "))
	if text == "" || utf8.RuneCountInString(text) > 80 {
		return false
	}
	return continuationOnlyPattern.MatchString(text)
}

func Preview(text string, maxRunes int) string {
	text = strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "..."
}

func RedactedPreview(text string, maxRunes int) string {
	return Preview(RedactSensitive(text), maxRunes)
}

func RedactSensitive(text string) string {
	if text == "" {
		return ""
	}
	redacted := text
	for _, pattern := range sensitiveRedactionPatterns {
		redacted = pattern.re.ReplaceAllString(redacted, pattern.replacement)
	}
	return redacted
}

func sensitiveWordMatchIndex(text string, literalHits map[string]bool, word string) []int {
	if word == "" {
		return nil
	}
	if literalHits != nil && !literalHits[word] {
		return nil
	}

	requireBoundary := isASCIIBoundedTerm(word)
	for offset := 0; offset <= len(text)-len(word); {
		relative := strings.Index(text[offset:], word)
		if relative < 0 {
			return nil
		}
		start := offset + relative
		end := start + len(word)
		if !requireBoundary || hasASCIIWordBoundaries(text, start, end) {
			return []int{start, end}
		}
		offset = start + 1
	}
	return nil
}

func isASCIIBoundedTerm(text string) bool {
	if text == "" || !isASCIIAlphaNumeric(text[0]) || !isASCIIAlphaNumeric(text[len(text)-1]) {
		return false
	}
	for i := 0; i < len(text); i++ {
		if text[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

func hasASCIIWordBoundaries(text string, start int, end int) bool {
	if start > 0 && isASCIIIdentifierByte(text[start-1]) {
		return false
	}
	if end < len(text) && isASCIIIdentifierByte(text[end]) {
		return false
	}
	return true
}

func isASCIIIdentifierByte(value byte) bool {
	return isASCIIAlphaNumeric(value) || value == '_'
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func regexMatchContext(text string, loc []int) (string, string) {
	if len(loc) != 2 {
		return "", ""
	}
	return matchContext(text, loc[0], loc[1])
}

func matchContext(text string, start int, end int) (string, string) {
	if start < 0 || end < start || start > len(text) {
		return "", ""
	}
	if end > len(text) {
		end = len(text)
	}
	sample := RedactedPreview(text[start:end], 120)
	contextStart := byteOffsetBefore(text, start, 80)
	contextEnd := byteOffsetAfter(text, end, 80)
	rawContext := strings.TrimSpace(text[contextStart:contextEnd])
	redactedContext := RedactSensitive(rawContext)
	if redactedContext != rawContext {
		if contextStart > 0 {
			redactedContext = "..." + redactedContext
		}
		return sample, redactedContext
	}

	before := strings.TrimSpace(text[contextStart:start])
	hit := text[start:end]
	after := strings.TrimSpace(text[end:contextEnd])
	parts := make([]string, 0, 3)
	if before != "" {
		parts = append(parts, before)
	}
	parts = append(parts, HitStartMarker+hit+HitEndMarker)
	if after != "" {
		parts = append(parts, after)
	}
	context := strings.Join(parts, " ")
	if contextStart > 0 {
		context = "..." + context
	}
	return sample, context
}

func byteOffsetBefore(text string, start int, maxRunes int) int {
	if start <= 0 || maxRunes <= 0 {
		return start
	}
	offsets := make([]int, 0, maxRunes+1)
	for idx := range text[:start] {
		offsets = append(offsets, idx)
	}
	if len(offsets) <= maxRunes {
		return 0
	}
	return offsets[len(offsets)-maxRunes]
}

func byteOffsetAfter(text string, end int, maxRunes int) int {
	if end >= len(text) || maxRunes <= 0 {
		return end
	}
	count := 0
	for idx := range text[end:] {
		if count >= maxRunes {
			return end + idx
		}
		count++
	}
	return len(text)
}

func MatchesJSON(matches []Match) string {
	if len(matches) == 0 {
		return "[]"
	}
	data, err := json.Marshal(matches)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func parseSensitiveWords(raw string) []string {
	lines := strings.Split(raw, "\n")
	seen := map[string]struct{}{}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = normalizeForScan(strings.TrimSpace(line))
		if line == "" {
			continue
		}
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		out = append(out, line)
	}
	return out
}

func normalizePatternNames(names []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}

func disabledPatternSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			out[strings.ToLower(name)] = true
		}
	}
	return out
}

func normalizeForScan(text string) string {
	text = strings.ReplaceAll(text, "```", " ")
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return ' '
		}
		return unicode.ToLower(r)
	}, text)
	return strings.Join(strings.Fields(text), " ")
}

func limitScanText(text string, maxLen int) string {
	if maxLen <= 0 {
		maxLen = DefaultMaxTextLength
	}
	if len(text) <= maxLen {
		return text
	}
	head := defaultHeadScanLength
	tail := defaultTailScanLength
	if maxLen < head+tail {
		head = maxLen * 4 / 5
		tail = maxLen - head
	}
	if head > len(text) {
		head = len(text)
	}
	if tail > len(text)-head {
		tail = len(text) - head
	}
	return safeUTF8Prefix(text, head) + "\n" + safeUTF8Suffix(text, tail)
}

func safeUTF8Prefix(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if maxBytes >= len(text) {
		return text
	}
	for maxBytes > 0 && !utf8.ValidString(text[:maxBytes]) {
		maxBytes--
	}
	return text[:maxBytes]
}

func safeUTF8Suffix(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if maxBytes >= len(text) {
		return text
	}
	start := len(text) - maxBytes
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}

func defensiveContextDiscount(text string, scanTexts []string, cfg ContextDiscountConfig, highConfidenceMatch bool) int {
	if !cfg.Enabled {
		return 0
	}
	discount := 0
	for _, pattern := range defensiveContextPatterns {
		if pattern.MatchString(text) {
			discount += 30
		}
	}
	if discount > cfg.MaxDiscount {
		discount = cfg.MaxDiscount
	}
	if cfg.IntentAware && highConfidenceMatch && hasExplicitOperationalIntent(scanTexts) && discount > cfg.OperationalMaxDiscount {
		discount = cfg.OperationalMaxDiscount
	}
	return discount
}

func hasExplicitOperationalIntent(scanTexts []string) bool {
	for _, text := range scanTexts {
		quotedRequestReactivated := quotedPolicyReactivationPattern.MatchString(text)
		for _, pattern := range operationalRequestPatterns {
			for _, loc := range pattern.FindAllStringIndex(text, -1) {
				start, end := loc[0], loc[1]
				if operationalRequestIsNegated(text, start, end) ||
					(operationalRequestIsQuoted(text, start) && !quotedRequestReactivated) ||
					operationalRequestIsDefensiveArtifact(text, start, end) {
					continue
				}
				return true
			}
		}
	}
	return false
}

func operationalRequestIsNegated(text string, start, end int) bool {
	probeEnd := end
	if probeEnd > len(text) {
		probeEnd = len(text)
	}
	probe := text[start:probeEnd]
	for _, pattern := range operationalNegationPatterns {
		if pattern.MatchString(probe) {
			return true
		}
	}
	return false
}

func operationalRequestIsDefensiveArtifact(text string, start, end int) bool {
	windowStart := start - 16
	if windowStart < 0 {
		windowStart = 0
	}
	windowEnd := end + 112
	if windowEnd > len(text) {
		windowEnd = len(text)
	}
	window := text[windowStart:windowEnd]
	for _, pattern := range benignOperationalArtifactPatterns {
		if pattern.MatchString(window) {
			return true
		}
	}
	return false
}

func operationalRequestIsQuoted(text string, start int) bool {
	if start <= 0 || start > len(text) {
		return false
	}
	prefix := text[:start]
	for _, pair := range [][2]string{{"“", "”"}, {"「", "」"}, {"『", "』"}} {
		if strings.LastIndex(prefix, pair[0]) > strings.LastIndex(prefix, pair[1]) {
			return true
		}
	}
	for _, quote := range []byte{'"', '`'} {
		count := 0
		for i := 0; i < len(prefix); i++ {
			if prefix[i] == quote && (i == 0 || prefix[i-1] != '\\') {
				count++
			}
		}
		if count%2 == 1 {
			return true
		}
	}
	return false
}

func reasonForVerdict(action string, score int, threshold int, matches []Match) string {
	if len(matches) == 0 {
		return ""
	}
	names := make([]string, 0, len(matches))
	for i, match := range matches {
		if i >= 3 {
			break
		}
		names = append(names, match.Name)
	}
	if action == ActionBlock {
		return fmt.Sprintf("prompt blocked: score %d >= %d (%s)", score, threshold, strings.Join(names, ", "))
	}
	if action == ActionWarn {
		return fmt.Sprintf("prompt warning: score %d >= %d (%s)", score, threshold, strings.Join(names, ", "))
	}
	return fmt.Sprintf("prompt matched: score %d (%s)", score, strings.Join(names, ", "))
}
