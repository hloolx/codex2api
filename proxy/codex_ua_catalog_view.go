package proxy

import (
	"errors"
	"fmt"
	"strings"
)

// ==================== 管理端视图 ====================

type CodexUserAgentCatalogOption struct {
	Value  string `json:"value"`
	Weight int    `json:"weight"`
}

type CodexUserAgentCatalogPlatform struct {
	OSName    string `json:"os_name"`
	OSVersion string `json:"os_version"`
	Arch      string `json:"arch"`
	Weight    int    `json:"weight"`
}

type CodexUserAgentCatalogVersionPair struct {
	CLIVersion string `json:"cli_version"`
	AppVersion string `json:"app_version"`
	Weight     int    `json:"weight"`
}

type CodexUserAgentCatalogKind struct {
	Kind               string                             `json:"kind"`
	ClientName         string                             `json:"client_name"`
	AppFollowsCLI      bool                               `json:"app_follows_cli"`
	DefaultAppName     string                             `json:"default_app_name"`
	DefaultPlatform    CodexUserAgentCatalogPlatform      `json:"default_platform"`
	DefaultTerminal    string                             `json:"default_terminal"`
	AppNames           []CodexUserAgentCatalogOption      `json:"app_names"`
	Terminals          []CodexUserAgentCatalogOption      `json:"terminals"`
	ReferenceTerminals []string                           `json:"reference_terminals"`
	Platforms          []CodexUserAgentCatalogPlatform    `json:"platforms"`
	ReferencePlatforms []CodexUserAgentCatalogPlatform    `json:"reference_platforms"`
	VersionPairs       []CodexUserAgentCatalogVersionPair `json:"version_pairs"`
}

type CodexUserAgentCatalogView struct {
	Kinds          []CodexUserAgentCatalogKind `json:"kinds"`
	DefaultPoolMix map[string]int              `json:"default_pool_mix"`
}

// CodexUserAgentCatalog 返回管理页构建搭配选择所需的目录快照。
func CodexUserAgentCatalog() CodexUserAgentCatalogView {
	view := CodexUserAgentCatalogView{DefaultPoolMix: map[string]int{}}
	for kind, w := range defaultCodexUAPoolMix {
		view.DefaultPoolMix[string(kind)] = w
	}
	for _, kind := range codexUAKindOrder {
		spec := codexUACatalog[kind]
		item := CodexUserAgentCatalogKind{
			Kind:            string(spec.Kind),
			ClientName:      spec.ClientName,
			AppFollowsCLI:   spec.AppFollowsCLI,
			DefaultAppName:  spec.ClientName,
			DefaultPlatform: CodexUserAgentCatalogPlatform{OSName: spec.DefaultPlatform.OSName, OSVersion: spec.DefaultPlatform.OSVersion, Arch: spec.DefaultPlatform.Arch},
			DefaultTerminal: spec.DefaultTerminal,
		}
		if !spec.AppFollowsCLI && len(spec.AppNames) > 0 {
			item.DefaultAppName = spec.AppNames[0].Value
		}
		for _, a := range spec.AppNames {
			item.AppNames = append(item.AppNames, CodexUserAgentCatalogOption{Value: a.Value, Weight: a.Weight})
		}
		for _, t := range spec.Terminals {
			item.Terminals = append(item.Terminals, CodexUserAgentCatalogOption{Value: t.Value, Weight: t.Weight})
		}
		for _, p := range spec.Platforms {
			item.Platforms = append(item.Platforms, CodexUserAgentCatalogPlatform{OSName: p.OSName, OSVersion: p.OSVersion, Arch: p.Arch, Weight: p.Weight})
		}
		for _, p := range spec.VersionPairs {
			item.VersionPairs = append(item.VersionPairs, CodexUserAgentCatalogVersionPair{CLIVersion: p.CLIVersion, AppVersion: p.AppVersion, Weight: p.Weight})
		}
		codexCatalogReferenceOptions(spec, &item)
		view.Kinds = append(view.Kinds, item)
	}
	return view
}

func codexCatalogReferenceOptions(spec *codexUAKindSpec, item *CodexUserAgentCatalogKind) {
	item.ReferenceTerminals = []string{}
	for _, term := range spec.ReferenceTerminals {
		if !codexUAHasOption(spec.Terminals, term) {
			item.ReferenceTerminals = append(item.ReferenceTerminals, term)
		}
	}
	item.ReferencePlatforms = []CodexUserAgentCatalogPlatform{}
	for _, platform := range spec.ReferencePlatforms {
		if !codexUAHasPlatform(spec.Platforms, platform) {
			item.ReferencePlatforms = append(item.ReferencePlatforms, CodexUserAgentCatalogPlatform{OSName: platform.OSName, OSVersion: platform.OSVersion, Arch: platform.Arch})
		}
	}
}

type CodexUserAgentPersona struct {
	Label          string `json:"label,omitempty"`
	AccountID      int64  `json:"account_id,omitempty"`
	UserAgent      string `json:"user_agent"`
	Originator     string `json:"originator"`
	Version        string `json:"version"`
	AppVersion     string `json:"app_version,omitempty"`
	TargetPlatform string `json:"target_platform,omitempty"`
	Source         string `json:"source,omitempty"`
	Status         string `json:"status,omitempty"`
}

type CodexUserAgentPreview struct {
	Mode       string                  `json:"mode"`
	Kind       string                  `json:"kind,omitempty"`
	Persona    *CodexUserAgentPersona  `json:"persona,omitempty"`
	Samples    []CodexUserAgentPersona `json:"samples,omitempty"`
	Warnings   []string                `json:"warnings,omitempty"`
	Normalized string                  `json:"normalized"`
}

