package misc

import (
	"fmt"
	"testing"
)

func TestAntigravityPinnedVersionIsReported(t *testing.T) {
	if got := AntigravityLatestVersion(); got != antigravityPinnedVersion {
		t.Fatalf("AntigravityLatestVersion() = %q, want %q", got, antigravityPinnedVersion)
	}
	if got := AntigravityChangelist(); got != antigravityPinnedChangelist {
		t.Fatalf("AntigravityChangelist() = %q, want %q", got, antigravityPinnedChangelist)
	}
}

// Cloud Code resolves newer models only for clients reporting at least 2.23.0;
// older versions get 404 Requested entity was not found.
func TestAntigravityPinnedVersionMeetsBackendFloor(t *testing.T) {
	const floorMajor, floorMinor = 2, 23

	var major, minor, patch int
	if _, err := fmt.Sscanf(antigravityPinnedVersion, "%d.%d.%d", &major, &minor, &patch); err != nil {
		t.Fatalf("antigravityPinnedVersion = %q is not a dotted version: %v", antigravityPinnedVersion, err)
	}
	if major < floorMajor || (major == floorMajor && minor < floorMinor) {
		t.Fatalf("antigravityPinnedVersion = %q, want at least %d.%d.0", antigravityPinnedVersion, floorMajor, floorMinor)
	}
}

func TestAntigravityUserAgentUsesHubFamily(t *testing.T) {
	want := "antigravity/hub/2.31.2 (aidev_client; os_type=darwin; arch=arm64; cl=1019359392)"
	if got := AntigravityUserAgent(); got != want {
		t.Fatalf("AntigravityUserAgent() = %q, want %q", got, want)
	}
}

func TestAntigravitySessionIDIsSignedFNV1A64OfEmptyInput(t *testing.T) {
	// The official client hashes no input at all, so the value is fixed by the
	// hash parameters: offset basis 0xcbf29ce484222325 read as a signed int64.
	offsetBasis := uint64(0xcbf29ce484222325)
	want := fmt.Sprintf("%d", int64(offsetBasis))
	if AntigravitySessionID != want {
		t.Fatalf("AntigravitySessionID = %q, want %q", AntigravitySessionID, want)
	}
}

func TestAntigravityVersionFromUserAgentParsesHubFamily(t *testing.T) {
	for _, userAgent := range []string{
		"antigravity/hub/2.24.0 darwin/arm64",
		"antigravity/hub/2.24.0 (aidev_client; os_type=darwin; arch=arm64; cl=1000335682)",
		"antigravity/hub/2.24.0 (aidev_client; os_type=darwin; arch=arm64; cl=1000335682) google-api-nodejs-client/10.3.0",
	} {
		if got := AntigravityVersionFromUserAgent(userAgent); got != "2.24.0" {
			t.Fatalf("AntigravityVersionFromUserAgent(%q) = %q, want %q", userAgent, got, "2.24.0")
		}
	}
}

func TestAntigravityVersionFromUserAgentParsesLegacyFamily(t *testing.T) {
	if got := AntigravityVersionFromUserAgent("antigravity/1.23.2 windows/amd64"); got != "1.23.2" {
		t.Fatalf("AntigravityVersionFromUserAgent() = %q, want %q", got, "1.23.2")
	}
}

func TestAntigravityVersionFromUserAgentFallsBackToPinnedVersion(t *testing.T) {
	if got := AntigravityVersionFromUserAgent("claude-cli/1.0.0"); got != antigravityPinnedVersion {
		t.Fatalf("AntigravityVersionFromUserAgent() = %q, want %q", got, antigravityPinnedVersion)
	}
}

func TestAntigravityLoadCodeAssistUserAgentUsesShortUA(t *testing.T) {
	want := "antigravity/hub/2.31.2 (aidev_client; os_type=darwin; arch=arm64; cl=1019359392)"
	if got := AntigravityLoadCodeAssistUserAgent(""); got != want {
		t.Fatalf("AntigravityLoadCodeAssistUserAgent() = %q, want %q", got, want)
	}
	if got := AntigravityLoadCodeAssistUserAgent(want); got != want {
		t.Fatalf("AntigravityLoadCodeAssistUserAgent(configured) = %q, want %q", got, want)
	}
}

func TestAntigravityOnboardUserUserAgentUsesLongUA(t *testing.T) {
	want := "antigravity/hub/2.31.2 (aidev_client; os_type=darwin; arch=arm64; cl=1019359392) google-api-nodejs-client/10.3.0"
	if got := AntigravityOnboardUserUserAgent(""); got != want {
		t.Fatalf("AntigravityOnboardUserUserAgent() = %q, want %q", got, want)
	}
}

func TestConfigureAntigravityPinnedIdentityAtomicOverride(t *testing.T) {
	t.Cleanup(func() {
		ConfigureAntigravityPinnedIdentity("", "")
	})

	if ConfigureAntigravityPinnedIdentity("2.35.0", "") {
		t.Fatal("expected partial override without changelist to be rejected")
	}
	if ConfigureAntigravityPinnedIdentity("2.35.0", "not-numeric") {
		t.Fatal("expected non-numeric changelist to be rejected")
	}
	if got := AntigravityLatestVersion(); got != antigravityPinnedVersion {
		t.Fatalf("version after rejected override = %q, want %q", got, antigravityPinnedVersion)
	}

	if !ConfigureAntigravityPinnedIdentity("2.35.0", "1099999999") {
		t.Fatal("expected valid atomic pair override to succeed")
	}
	wantUA := "antigravity/hub/2.35.0 (aidev_client; os_type=darwin; arch=arm64; cl=1099999999)"
	if got := AntigravityUserAgent(); got != wantUA {
		t.Fatalf("AntigravityUserAgent() = %q, want %q", got, wantUA)
	}

	// A subsequent partial or malformed hot-reload must reset back to the built-in
	// pinned pair rather than leaving the previous override stale.
	if ConfigureAntigravityPinnedIdentity("2.36.0", "") {
		t.Fatal("expected partial hot-reload override to return false")
	}
	if got := AntigravityLatestVersion(); got != antigravityPinnedVersion {
		t.Fatalf("version after malformed hot-reload = %q, want built-in %q", got, antigravityPinnedVersion)
	}
	if got := AntigravityChangelist(); got != antigravityPinnedChangelist {
		t.Fatalf("changelist after malformed hot-reload = %q, want built-in %q", got, antigravityPinnedChangelist)
	}

	if !ConfigureAntigravityPinnedIdentity("2.35.0", "1099999999") {
		t.Fatal("expected valid override after reset to succeed")
	}
	if !ConfigureAntigravityPinnedIdentity("", "") {
		t.Fatal("expected empty override to reset back to built-in pin")
	}
	if got := AntigravityLatestVersion(); got != antigravityPinnedVersion {
		t.Fatalf("version after reset = %q, want %q", got, antigravityPinnedVersion)
	}
}
