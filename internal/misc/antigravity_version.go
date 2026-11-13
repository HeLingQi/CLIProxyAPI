// Package misc provides miscellaneous utility functions for the CLI Proxy API server.
package misc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// antigravityCLIFallbackVersion and antigravityCLIFallbackChangelist form the
	// client fingerprint reported when the CLI updater manifest has not been fetched
	// yet or cannot be reached. The backend gates model visibility on both fields:
	// releases below 1.2.0 lose the gemini-3.8-flash family and a changelist below
	// ~1.017e9 hides the swarm orchestrator and worker models, so this fallback must
	// stay on a verified recent release. The values below were taken from the
	// released 1.5.7 binary.
	antigravityCLIFallbackVersion    = "1.5.7"
	antigravityCLIFallbackChangelist = "1019769835"
	// antigravityCLIPlatform mirrors the manifest that is fetched. The backend does
	// not vary behavior by platform pair, so it stays fixed to keep the User-Agent
	// and the manifest describing the same build family.
	antigravityCLIPlatform     = "os_type=linux; arch=amd64"
	antigravityVersionCacheTTL = 6 * time.Hour
	antigravityFetchTimeout    = 10 * time.Second
)

var (
	antigravityCLILatestManifestURL = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/linux_amd64.json"
)

// antigravityCLIManifest is the CLI auto-updater manifest. The release archive URL
// names the build as "<version>-<changelist>" in the path segment before the platform
// directory, for example .../antigravity-cli/1.5.7-6190022064472064/linux-x64/….
// The backend only compares changelists with ">=", so that build id passes the same
// feature gates as the changelist embedded in the released binary.
type antigravityCLIManifest struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

// antigravityClientInfo identifies the Antigravity release that requests impersonate.
type antigravityClientInfo struct {
	version    string
	changelist string
}

var (
	cachedAntigravityClient  = antigravityClientInfo{version: antigravityCLIFallbackVersion, changelist: antigravityCLIFallbackChangelist}
	antigravityVersionMu     sync.RWMutex
	antigravityVersionExpiry time.Time
	antigravityUpdaterOnce   sync.Once
)

// StartAntigravityVersionUpdater starts a background goroutine that periodically refreshes the cached antigravity version.
// This is intentionally decoupled from request execution to avoid blocking executors on version lookups.
func StartAntigravityVersionUpdater(ctx context.Context) {
	antigravityUpdaterOnce.Do(func() {
		go runAntigravityVersionUpdater(ctx)
	})
}

func runAntigravityVersionUpdater(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	ticker := time.NewTicker(antigravityVersionCacheTTL / 2)
	defer ticker.Stop()

	log.Infof("periodic antigravity version refresh started (interval=%s)", antigravityVersionCacheTTL/2)

	refreshAntigravityVersion(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshAntigravityVersion(ctx)
		}
	}
}

func refreshAntigravityVersion(ctx context.Context) {
	client, errFetch := fetchAntigravityCLIClientInfo(ctx)

	antigravityVersionMu.Lock()
	defer antigravityVersionMu.Unlock()

	now := time.Now()

	if errFetch == nil {
		cachedAntigravityClient = client
		antigravityVersionExpiry = now.Add(antigravityVersionCacheTTL)
		log.WithFields(log.Fields{"version": client.version, "changelist": client.changelist}).Info("fetched latest antigravity CLI version")
		return
	}

	if cachedAntigravityClient.version == "" || now.After(antigravityVersionExpiry) {
		cachedAntigravityClient = fallbackAntigravityClientInfo()
		antigravityVersionExpiry = now.Add(antigravityVersionCacheTTL)
		log.WithError(errFetch).Warn("failed to refresh antigravity version, using fallback version")
		return
	}

	log.WithError(errFetch).Debug("failed to refresh antigravity version, keeping cached value")
}

// AntigravityLatestVersion returns the cached antigravity version refreshed by StartAntigravityVersionUpdater.
// It falls back to antigravityCLIFallbackVersion if the cache is empty or stale.
func AntigravityLatestVersion() string {
	return currentAntigravityClientInfo().version
}

// currentAntigravityClientInfo returns the fingerprint to report, falling back to the
// verified release below when the cache is empty or stale. Version and changelist are
// returned together so a fresh version never carries another release's changelist.
func currentAntigravityClientInfo() antigravityClientInfo {
	antigravityVersionMu.RLock()
	client := cachedAntigravityClient
	expiry := antigravityVersionExpiry
	antigravityVersionMu.RUnlock()

	if client.version == "" || time.Now().After(expiry) {
		return fallbackAntigravityClientInfo()
	}
	return client
}

func fallbackAntigravityClientInfo() antigravityClientInfo {
	return antigravityClientInfo{version: antigravityCLIFallbackVersion, changelist: antigravityCLIFallbackChangelist}
}

// AntigravityUserAgent returns the User-Agent string used by the Antigravity CLI, the
// client family whose requests this proxy mirrors.
func AntigravityUserAgent() string {
	client := currentAntigravityClientInfo()
	metadata := "aidev_client; " + antigravityCLIPlatform
	if client.changelist != "" {
		metadata += "; cl=" + client.changelist
	}
	return fmt.Sprintf("antigravity/cli/%s (%s; auth_method=consumer)", client.version, metadata)
}

