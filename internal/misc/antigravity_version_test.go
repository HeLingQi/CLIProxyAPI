package misc

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func overrideAntigravityVersionURLsForTest(t *testing.T, cliManifestURL string) func() {
	t.Helper()

	oldCLIManifest := antigravityCLILatestManifestURL
	antigravityCLILatestManifestURL = cliManifestURL

	return func() {
		antigravityCLILatestManifestURL = oldCLIManifest
	}
}

func overrideAntigravityVersionCacheForTest(t *testing.T, client antigravityClientInfo, expiry time.Time) func() {
	t.Helper()

	antigravityVersionMu.Lock()
	oldClient := cachedAntigravityClient
	oldExpiry := antigravityVersionExpiry
	cachedAntigravityClient = client
	antigravityVersionExpiry = expiry
	antigravityVersionMu.Unlock()

	return func() {
		antigravityVersionMu.Lock()
		cachedAntigravityClient = oldClient
		antigravityVersionExpiry = oldExpiry
		antigravityVersionMu.Unlock()
	}
}

func TestAntigravityLatestVersionUsesCurrentCLIFallback(t *testing.T) {
	restore := overrideAntigravityVersionCacheForTest(t, antigravityClientInfo{}, time.Time{})
	defer restore()

	version := AntigravityLatestVersion()
	if version != antigravityCLIFallbackVersion {
		t.Fatalf("AntigravityLatestVersion() = %q, want %q", version, antigravityCLIFallbackVersion)
	}
}

// The backend hides the gemini-3.8-flash family from CLI clients below 1.2.0, so the
// offline fallback must stay at or above that release.
func TestAntigravityFallbackVersionMeetsBackendFloor(t *testing.T) {
	const floorMajor, floorMinor = 1, 2

	var major, minor, patch int
	if _, err := fmt.Sscanf(antigravityCLIFallbackVersion, "%d.%d.%d", &major, &minor, &patch); err != nil {
		t.Fatalf("antigravityCLIFallbackVersion = %q is not a dotted version: %v", antigravityCLIFallbackVersion, err)
	}
	if major < floorMajor || (major == floorMajor && minor < floorMinor) {
		t.Fatalf("antigravityCLIFallbackVersion = %q, want at least %d.%d.0", antigravityCLIFallbackVersion, floorMajor, floorMinor)
	}
}

// Changelists at or below 1013060670 hide the swarm orchestrator and worker models.
func TestAntigravityFallbackChangelistMeetsBackendFloor(t *testing.T) {
	const floor = 1017000000

	changelist, errParse := strconv.ParseInt(antigravityCLIFallbackChangelist, 10, 64)
	if errParse != nil {
		t.Fatalf("antigravityCLIFallbackChangelist = %q is not a number: %v", antigravityCLIFallbackChangelist, errParse)
	}
	if changelist < floor {
		t.Fatalf("antigravityCLIFallbackChangelist = %q, want at least %d", antigravityCLIFallbackChangelist, floor)
	}
}

