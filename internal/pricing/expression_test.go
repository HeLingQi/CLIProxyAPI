package pricing

import (
	"strings"
	"testing"
	"time"
)

func TestPricingExpressionEvaluatesStandardTokenBuckets(t *testing.T) {
	expression, errCompile := compileExpression(`tier("standard", p * 2 + cr * 0.2 + cc * 2.5 + cc1h * 4 + cc12h * 8 + c * 10 + ai * 3 + ao * 6)`)
	if errCompile != nil {
		t.Fatalf("compileExpression() error = %v", errCompile)
	}

	result, errEvaluate := expression.Evaluate(expressionContext{
		UncachedInput:   1_000_000,
		CacheReadInput:  1_000_000,
		CacheWriteInput: 1_000_000,
		CacheWrite1h:    1_000_000,
		CacheWrite12h:   1_000_000,
		Output:          1_000_000,
		AudioInput:      1_000_000,
		AudioOutput:     1_000_000,
	})
	if errEvaluate != nil {
		t.Fatalf("Evaluate() error = %v", errEvaluate)
	}
	if result.USDMicros != 35_700_000 || result.Tier != "standard" {
		t.Fatalf("Evaluate() = %+v, want 35700000 micro-USD in standard tier", result)
	}
}

func TestPricingExpressionSelectsContextLengthTier(t *testing.T) {
	expression, errCompile := compileExpression(`len <= 100000 ? tier("0_100k", p * 1 + c * 5) : tier("100k_plus", p * 2 + c * 10)`)
	if errCompile != nil {
		t.Fatalf("compileExpression() error = %v", errCompile)
	}

	lower, errLower := expression.Evaluate(expressionContext{InputLength: 100_000, UncachedInput: 1_000_000, Output: 1_000_000})
	if errLower != nil {
		t.Fatalf("lower Evaluate() error = %v", errLower)
	}
	higher, errHigher := expression.Evaluate(expressionContext{InputLength: 100_001, UncachedInput: 1_000_000, Output: 1_000_000})
	if errHigher != nil {
		t.Fatalf("higher Evaluate() error = %v", errHigher)
	}
	if lower.USDMicros != 6_000_000 || lower.Tier != "0_100k" {
		t.Fatalf("lower = %+v", lower)
	}
	if higher.USDMicros != 12_000_000 || higher.Tier != "100k_plus" {
		t.Fatalf("higher = %+v", higher)
	}
}

func TestPricingExpressionEvaluatesTimeAndThinkingConditions(t *testing.T) {
	timeExpression, errTime := compileExpression(`weekday("UTC") >= 1 && weekday("UTC") <= 5 && (hour("UTC") >= 1 && hour("UTC") < 4) ? tier("peak", p * 2) : tier("off_peak", p * 1)`)
	if errTime != nil {
		t.Fatalf("compile time expression: %v", errTime)
	}
	thinkingExpression, errThinking := compileExpression(`param("enable_thinking") == true ? tier("thinking", c * 4) : tier("standard", c * 1)`)
	if errThinking != nil {
		t.Fatalf("compile thinking expression: %v", errThinking)
	}

	peak, errPeak := timeExpression.Evaluate(expressionContext{At: time.Date(2026, time.November, 16, 2, 0, 0, 0, time.UTC), UncachedInput: 1_000_000})
	if errPeak != nil || peak.USDMicros != 2_000_000 || peak.Tier != "peak" {
		t.Fatalf("peak = %+v, err = %v", peak, errPeak)
	}
	offPeak, errOffPeak := timeExpression.Evaluate(expressionContext{At: time.Date(2026, time.November, 15, 2, 0, 0, 0, time.UTC), UncachedInput: 1_000_000})
	if errOffPeak != nil || offPeak.USDMicros != 1_000_000 || offPeak.Tier != "off_peak" {
		t.Fatalf("offPeak = %+v, err = %v", offPeak, errOffPeak)
	}
	thinking, errThinkingEvaluate := thinkingExpression.Evaluate(expressionContext{Thinking: true, Output: 1_000_000})
	if errThinkingEvaluate != nil || thinking.USDMicros != 4_000_000 || thinking.Tier != "thinking" {
		t.Fatalf("thinking = %+v, err = %v", thinking, errThinkingEvaluate)
	}
}

func TestPricingExpressionEvaluatesFixedImageCharge(t *testing.T) {
	expression, errCompile := compileExpression(`tier("image", fixed(0.0035)) * image_count`)
	if errCompile != nil {
		t.Fatalf("compileExpression() error = %v", errCompile)
	}
	result, errEvaluate := expression.Evaluate(expressionContext{ImageCount: 2})
	if errEvaluate != nil {
		t.Fatalf("Evaluate() error = %v", errEvaluate)
	}
	if result.USDMicros != 7_000 || result.Tier != "image" {
		t.Fatalf("Evaluate() = %+v, want 7000 micro-USD in image tier", result)
	}
}

func TestPricingExpressionRejectsUnsupportedOrUnsafeInput(t *testing.T) {
	for _, source := range []string{
		`p / 2`,
		`unknown * 1`,
		`tier("standard", p) + shell("rm")`,
		`param("other") ? p : c`,
		strings.Repeat("(", 70) + `p` + strings.Repeat(")", 70),
	} {
		if _, errCompile := compileExpression(source); errCompile == nil {
			t.Errorf("compileExpression(%q) succeeded, want error", source)
		}
	}
}

func TestPricingExpressionRejectsNegativeContextAndOverflow(t *testing.T) {
	expression, errCompile := compileExpression(`tier("standard", p * 999999999999999999999999)`)
	if errCompile != nil {
		t.Fatalf("compileExpression() error = %v", errCompile)
	}
	if _, errEvaluate := expression.Evaluate(expressionContext{UncachedInput: -1}); errEvaluate == nil {
		t.Fatal("Evaluate() accepted negative token count")
	}
	if _, errEvaluate := expression.Evaluate(expressionContext{UncachedInput: 1_000_000}); errEvaluate == nil {
		t.Fatal("Evaluate() accepted micro-USD overflow")
	}
}