func isAntigravityFamilyUserAgent(lower string) bool {
	return strings.HasPrefix(lower, "antigravity/cli/") || strings.HasPrefix(lower, "antigravity/")
}

func antigravityBaseUserAgent(userAgent string) string {
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		return AntigravityUserAgent()
	}
	lower := strings.ToLower(userAgent)
	if isAntigravityFamilyUserAgent(lower) {
		if idx := strings.Index(lower, " google-api-nodejs-client/"); idx >= 0 {
			trimmed := strings.TrimSpace(userAgent[:idx])
			if trimmed != "" {
				return trimmed
			}
		}
	}
	return userAgent
}

// AntigravityRequestUserAgent returns the Antigravity runtime UA used by
// generate/stream/model-list requests.
func AntigravityRequestUserAgent(userAgent string) string {
	return antigravityBaseUserAgent(userAgent)
}

// AntigravityLoadCodeAssistUserAgent returns the Antigravity runtime UA used by
// loadCodeAssist requests.
func AntigravityLoadCodeAssistUserAgent(userAgent string) string {
	return AntigravityRequestUserAgent(userAgent)
}

// AntigravityVersionFromUserAgent extracts the Antigravity version prefix from
// either the family-qualified or the legacy Antigravity UA forms.
func AntigravityVersionFromUserAgent(userAgent string) string {
	base := antigravityBaseUserAgent(userAgent)
	lower := strings.ToLower(base)
	for _, prefix := range []string{"antigravity/cli/", "antigravity/hub/", "antigravity/"} {
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		rest := strings.TrimSpace(base[len(prefix):])
		if idx := strings.IndexAny(rest, " \t("); idx >= 0 {
			rest = strings.TrimSpace(rest[:idx])
		}
		if rest != "" {
			return rest
		}
		break
	}
	return AntigravityLatestVersion()
}

func fetchAntigravityCLIClientInfo(ctx context.Context) (antigravityClientInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	client := &http.Client{Timeout: antigravityFetchTimeout}
	return fetchAntigravityCLIManifest(ctx, client)
}

func fetchAntigravityCLIManifest(ctx context.Context, client *http.Client) (antigravityClientInfo, error) {
	httpReq, errReq := http.NewRequestWithContext(ctx, http.MethodGet, antigravityCLILatestManifestURL, nil)
	if errReq != nil {
		return antigravityClientInfo{}, fmt.Errorf("build antigravity CLI updater manifest request: %w", errReq)
	}
	// The CLI updater fetches the manifest with the plain Go client, so no User-Agent
	// or Cache-Control override is sent here.

	resp, errDo := client.Do(httpReq)
	if errDo != nil {
		return antigravityClientInfo{}, fmt.Errorf("fetch antigravity CLI updater manifest: %w", errDo)
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Warn("antigravity CLI updater manifest response body close error")
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return antigravityClientInfo{}, fmt.Errorf("antigravity CLI updater manifest returned status %d", resp.StatusCode)
	}

	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if errRead != nil {
		return antigravityClientInfo{}, fmt.Errorf("read antigravity CLI updater manifest: %w", errRead)
	}

	var manifest antigravityCLIManifest
	if errDecode := json.Unmarshal(raw, &manifest); errDecode != nil {
		return antigravityClientInfo{}, fmt.Errorf("decode antigravity CLI updater manifest: %w", errDecode)
	}

	version := strings.TrimSpace(manifest.Version)
	if version == "" {
		return antigravityClientInfo{}, errors.New("antigravity CLI updater manifest returned empty version")
	}
	if !isValidAntigravitySemVersion(version) {
		return antigravityClientInfo{}, fmt.Errorf("antigravity CLI updater manifest returned invalid version %q", version)
	}

	changelist := antigravityChangelistFromReleaseURL(manifest.URL, version)
	if changelist == "" {
		// An unexpected archive layout must not cost the feature gates that the
		// fallback changelist already satisfies.
		changelist = antigravityCLIFallbackChangelist
	}
	return antigravityClientInfo{version: version, changelist: changelist}, nil
}

// antigravityChangelistFromReleaseURL picks the build id out of the
// "<version>-<changelist>" archive directory for the reported release version.
func antigravityChangelistFromReleaseURL(releaseURL, version string) string {
	if strings.TrimSpace(releaseURL) == "" || version == "" {
		return ""
	}
	parsed, errParse := url.Parse(releaseURL)
	if errParse != nil {
		return ""
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		build, hasPrefix := strings.CutPrefix(segment, version+"-")
		if hasPrefix && isAntigravityChangelist(build) {
			return build
		}
	}
	return ""
}

func isValidAntigravitySemVersion(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}

	for _, part := range parts {
		if !isNumericToken(part) {
			return false
		}
	}

	return true
}

// isAntigravityChangelist reports whether the value can be read as a changelist by the
// backend, which ignores the field entirely when it is not a number.
func isAntigravityChangelist(value string) bool {
	return isNumericToken(value)
}

func isNumericToken(value string) bool {
	if value == "" {
		return false
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}
