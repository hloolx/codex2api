package proxy

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestParseModelTraceNumbersUsesLongestRun(t *testing.T) {
	numbers := ParseModelTraceNumbers("说明：12, 13, 999, 14 end 20 21")
	want := []int{12, 13, 14}
	if len(numbers) != len(want) {
		t.Fatalf("ParseModelTraceNumbers() = %v, want %v", numbers, want)
	}
	for index := range want {
		if numbers[index] != want[index] {
			t.Fatalf("ParseModelTraceNumbers() = %v, want %v", numbers, want)
		}
	}
}

func TestModelTraceChallengesUseUniqueSupportedLengths(t *testing.T) {
	detector, err := NewModelTraceDetector()
	if err != nil {
		t.Fatalf("NewModelTraceDetector() error = %v", err)
	}
	challenges, err := detector.GenerateChallenges(2 * ModelTraceTargetOutputs)
	if err != nil {
		t.Fatalf("GenerateChallenges() error = %v", err)
	}
	seen := make(map[int]bool)
	for _, challenge := range challenges {
		if challenge.ExpectedCount < 292 || challenge.ExpectedCount > 332 {
			t.Fatalf("unexpected challenge length %d", challenge.ExpectedCount)
		}
		if seen[challenge.ExpectedCount] {
			t.Fatalf("duplicate challenge length %d", challenge.ExpectedCount)
		}
		seen[challenge.ExpectedCount] = true
		if !strings.Contains(challenge.Prompt, strconv.Itoa(challenge.ExpectedCount)) {
			t.Fatalf("challenge prompt does not contain expected count: %q", challenge.Prompt)
		}
	}
}

func TestModelTraceAnalysisMatchesUpstreamCore(t *testing.T) {
	detector, err := NewModelTraceDetector()
	if err != nil {
		t.Fatalf("NewModelTraceDetector() error = %v", err)
	}
	if models := detector.Models(); len(models) != 17 {
		t.Fatalf("Models() returned %d models, want 17", len(models))
	}

	// Golden values come from upstream static/fingerprint-core.js run against
	// the same pinned bank, so drift in the Go port shows up here.
	report, err := detector.Analyze(syntheticModelTraceOutputs(310, 311, 312))
	if err != nil {
		t.Fatalf("Analyze() error = %v", err)
	}
	if report.Prediction != "claude-sonnet-5" || report.FamilyPrediction != "claude" || report.UsedOutputs != 3 || report.Calibration.Queries != 3 {
		t.Fatalf("Analyze() prediction=%q family=%q outputs=%d queries=%d", report.Prediction, report.FamilyPrediction, report.UsedOutputs, report.Calibration.Queries)
	}
	assertModelTraceClose(t, report.Probability, 0.8333973665444901)
	assertModelTraceClose(t, report.FamilyProbability, 0.8354263908577124)
	assertModelTraceClose(t, report.Results[0].ProfileSimilarity, 0.5247777892155716)

	single, err := detector.Analyze(syntheticModelTraceOutputs(310))
	if err != nil {
		t.Fatalf("Analyze(single) error = %v", err)
	}
	if single.Prediction != "gpt-5.6-sol" || single.FamilyPrediction != "claude" || single.UsedOutputs != 1 || single.Calibration.Queries != 1 {
		t.Fatalf("Analyze(single) prediction=%q family=%q outputs=%d queries=%d", single.Prediction, single.FamilyPrediction, single.UsedOutputs, single.Calibration.Queries)
	}
	assertModelTraceClose(t, single.Probability, 0.400810092958018)
	assertModelTraceClose(t, single.FamilyProbability, 0.5126217933968289)
	assertModelTraceClose(t, single.Results[0].ProfileSimilarity, 0.29050196506693937)
}

func syntheticModelTraceOutputs(lengths ...int) []ModelTraceOutput {
	outputs := make([]ModelTraceOutput, len(lengths))
	for outputIndex, length := range lengths {
		var text strings.Builder
		for index := range length {
			if index > 0 {
				text.WriteByte(',')
			}
			value := (index*index*17+index*73+outputIndex*29)%355 + 1
			text.WriteString(strconv.Itoa(value))
		}
		outputs[outputIndex] = ModelTraceOutput{Text: text.String(), ExpectedCount: length}
	}
	return outputs
}

func assertModelTraceClose(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-10 {
		t.Fatalf("got %.15f, want %.15f", got, want)
	}
}