func TestAntigravityUserAgentUsesCLIFamily(t *testing.T) {
	for _, tc := range []struct {
		name   string
		client antigravityClientInfo
		want   string
	}{
		{"fallback", antigravityClientInfo{antigravityCLIFallbackVersion, antigravityCLIFallbackChangelist}, "antigravity/cli/1.5.7 (aidev_client; os_type=linux; arch=amd64; cl=1019769835; auth_method=consumer)"},
		{"manifest build id", antigravityClientInfo{"1.6.0", "6190022064472064"}, "antigravity/cli/1.6.0 (aidev_client; os_type=linux; arch=amd64; cl=6190022064472064; auth_method=consumer)"},
		{"unknown changelist", antigravityClientInfo{"1.6.0", ""}, "antigravity/cli/1.6.0 (aidev_client; os_type=linux; arch=amd64; auth_method=consumer)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := overrideAntigravityVersionCacheForTest(t, tc.client, time.Now().Add(time.Hour))
			defer restore()
			if got := AntigravityUserAgent(); got != tc.want {
				t.Fatalf("AntigravityUserAgent() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAntigravityVersionFromUserAgentParsesCLIFamily(t *testing.T) {
	for _, userAgent := range []string{
		"antigravity/cli/1.5.7 linux/amd64",
		"antigravity/cli/1.5.7 (aidev_client; os_type=linux; arch=amd64; cl=1019769835; auth_method=consumer)",
	} {
		if got := AntigravityVersionFromUserAgent(userAgent); got != "1.5.7" {
			t.Fatalf("AntigravityVersionFromUserAgent(%q) = %q, want %q", userAgent, got, "1.5.7")
		}
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

func TestAntigravityLoadCodeAssistUserAgentUsesShortUA(t *testing.T) {
	restore := overrideAntigravityVersionCacheForTest(t, antigravityClientInfo{version: "1.5.7"}, time.Now().Add(time.Hour))
	defer restore()

	want := "antigravity/cli/1.5.7 (aidev_client; os_type=linux; arch=amd64; auth_method=consumer)"
	if got := AntigravityLoadCodeAssistUserAgent(""); got != want {
		t.Fatalf("AntigravityLoadCodeAssistUserAgent() = %q, want %q", got, want)
	}
	if got := AntigravityLoadCodeAssistUserAgent(want); got != want {
		t.Fatalf("AntigravityLoadCodeAssistUserAgent(configured) = %q, want %q", got, want)
	}
}

// antigravityCLITestReleaseURL mirrors the archive layout of the live manifest, where
// the changelist trails the version in the directory before the platform segment.
func antigravityCLITestReleaseURL(version, changelist string) string {
	return fmt.Sprintf("https://storage.googleapis.com/antigravity-public/antigravity-cli/%s-%s/linux-x64/cli_linux_x64.tar.gz", version, changelist)
}

func TestFetchAntigravityCLIClientInfoUsesManifest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifests/linux_amd64.json":
			// The CLI updater uses the plain Go client, so no UA override is sent.
			if got := r.Header.Get("User-Agent"); !strings.HasPrefix(got, "Go-http-client/") {
				t.Errorf("CLI manifest User-Agent = %q, want the Go client default", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"version":"1.6.0","url":%q,"sha512":"abc"}`, antigravityCLITestReleaseURL("1.6.0", "6190022064472064"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	restore := overrideAntigravityVersionURLsForTest(t, server.URL+"/manifests/linux_amd64.json")
	defer restore()

	client, errFetch := fetchAntigravityCLIClientInfo(context.Background())
	if errFetch != nil {
		t.Fatalf("fetchAntigravityCLIClientInfo() error = %v", errFetch)
	}
	if client.version != "1.6.0" || client.changelist != "6190022064472064" {
		t.Fatalf("fetchAntigravityCLIClientInfo() = %+v, want version 1.6.0 and changelist 6190022064472064", client)
	}
}

func TestFetchAntigravityCLIClientInfoKeepsFloorWithoutArchiveURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"1.6.0"}`))
	}))
	defer server.Close()

	restore := overrideAntigravityVersionURLsForTest(t, server.URL+"/manifests/linux_amd64.json")
	defer restore()

	client, errFetch := fetchAntigravityCLIClientInfo(context.Background())
	if errFetch != nil {
		t.Fatalf("fetchAntigravityCLIClientInfo() error = %v", errFetch)
	}
	if client.changelist != antigravityCLIFallbackChangelist {
		t.Fatalf("fetchAntigravityCLIClientInfo() changelist = %q, want %q", client.changelist, antigravityCLIFallbackChangelist)
	}
}

func TestFetchAntigravityCLIClientInfoRejectsInvalidVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"latest","url":"https://example.invalid/x"}`))
	}))
	defer server.Close()

	restore := overrideAntigravityVersionURLsForTest(t, server.URL+"/manifests/linux_amd64.json")
	defer restore()

	if _, errFetch := fetchAntigravityCLIClientInfo(context.Background()); errFetch == nil {
		t.Fatal("fetchAntigravityCLIClientInfo() error = nil, want error")
	}
}

func TestFetchAntigravityCLIClientInfoReturnsManifestError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary outage", http.StatusInternalServerError)
	}))
	defer server.Close()

	restore := overrideAntigravityVersionURLsForTest(t, server.URL+"/manifests/linux_amd64.json")
	defer restore()

	if _, errFetch := fetchAntigravityCLIClientInfo(context.Background()); errFetch == nil {
		t.Fatal("fetchAntigravityCLIClientInfo() error = nil, want error")
	}
}
