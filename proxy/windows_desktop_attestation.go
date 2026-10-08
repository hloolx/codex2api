package proxy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"golang.org/x/text/language"
)

const (
	windowsDesktopAttestationHeader = "X-Oai-Attestation"
	windowsDesktopBundleID          = "com.openai.codex"
	windowsDesktopVersion           = 1
	windowsDesktopSuccessStatus     = 0
	windowsDesktopUnavailableCode   = 1
	windowsDesktopDefaultLocale     = "en-US"
	windowsDesktopDefaultTimezone   = "Etc/UTC"
	windowsDesktopTokenFields       = 3
	windowsDesktopDisplaySlotCount  = 10 // 保留原有账号到档位的映射

	cborMajorUnsigned = 0
	cborMajorBytes    = 2
	cborMajorText     = 3
	cborMajorArray    = 4
	cborMajorMap      = 5
	cborMajorShift    = 5
	cborDirectLimit   = 24
	cborUint8Marker   = 24
	cborUint16Marker  = 25
	cborUint32Marker  = 26
	cborUint64Marker  = 27
	cborFloat64Marker = 0xfb
)

const (
	windowsSignalSchema = iota
	windowsSignalLanguages
	windowsSignalLocale
	windowsSignalTimezone
	windowsSignalScreenSize
	windowsSignalScreenScale
	windowsSignalSessionID
	windowsSignalFieldCount
)

type windowsDesktopDisplay struct {
	physicalWidth  int
	physicalHeight int
	scaleFactor    float64
}

type windowsDesktopSignalValues struct {
	sessionID string
	timezone  string
	locale    string
	display   windowsDesktopDisplay
}

// 顺序固定：账号 ID 选中一个档位后，跨请求和重启都保持同一屏幕尺寸与缩放比例。
var windowsDesktopDisplays = [...]windowsDesktopDisplay{
	{physicalWidth: 1920, physicalHeight: 1080, scaleFactor: 1},
	{physicalWidth: 1920, physicalHeight: 1080, scaleFactor: 1.25},
	{physicalWidth: 1920, physicalHeight: 1200, scaleFactor: 1.25},
	{physicalWidth: 2560, physicalHeight: 1440, scaleFactor: 1.25},
	{physicalWidth: 2560, physicalHeight: 1440, scaleFactor: 1.5},
	{physicalWidth: 2560, physicalHeight: 1600, scaleFactor: 1.5},
	{physicalWidth: 2880, physicalHeight: 1800, scaleFactor: 2},
	{physicalWidth: 3840, physicalHeight: 2160, scaleFactor: 1.5},
	{physicalWidth: 3840, physicalHeight: 2160, scaleFactor: 2},
}

// ApplyWindowsDesktopAttestation 为 Windows Codex Desktop 出站身份补齐官方客户端的
// 无 DeviceCheck 状态。已有下游证明或账号自定义证明保持优先。
func ApplyWindowsDesktopAttestation(headers http.Header, account *auth.Account) {
	if headers == nil || account == nil || strings.TrimSpace(headers.Get(windowsDesktopAttestationHeader)) != "" {
		return
	}
	userAgent := strings.TrimSpace(headers.Get("User-Agent"))
	if !strings.EqualFold(codexUserAgentClientName(userAgent), "Codex Desktop") || !strings.Contains(userAgent, "(Windows ") {
		return
	}
	timezone := account.EffectiveCodexTimezone()
	if timezone == "" {
		timezone = windowsDesktopDefaultTimezone
	}
	accountID := account.ID()
	values := windowsDesktopSignalValues{
		sessionID: windowsDesktopDailySessionID(accountID, timezone, time.Now()),
		timezone:  timezone,
		locale:    windowsDesktopLocaleForTimezone(timezone),
		display:   windowsDesktopDisplayForAccount(accountID),
	}
	token := windowsDesktopAttestationToken(values)
	header, err := json.Marshal(struct {
		Version int    `json:"v"`
		Status  int    `json:"s"`
		Token   string `json:"t"`
	}{Version: windowsDesktopVersion, Status: windowsDesktopSuccessStatus, Token: token})
	if err == nil {
		headers.Set(windowsDesktopAttestationHeader, string(header))
	}
}

func windowsDesktopLocaleForTimezone(timezone string) string {
	// 时区先映射到国家，再用 CLDR 的 likely language 选择该地区常用语言。
	country := windowsTimezoneCountryByZone[timezone]
	if country == "" {
		return windowsDesktopDefaultLocale
	}
	base, _ := language.Make("und-" + country).Base()
	if base.String() == "und" {
		return windowsDesktopDefaultLocale
	}
	return base.String() + "-" + country
}

