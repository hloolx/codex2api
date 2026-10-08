package proxy

// This file ports ModelTrace's dependency-free browser fingerprint core to Go.
// The model bank is pinned and embedded so detection never depends on a remote
// service or exposes account credentials outside the normal proxy path. An
// operator may install a newer upstream bank explicitly; it must pass the same
// validation plus an algorithm pin, and the embedded copy remains the fallback.

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

const (
	ModelTraceSourceURL      = "https://github.com/xqy2006/ModelTrace"
	ModelTraceSourceRevision = "d4131b30243dfa05e70180b5eedde742103f1d73"
	// ModelTraceTargetOutputs is the bank's recommended sample count and the
	// largest pooled count it ships calibration for.
	ModelTraceTargetOutputs = 3

	modelTraceValueMin          = 1
	modelTraceValueMax          = 355
	modelTraceDimension         = modelTraceValueMax - modelTraceValueMin + 1
	modelTraceAlpha             = 0.5
	modelTraceOrderedDimensions = 74
)

//go:embed modeltracedata/unified_bank.json
var modelTraceBankJSON []byte

type modelTraceBank struct {
	Schema             string                           `json:"schema"`
	BuiltAt            string                           `json:"built_at"`
	Method             modelTraceMethod                 `json:"method"`
	RecommendedQueries int                              `json:"recommended_queries"`
	Models             []modelTraceModel                `json:"models"`
	Calibration        map[string]modelTraceCalibration `json:"calibration"`
	Robust             modelTraceRobust                 `json:"robust"`
}

// modelTraceMethod describes the scoring algorithm a bank was trained for. The
// Go port only implements one, so banks for any other method are rejected.
type modelTraceMethod struct {
	Name               string  `json:"name"`
	Range              []int   `json:"range"`
	Alpha              float64 `json:"alpha"`
	OrderedBlockWeight float64 `json:"ordered_block_weight"`
}

type modelTraceModel struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	Family      string    `json:"family"`
	FamilyName  string    `json:"family_name"`
	Counts      []float64 `json:"counts"`
}

type modelTraceCalibration struct {
	Beta       float64 `json:"beta"`
	CVAccuracy float64 `json:"cv_accuracy"`
}

type modelTraceRobust struct {
	ModelOrder    []string                  `json:"model_order"`
	RobustReady   bool                      `json:"robust_ready"`
	Hellinger     modelTraceArtifact        `json:"hellinger"`
	OrderedBlocks modelTraceOrderedArtifact `json:"ordered_blocks"`
}

type modelTraceArtifact struct {
	FeatureMean   []float64   `json:"feature_mean"`
	FeatureScale  []float64   `json:"feature_scale"`
	NuisanceBasis [][]float64 `json:"nuisance_basis"`
	Centroids     [][]float64 `json:"centroids"`
}

type modelTraceOrderedArtifact struct {
	Feature              string        `json:"feature"`
	FeatureMean          []float64     `json:"feature_mean"`
	FeatureScale         []float64     `json:"feature_scale"`
	NuisanceBasis        [][]float64   `json:"nuisance_basis"`
	Centroids            [][]float64   `json:"centroids"`
	Weight               float64       `json:"weight"`
	EnvironmentCentroids [][][]float64 `json:"environment_centroids"`
}

func (artifact modelTraceOrderedArtifact) base() modelTraceArtifact {
	return modelTraceArtifact{
		FeatureMean: artifact.FeatureMean, FeatureScale: artifact.FeatureScale,
		NuisanceBasis: artifact.NuisanceBasis, Centroids: artifact.Centroids,
	}
}

type ModelTraceChallenge struct {
	ID            string `json:"id"`
	ExpectedCount int    `json:"expected_count"`
	Prompt        string `json:"prompt"`
}

type ModelTraceOutput struct {
	Text          string
	ExpectedCount int
}

type ModelTraceDiagnostic struct {
	Index          int  `json:"index"`
	ParsedNumbers  int  `json:"parsed_numbers"`
	MinimumNumbers int  `json:"minimum_numbers"`
	Accepted       bool `json:"accepted"`
}

