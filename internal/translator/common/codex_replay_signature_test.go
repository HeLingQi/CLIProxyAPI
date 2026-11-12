package common

import (
	"github.com/MoeForge/moego/base64"
	"strings"
	"testing"
)

func testCodexReplayGPTSignature() string {
	decoded := make([]byte, 89)
	decoded[0] = 0x80
	for i := 9; i < len(decoded); i++ {
		decoded[i] = byte(i)
	}
	return base64.RawURLEncoding.EncodeToString(decoded)
}

func TestCodexReplaySignatureForTarget(t *testing.T) {
	gptSig := testCodexReplayGPTSignature()

	cases := []struct {
		name  string
		model string
		sig   string
		want  string
		ok    bool
	}{
		{"gpt on gpt target", "gpt-5.4", gptSig, gptSig, true},
		{"routed gpt on openrouter gpt target", "openrouter/openai/gpt-5.6-luna-pro", gptSig + "." + strings.Repeat("Ab3", 27)[:79], gptSig + "." + strings.Repeat("Ab3", 27)[:79], true},
		{"gpt cache prefix normalized", "gpt-5.4", "gpt#" + gptSig, gptSig, true},
		// An unmapped target defaults to the Codex GPT family.
		{"gpt on unmapped target", "deepseek-v4", gptSig, gptSig, true},
		{"empty signature", "gpt-5.4", "", "", false},
		{"unknown format", "gpt-5.4", "opaque-blob-xyz", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := CodexReplaySignatureForTarget(tc.model, tc.sig)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("CodexReplaySignatureForTarget(%q, sig) = (%v, %v), want (%v, %v)", tc.model, truncateForTest(got), ok, truncateForTest(tc.want), tc.ok)
			}
		})
	}
}

func truncateForTest(s string) string {
	if len(s) > 24 {
		return s[:24] + "..."
	}
	return s
}