func windowsDesktopDisplayForAccount(accountID int64) windowsDesktopDisplay {
	seed := fmt.Sprintf("codex2api:windows-desktop-display:v1:%d", accountID)
	digest := sha256.Sum256([]byte(seed))
	slot := binary.BigEndian.Uint64(digest[:8]) % windowsDesktopDisplaySlotCount
	if slot == 0 {
		// 原 1366×768 档位改为 1920×1080，其余账号的档位不变。
		return windowsDesktopDisplays[0]
	}
	return windowsDesktopDisplays[slot-1]
}

func windowsDesktopDailySessionID(accountID int64, timezone string, now time.Time) string {
	// 使用账号时区的自然日，跨实例和重启保持当天 UUID 不变。
	location, err := time.LoadLocation(timezone)
	if err != nil {
		location = time.UTC
	}
	date := now.In(location).Format(time.DateOnly)
	seed := fmt.Sprintf("codex2api:windows-desktop-session:v1:%d:%s", accountID, date)
	return deriveStableCodexUUID(seed)
}

func windowsDesktopAttestationToken(values windowsDesktopSignalValues) string {
	signals := windowsDesktopSignals(values)
	data := appendCBORHeader(nil, cborMajorMap, windowsDesktopTokenFields)
	data = appendCBORText(data, "error_code")
	data = appendCBORHeader(data, cborMajorUnsigned, windowsDesktopUnavailableCode)
	data = appendCBORText(data, "bundle_id")
	data = appendCBORText(data, windowsDesktopBundleID)
	data = appendCBORText(data, "f")
	data = appendCBORHeader(data, cborMajorBytes, uint64(len(signals)))
	data = append(data, signals...)
	return "v1." + base64.RawURLEncoding.EncodeToString(data)
}

func windowsDesktopSignals(values windowsDesktopSignalValues) []byte {
	// Electron Display.size 是逻辑像素；官方实现将主屏宽高相加并四舍五入。
	logicalWidth := math.Round(float64(values.display.physicalWidth) / values.display.scaleFactor)
	logicalHeight := math.Round(float64(values.display.physicalHeight) / values.display.scaleFactor)
	screenSizeSum := uint64(math.Max(0, math.Round(logicalWidth+logicalHeight)))
	data := appendCBORHeader(nil, cborMajorMap, windowsSignalFieldCount)
	data = appendCBORHeader(data, cborMajorUnsigned, windowsSignalSchema)
	data = appendCBORHeader(data, cborMajorUnsigned, windowsDesktopVersion)
	data = appendCBORHeader(data, cborMajorUnsigned, windowsSignalLanguages)
	data = appendCBORHeader(data, cborMajorArray, 1)
	data = appendCBORText(data, values.locale)
	data = appendCBORHeader(data, cborMajorUnsigned, windowsSignalLocale)
	data = appendCBORText(data, values.locale)
	data = appendCBORHeader(data, cborMajorUnsigned, windowsSignalTimezone)
	data = appendCBORText(data, values.timezone)
	data = appendCBORHeader(data, cborMajorUnsigned, windowsSignalScreenSize)
	data = appendCBORHeader(data, cborMajorUnsigned, screenSizeSum)
	data = appendCBORHeader(data, cborMajorUnsigned, windowsSignalScreenScale)
	data = appendCBORNumber(data, values.display.scaleFactor)
	data = appendCBORHeader(data, cborMajorUnsigned, windowsSignalSessionID)
	return appendCBORText(data, values.sessionID)
}

func appendCBORText(data []byte, value string) []byte {
	data = appendCBORHeader(data, cborMajorText, uint64(len(value)))
	return append(data, value...)
}

func appendCBORNumber(data []byte, value float64) []byte {
	if value >= 0 && math.Trunc(value) == value {
		return appendCBORHeader(data, cborMajorUnsigned, uint64(value))
	}
	data = append(data, cborFloat64Marker)
	return binary.BigEndian.AppendUint64(data, math.Float64bits(value))
}

func appendCBORHeader(data []byte, major byte, value uint64) []byte {
	prefix := major << cborMajorShift
	switch {
	case value < cborDirectLimit:
		return append(data, prefix|byte(value))
	case value <= math.MaxUint8:
		return append(data, prefix|cborUint8Marker, byte(value))
	case value <= math.MaxUint16:
		data = append(data, prefix|cborUint16Marker)
		return binary.BigEndian.AppendUint16(data, uint16(value))
	case value <= math.MaxUint32:
		data = append(data, prefix|cborUint32Marker)
		return binary.BigEndian.AppendUint32(data, uint32(value))
	default:
		data = append(data, prefix|cborUint64Marker)
		return binary.BigEndian.AppendUint64(data, value)
	}
}