type ModelTraceResult struct {
	Model                  string  `json:"model"`
	DisplayName            string  `json:"display_name"`
	Probability            float64 `json:"probability"`
	ProfileSimilarity      float64 `json:"profile_similarity"`
	Score                  float64 `json:"score"`
	Family                 string  `json:"family"`
	FamilyName             string  `json:"family_name"`
	ConditionalProbability float64 `json:"conditional_probability"`
}

type ModelTraceFamilyProbability struct {
	Family      string  `json:"family"`
	DisplayName string  `json:"display_name"`
	Probability float64 `json:"probability"`
}

type ModelTraceCalibrationResult struct {
	Queries    int     `json:"queries"`
	Beta       float64 `json:"beta"`
	CVAccuracy float64 `json:"cv_accuracy"`
}

// ModelTraceReport contains only aggregate fingerprints and diagnostics. Raw
// upstream answers are intentionally excluded from the SSE response.
type ModelTraceReport struct {
	Prediction           string                        `json:"prediction"`
	PredictionName       string                        `json:"prediction_name"`
	Probability          float64                       `json:"probability"`
	UsedOutputs          int                           `json:"used_outputs"`
	Results              []ModelTraceResult            `json:"results"`
	Diagnostics          []ModelTraceDiagnostic        `json:"diagnostics"`
	Calibration          ModelTraceCalibrationResult   `json:"calibration"`
	FamilyPrediction     string                        `json:"family_prediction"`
	FamilyPredictionName string                        `json:"family_prediction_name"`
	FamilyProbability    float64                       `json:"family_probability"`
	FamilyProbabilities  []ModelTraceFamilyProbability `json:"family_probabilities"`
	Method               string                        `json:"method"`
	CandidateScope       string                        `json:"candidate_scope"`
	BankSchema           string                        `json:"bank_schema"`
	Source               string                        `json:"source"`
	SourceURL            string                        `json:"source_url"`
	SourceRevision       string                        `json:"source_revision"`
}

const (
	ModelTraceBankOriginEmbedded = "embedded"
	ModelTraceBankOriginOverride = "override"
)

// ModelTraceDetector is immutable and safe for concurrent use after loading.
type ModelTraceDetector struct {
	bank     modelTraceBank
	revision string
	origin   string
	builtAt  time.Time
}

// ModelTraceBankInfo summarizes a loaded bank for the admin UI.
type ModelTraceBankInfo struct {
	Origin   string   `json:"origin"`
	Revision string   `json:"revision"`
	BuiltAt  string   `json:"built_at"`
	Models   []string `json:"models"`
}

var (
	modelTraceLoadOnce sync.Once
	modelTraceLoaded   *ModelTraceDetector
	modelTraceLoadErr  error
	modelTraceDigits   = regexp.MustCompile(`[0-9]+`)

	// modelTraceOverrideCache keeps the last parsed override so detection runs do
	// not re-decode a ~1 MB bank while its stored revision is unchanged.
	modelTraceOverrideCache atomic.Pointer[ModelTraceDetector]
)

// NewModelTraceDetector returns the embedded bank's detector.
func NewModelTraceDetector() (*ModelTraceDetector, error) {
	modelTraceLoadOnce.Do(func() {
		modelTraceLoaded, modelTraceLoadErr = parseModelTraceBank(modelTraceBankJSON, ModelTraceSourceRevision, ModelTraceBankOriginEmbedded)
	})
	return modelTraceLoaded, modelTraceLoadErr
}

// ParseModelTraceBankOverride validates an operator-installed bank.
func ParseModelTraceBankOverride(data []byte, revision string) (*ModelTraceDetector, error) {
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return nil, errors.New("ModelTrace bank revision is required")
	}
	return parseModelTraceBank(data, revision, ModelTraceBankOriginOverride)
}

// CacheModelTraceOverride remembers the parsed override that is installed.
func CacheModelTraceOverride(detector *ModelTraceDetector) {
	modelTraceOverrideCache.Store(detector)
}

// CachedModelTraceOverride returns the cached override when it matches revision.
func CachedModelTraceOverride(revision string) *ModelTraceDetector {
	if cached := modelTraceOverrideCache.Load(); cached != nil && cached.revision == revision {
		return cached
	}
	return nil
}