// PreviewCodexUserAgentConfig 按当前配置算出真实出站身份(UA / Originator / Version),
// 号池模式下对给定账号逐个抽样。versionFloor 与执行链路同义(自动兼容模式下的最低 CLI 版本)。
func PreviewCodexUserAgentConfig(raw, versionFloor string, sampleAccountIDs []int64) (CodexUserAgentPreview, error) {
	normalized, err := NormalizeCodexUserAgentConfigJSON(raw)
	if err != nil {
		return CodexUserAgentPreview{}, err
	}
	cfg := codexUserAgentConfigFromJSON(normalized)
	preview := CodexUserAgentPreview{Mode: CodexUserAgentModeSingle, Normalized: normalized}
	if cfg.Mode == CodexUserAgentModePool && cfg.RawUserAgent == "" {
		return previewCodexPoolConfig(cfg, codexPoolPreviewInput{normalized: normalized, floor: versionFloor, accounts: sampleAccountIDs})
	}

	ua, version, ok, err := codexUserAgentFromConfigChecked(normalized, 0, versionFloor)
	if err != nil {
		return CodexUserAgentPreview{}, err
	}
	if !ok {
		// 空配置:走内置画像池的默认画像,预览按账号 0 展示。
		ua, version, err = generatedCodexClientHeadersChecked(nil, RuntimeSettings{CodexUserAgentConfig: normalized, CodexMinCLIVersion: versionFloor, ClientCompatMode: ClientCompatModeAuto})
		if err != nil {
			return CodexUserAgentPreview{}, err
		}
	}
	kind := effectiveCodexClientKind(cfg)
	preview.Kind = string(kind)
	preview.Persona = &CodexUserAgentPersona{UserAgent: ua, Originator: CodexOriginatorForGeneratedUserAgent(ua), Version: version}
	*preview.Persona = codexPersonaVersionSource(*preview.Persona, cfg)
	preview.Warnings = codexUserAgentComboWarnings(cfg, kind)
	return preview, nil
}

// codexUserAgentComboWarnings 提示目录里从未出现过的静态画像字段(不阻止保存)。
func codexUserAgentComboWarnings(cfg CodexUserAgentConfig, kind CodexClientKind) []string {
	spec, ok := codexUAKindSpecFor(kind)
	if !ok || cfg.RawUserAgent != "" {
		return nil
	}
	var warnings []string
	if term := strings.TrimSpace(cfg.Terminal); term != "" && !codexUAHasKnownTerminal(spec, term) {
		warnings = append(warnings, "terminal")
	}
	if name := strings.TrimSpace(cfg.AppName); name != "" && !codexUAHasOption(spec.AppNames, name) {
		warnings = append(warnings, "app_name")
	}
	if codexHasCustomPlatform(cfg) {
		platform := codexUAPlatform{
			OSName:    firstNonEmptyString(cfg.OSName, spec.DefaultPlatform.OSName),
			OSVersion: firstNonEmptyString(cfg.OSVersion, spec.DefaultPlatform.OSVersion),
			Arch:      firstNonEmptyString(cfg.Arch, spec.DefaultPlatform.Arch),
		}
		if !codexUAHasPlatform(spec.Platforms, platform) && !codexUAHasPlatform(spec.ReferencePlatforms, platform) {
			warnings = append(warnings, "platform")
		}
	}
	return warnings
}

func codexUAHasOption(items []codexUAWeighted, value string) bool {
	for _, it := range items {
		if strings.EqualFold(it.Value, value) {
			return true
		}
	}
	return false
}

func codexUAHasKnownTerminal(spec *codexUAKindSpec, value string) bool {
	return codexUAHasOption(spec.Terminals, value) || codexUAHasReferenceTerminal(spec, value)
}

func codexUAHasReferenceTerminal(spec *codexUAKindSpec, value string) bool {
	for _, terminal := range spec.ReferenceTerminals {
		if strings.EqualFold(terminal, value) {
			return true
		}
	}
	return false
}

func codexUAHasPlatform(items []codexUAPlatform, platform codexUAPlatform) bool {
	for _, it := range items {
		if strings.EqualFold(it.OSName, platform.OSName) && it.OSVersion == platform.OSVersion && strings.EqualFold(it.Arch, platform.Arch) {
			return true
		}
	}
	return false
}

type codexPoolPreviewInput struct {
	normalized string
	floor      string
	accounts   []int64
}

func previewCodexPoolConfig(cfg CodexUserAgentConfig, input codexPoolPreviewInput) (CodexUserAgentPreview, error) {
	preview := CodexUserAgentPreview{Mode: CodexUserAgentModePool, Normalized: input.normalized}
	sampleAccountIDs := input.accounts
	if len(sampleAccountIDs) == 0 {
		sampleAccountIDs = []int64{1, 2, 3, 4, 5, 6}
	}
	for i, id := range sampleAccountIDs {
		ua, version, ok, err := codexPoolPersona(cfg, id, input.floor)
		if err != nil {
			return CodexUserAgentPreview{}, err
		}
		if !ok {
			return CodexUserAgentPreview{}, errors.New("codex User-Agent pool_mix has no positive weights")
		}
		preview.Samples = append(preview.Samples, codexPersonaVersionSource(CodexUserAgentPersona{
			Label:      fmt.Sprintf("#%d", i+1),
			AccountID:  id,
			UserAgent:  ua,
			Originator: CodexOriginatorForGeneratedUserAgent(ua),
			Version:    version,
		}, cfg))
	}
	return preview, nil
}

func codexHasCustomPlatform(cfg CodexUserAgentConfig) bool {
	return cfg.OSName != "" || cfg.OSVersion != "" || cfg.Arch != ""
}