func parseModelTraceBank(data []byte, revision, origin string) (*ModelTraceDetector, error) {
	var bank modelTraceBank
	if err := json.Unmarshal(data, &bank); err != nil {
		return nil, fmt.Errorf("decode ModelTrace bank: %w", err)
	}
	if err := validateModelTraceBank(&bank); err != nil {
		return nil, err
	}
	builtAt, _ := time.Parse(time.RFC3339Nano, strings.TrimSpace(bank.BuiltAt))
	return &ModelTraceDetector{bank: bank, revision: revision, origin: origin, builtAt: builtAt}, nil
}

func validateModelTraceBank(bank *modelTraceBank) error {
	if bank.Schema != "robust-number-fingerprint-bank" || bank.RecommendedQueries != ModelTraceTargetOutputs {
		return errors.New("unsupported ModelTrace bank")
	}
	// The data alone cannot prove the scoring code is unchanged, so pin the
	// declared method to the one this port implements.
	method := bank.Method
	if method.Name != "Ordered-block + nuisance-Hellinger" || len(method.Range) != 2 ||
		method.Range[0] != modelTraceValueMin || method.Range[1] != modelTraceValueMax ||
		method.Alpha != modelTraceAlpha || method.OrderedBlockWeight != bank.Robust.OrderedBlocks.Weight ||
		bank.Robust.OrderedBlocks.Feature != "four position blocks x 16 value bins plus final-digit distribution" {
		return errors.New("ModelTrace bank uses an unsupported scoring method")
	}
	if !bank.Robust.RobustReady || len(bank.Models) < 2 || len(bank.Robust.ModelOrder) != len(bank.Models) {
		return errors.New("incomplete ModelTrace bank")
	}
	for index, model := range bank.Models {
		if model.ID == "" || model.DisplayName == "" || model.ID != bank.Robust.ModelOrder[index] || len(model.Counts) != modelTraceDimension {
			return errors.New("invalid ModelTrace model entry")
		}
	}
	if err := validateModelTraceArtifact(bank.Robust.Hellinger, modelTraceDimension, len(bank.Models)); err != nil {
		return fmt.Errorf("invalid ModelTrace Hellinger artifact: %w", err)
	}
	ordered := bank.Robust.OrderedBlocks
	if ordered.Weight < 0 || ordered.Weight > 1 {
		return errors.New("invalid ModelTrace ordered-block weight")
	}
	if err := validateModelTraceArtifact(ordered.base(), modelTraceOrderedDimensions, len(bank.Models)); err != nil {
		return fmt.Errorf("invalid ModelTrace ordered-block artifact: %w", err)
	}
	if len(ordered.EnvironmentCentroids) == 0 {
		return errors.New("missing ModelTrace environment centroids")
	}
	for _, environment := range ordered.EnvironmentCentroids {
		if !matrixHasShape(environment, len(bank.Models), modelTraceOrderedDimensions) {
			return errors.New("invalid ModelTrace environment centroid shape")
		}
	}
	for queries := 1; queries <= ModelTraceTargetOutputs; queries++ {
		calibration, ok := bank.Calibration[strconv.Itoa(queries)]
		if !ok || calibration.Beta <= 0 {
			return errors.New("missing ModelTrace calibration")
		}
	}
	return nil
}

func validateModelTraceArtifact(artifact modelTraceArtifact, dimensions, models int) error {
	if len(artifact.FeatureMean) != dimensions || len(artifact.FeatureScale) != dimensions || !matrixHasShape(artifact.Centroids, models, dimensions) {
		return errors.New("invalid dimensions")
	}
	for _, scale := range artifact.FeatureScale {
		if scale == 0 || math.IsNaN(scale) || math.IsInf(scale, 0) {
			return errors.New("invalid feature scale")
		}
	}
	for _, basis := range artifact.NuisanceBasis {
		if len(basis) != dimensions {
			return errors.New("invalid nuisance basis")
		}
	}
	return nil
}

func matrixHasShape(matrix [][]float64, rows, columns int) bool {
	if len(matrix) != rows {
		return false
	}
	for _, row := range matrix {
		if len(row) != columns {
			return false
		}
	}
	return true
}

func (d *ModelTraceDetector) Models() []string {
	models := make([]string, len(d.bank.Models))
	for index, model := range d.bank.Models {
		models[index] = model.ID
	}
	return models
}

func (d *ModelTraceDetector) BankSchema() string { return d.bank.Schema }

func (d *ModelTraceDetector) Revision() string { return d.revision }

// BuiltAt is the bank's training timestamp; zero when the bank omits it.
func (d *ModelTraceDetector) BuiltAt() time.Time { return d.builtAt }

func (d *ModelTraceDetector) Info() ModelTraceBankInfo {
	return ModelTraceBankInfo{Origin: d.origin, Revision: d.revision, BuiltAt: d.bank.BuiltAt, Models: d.Models()}
}

func (d *ModelTraceDetector) GenerateChallenges(count int) ([]ModelTraceChallenge, error) {
	if count < 1 || count > 41 {
		return nil, errors.New("ModelTrace challenge count must be between 1 and 41")
	}
	lengths := make([]int, 41)
	for index := range lengths {
		lengths[index] = 292 + index
	}
	for index := 0; index < count; index++ {
		offset, err := modelTraceRandomIndex(len(lengths) - index)
		if err != nil {
			return nil, err
		}
		selected := index + offset
		lengths[index], lengths[selected] = lengths[selected], lengths[index]
	}

	openings := []string{
		"这是一次独立的数值选择记录", "请完成下面的无语义整数选择任务", "执行一次第一反应取值记录",
		"生成一组不承载语义的整数选择", "进行一轮快速逐项取值",
	}
	actions := []string{
		"为各个位置分别凭第一反应选择", "逐项选择", "每次只决定当前一项，共给出", "分别凭第一反应给出", "逐个直接选择",
	}
	endings := []string{
		"允许某个数字再次出现；每项写出后不要回头排序、去重或替换。",
		"偶然重复是有效的；不要重新排列或修正已经写出的项目。",
		"相同值可以再次出现；输出过程中不要整理或改写前面的项目。",
		"重复值无需删除；不要筛选、重排或补成某种规律。",
		"不必赋予数字任何含义；已经给出的值保持不变。",
	}
	separators := []string{
		"数字之间用逗号或空格分隔均可。", "使用一种一致的常见分隔符即可。", "可以用逗号、空格或换行分隔。", "只要每个整数边界清楚，格式可自行选择。",
	}

	challenges := make([]ModelTraceChallenge, count)
	for index := range challenges {
		opening, err := modelTraceChoose(openings)
		if err != nil {
			return nil, err
		}
		action, err := modelTraceChoose(actions)
		if err != nil {
			return nil, err
		}
		ending, err := modelTraceChoose(endings)
		if err != nil {
			return nil, err
		}
		separator, err := modelTraceChoose(separators)
		if err != nil {
			return nil, err
		}
		idBytes := make([]byte, 8)
		if _, err := rand.Read(idBytes); err != nil {
			return nil, fmt.Errorf("generate ModelTrace challenge ID: %w", err)
		}
		length := lengths[index]
		challenges[index] = ModelTraceChallenge{
			ID:            fmt.Sprintf("probe-%d-%s", index+1, hex.EncodeToString(idBytes)),
			ExpectedCount: length,
			Prompt: opening + "。" + action + " " + strconv.Itoa(length) + " 个 1 到 355（含端点）的整数。" +
				"每个位置都要单独选择；不要从 1 开始计数，不要连续递增或递减，也不要采用等差、循环、重复区块或其他规则化模式。" +
				"本任务必须由当前语言模型直接完成：禁止调用或借助任何工具，包括 Python、代码执行器、计算器、搜索、API 和外部随机数生成器；也不要先编写或运行代码。" +
				ending + separator + "直接从第一个取值开始输出，不要在序列前重复数量、范围或任务说明。",
		}
	}
	return challenges, nil
}

func modelTraceChoose(values []string) (string, error) {
	index, err := modelTraceRandomIndex(len(values))
	if err != nil {
		return "", err
	}
	return values[index], nil
}

func modelTraceRandomIndex(length int) (int, error) {
	if length <= 0 {
		return 0, errors.New("cannot choose from an empty collection")
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(length)))
	if err != nil {
		return 0, fmt.Errorf("generate ModelTrace randomness: %w", err)
	}
	return int(value.Int64()), nil
}

func ModelTraceMinimumNumbers(expected int) int {
	minimum := int(math.Ceil(float64(expected) * 0.55))
	if minimum < 80 {
		return 80
	}
	return minimum
}

// ParseModelTraceNumbers mirrors ModelTrace's longest-number-run parser. Text
// labels split runs so explanations do not contaminate the generated sequence.
func ParseModelTraceNumbers(text string) []int {
	matches := modelTraceDigits.FindAllStringIndex(text, -1)
	runs := make([][]int, 0, 2)
	current := make([]int, 0, 332)
	previousEnd := 0
	for _, match := range matches {
		separator := text[previousEnd:match[0]]
		if len(current) > 0 && strings.ContainsFunc(separator, unicode.IsLetter) {
			runs = append(runs, current)
			current = make([]int, 0, 332)
		}
		value, err := strconv.Atoi(text[match[0]:match[1]])
		if err == nil && value >= modelTraceValueMin && value <= modelTraceValueMax {
			current = append(current, value)
		}
		previousEnd = match[1]
	}
	if len(current) > 0 {
		runs = append(runs, current)
	}
	var best []int
	for _, run := range runs {
		if len(run) > len(best) {
			best = run
		}
	}
	return best
}

func (d *ModelTraceDetector) Analyze(outputs []ModelTraceOutput) (ModelTraceReport, error) {
	type validOutput struct {
		numbers []int
		counts  []float64
		scores  []float64
	}
	valid := make([]validOutput, 0, ModelTraceTargetOutputs)
	diagnostics := make([]ModelTraceDiagnostic, 0, len(outputs))
	for index, output := range outputs {
		numbers := ParseModelTraceNumbers(output.Text)
		minimum := ModelTraceMinimumNumbers(output.ExpectedCount)
		accepted := len(numbers) >= minimum
		diagnostics = append(diagnostics, ModelTraceDiagnostic{
			Index: index, ParsedNumbers: len(numbers), MinimumNumbers: minimum, Accepted: accepted,
		})
		if !accepted {
			continue
		}
		counts := modelTraceCountNumbers(numbers)
		scores := d.robustScoreNumbers(numbers, counts)
		valid = append(valid, validOutput{numbers: numbers, counts: counts, scores: scores})
	}
	if len(valid) == 0 {
		return ModelTraceReport{}, errors.New("没有可用回答：上游拒答或回答被严重截断")
	}

	modelCount := len(d.bank.Models)
	combinedScores := make([]float64, modelCount)
	pooledCounts := make([]float64, modelTraceDimension)
	for modelIndex := range modelCount {
		for _, output := range valid {
			combinedScores[modelIndex] += output.scores[modelIndex]
		}
		combinedScores[modelIndex] /= float64(len(valid))
	}
	for _, output := range valid {
		for index, count := range output.counts {
			pooledCounts[index] += count
		}
	}

	calibrationQueries := min(len(valid), ModelTraceTargetOutputs)
	calibration := d.bank.Calibration[strconv.Itoa(calibrationQueries)]
	calibrated := make([]float64, modelCount)
	for index, score := range combinedScores {
		calibrated[index] = calibration.Beta * score
	}
	probabilities := modelTraceSoftmax(calibrated)

	familyOrder := make([]string, 0)
	familyNames := make(map[string]string)
	familyProbabilities := make(map[string]float64)
	results := make([]ModelTraceResult, modelCount)
	for index, model := range d.bank.Models {
		family := model.Family
		if family == "" {
			family = "models"
		}
		familyName := model.FamilyName
		if familyName == "" {
			familyName = family
		}
		if _, exists := familyNames[family]; !exists {
			familyOrder = append(familyOrder, family)
			familyNames[family] = familyName
		}
		familyProbabilities[family] += probabilities[index]
		results[index] = ModelTraceResult{
			Model: model.ID, DisplayName: model.DisplayName, Probability: probabilities[index],
			ProfileSimilarity: modelTraceJSSimilarity(pooledCounts, model.Counts), Score: combinedScores[index],
			Family: family, FamilyName: familyName,
		}
	}
	for index := range results {
		results[index].ConditionalProbability = results[index].Probability / familyProbabilities[results[index].Family]
	}
	sort.SliceStable(results, func(left, right int) bool { return results[left].Probability > results[right].Probability })

	winningFamily := familyOrder[0]
	familyResults := make([]ModelTraceFamilyProbability, 0, len(familyOrder))
	for _, family := range familyOrder {
		if familyProbabilities[family] > familyProbabilities[winningFamily] {
			winningFamily = family
		}
		familyResults = append(familyResults, ModelTraceFamilyProbability{
			Family: family, DisplayName: familyNames[family], Probability: familyProbabilities[family],
		})
	}

	return ModelTraceReport{
		Prediction: results[0].Model, PredictionName: results[0].DisplayName, Probability: results[0].Probability,
		UsedOutputs: len(valid), Results: results, Diagnostics: diagnostics,
		Calibration:      ModelTraceCalibrationResult{Queries: calibrationQueries, Beta: calibration.Beta, CVAccuracy: calibration.CVAccuracy},
		FamilyPrediction: winningFamily, FamilyPredictionName: familyNames[winningFamily], FamilyProbability: familyProbabilities[winningFamily],
		FamilyProbabilities: familyResults, Method: "统一全局稳健数字指纹", CandidateScope: "closed_set",
		BankSchema: d.bank.Schema, Source: "ModelTrace", SourceURL: ModelTraceSourceURL, SourceRevision: d.revision,
	}, nil
}

func (d *ModelTraceDetector) robustScoreNumbers(numbers []int, counts []float64) []float64 {
	marginal := modelTraceRobustScores(counts, d.bank.Robust.Hellinger, modelTraceHellingerFeature)
	ordered := d.orderedBlockScores(numbers)
	weight := d.bank.Robust.OrderedBlocks.Weight
	for index := range marginal {
		marginal[index] = (1-weight)*marginal[index] + weight*ordered[index]
	}
	return marginal
}

func modelTraceCountNumbers(numbers []int) []float64 {
	counts := make([]float64, modelTraceDimension)
	for _, number := range numbers {
		counts[number-modelTraceValueMin]++
	}
	return counts
}

func modelTraceHellingerFeature(counts []float64) []float64 {
	total := modelTraceAlpha * modelTraceDimension
	for _, count := range counts {
		total += count
	}
	feature := make([]float64, len(counts))
	for index, count := range counts {
		feature[index] = math.Sqrt((count + modelTraceAlpha) / float64(total))
	}
	return feature
}

func modelTraceRobustScores(values []float64, artifact modelTraceArtifact, featureFn func([]float64) []float64) []float64 {
	feature := featureFn(values)
	projected := make([]float64, len(feature))
	for index, value := range feature {
		projected[index] = (value - artifact.FeatureMean[index]) / artifact.FeatureScale[index]
	}
	projected = modelTraceNormalize(modelTraceSubtractBasis(projected, artifact.NuisanceBasis))
	scores := make([]float64, len(artifact.Centroids))
	for index, centroid := range artifact.Centroids {
		scores[index] = modelTraceDot(projected, centroid)
	}
	return modelTraceStandardize(scores)
}

func (d *ModelTraceDetector) orderedBlockScores(numbers []int) []float64 {
	artifact := d.bank.Robust.OrderedBlocks
	feature := modelTraceOrderedBlockFeature(numbers)
	standardizedFeature := make([]float64, len(feature))
	for index, value := range feature {
		standardizedFeature[index] = (value - artifact.FeatureMean[index]) / artifact.FeatureScale[index]
	}
	unit := modelTraceNormalize(standardizedFeature)
	template := make([]float64, len(artifact.Centroids))
	for modelIndex := range template {
		template[modelIndex] = math.Inf(-1)
		for _, environment := range artifact.EnvironmentCentroids {
			score := modelTraceDot(unit, environment[modelIndex])
			if score > template[modelIndex] {
				template[modelIndex] = score
			}
		}
	}
	template = modelTraceStandardize(template)
	projected := modelTraceNormalize(modelTraceSubtractBasis(standardizedFeature, artifact.NuisanceBasis))
	nuisance := make([]float64, len(artifact.Centroids))
	for index, centroid := range artifact.Centroids {
		nuisance[index] = modelTraceDot(projected, centroid)
	}
	nuisance = modelTraceStandardize(nuisance)
	fused := make([]float64, len(template))
	for index := range fused {
		fused[index] = 0.5*template[index] + 0.5*nuisance[index]
	}
	return modelTraceStandardize(fused)
}

func modelTraceOrderedBlockFeature(numbers []int) []float64 {
	feature := make([]float64, 0, modelTraceOrderedDimensions)
	base := len(numbers) / 4
	remainder := len(numbers) % 4
	start := 0
	for chunk := 0; chunk < 4; chunk++ {
		size := base
		if chunk < remainder {
			size++
		}
		bins := make([]float64, 16)
		for index := range bins {
			bins[index] = 0.5
		}
		for _, value := range numbers[start : start+size] {
			bin := int(math.Floor((float64(value-1) / 355) * 16))
			if bin > 15 {
				bin = 15
			}
			bins[bin]++
		}
		start += size
		total := modelTraceSum(bins)
		for _, value := range bins {
			feature = append(feature, math.Sqrt(value/total))
		}
	}
	lastDigits := make([]float64, 10)
	for index := range lastDigits {
		lastDigits[index] = 0.5
	}
	for _, value := range numbers {
		lastDigits[value%10]++
	}
	lastTotal := modelTraceSum(lastDigits)
	for _, value := range lastDigits {
		feature = append(feature, math.Sqrt(value/lastTotal))
	}
	return feature
}

func modelTraceStandardize(values []float64) []float64 {
	center := modelTraceSum(values) / float64(len(values))
	variance := 0.0
	for _, value := range values {
		delta := value - center
		variance += delta * delta
	}
	scale := math.Max(math.Sqrt(variance/float64(len(values))), 1e-12)
	standardized := make([]float64, len(values))
	for index, value := range values {
		standardized[index] = (value - center) / scale
	}
	return standardized
}

func modelTraceSubtractBasis(values []float64, basis [][]float64) []float64 {
	output := append([]float64(nil), values...)
	for _, vector := range basis {
		projection := modelTraceDot(output, vector)
		for index := range output {
			output[index] -= projection * vector[index]
		}
	}
	return output
}

func modelTraceNormalize(values []float64) []float64 {
	scale := math.Max(math.Sqrt(modelTraceDot(values, values)), 1e-12)
	output := make([]float64, len(values))
	for index, value := range values {
		output[index] = value / scale
	}
	return output
}

func modelTraceDot(left, right []float64) float64 {
	value := 0.0
	for index := range left {
		value += left[index] * right[index]
	}
	return value
}

func modelTraceSum(values []float64) float64 {
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total
}

func modelTraceSoftmax(values []float64) []float64 {
	maximum := values[0]
	for _, value := range values[1:] {
		maximum = math.Max(maximum, value)
	}
	weights := make([]float64, len(values))
	total := 0.0
	for index, value := range values {
		weights[index] = math.Exp(value - maximum)
		total += weights[index]
	}
	for index := range weights {
		weights[index] /= total
	}
	return weights
}

func modelTraceJSSimilarity(left, right []float64) float64 {
	leftTotal := modelTraceSum(left)
	rightTotal := modelTraceSum(right) + modelTraceAlpha*modelTraceDimension
	leftDivergence := 0.0
	rightDivergence := 0.0
	for index := range left {
		p := left[index] / leftTotal
		q := (right[index] + modelTraceAlpha) / rightTotal
		midpoint := (p + q) / 2
		if p != 0 {
			leftDivergence += p * math.Log(p/midpoint)
		}
		if q != 0 {
			rightDivergence += q * math.Log(q/midpoint)
		}
	}
	js := (leftDivergence + rightDivergence) / 2
	return 1 - math.Sqrt(js/math.Log(2))
}
