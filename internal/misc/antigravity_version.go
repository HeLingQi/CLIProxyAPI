// Package misc provides miscellaneous utility functions for the CLI Proxy API server.
package misc

import (
	"fmt"
	"strings"
	"sync"
)

const (
	// antigravityPinnedVersion is the single Antigravity Hub release emulated on
	// the wire. Cloud Code rejects newer models for clients below 2.23.0, so the
	// pin must stay at or above that version.
	//
	// Reported version and changelist are one atomic identity: the official
	// client derives both from the same build, so discovering the newest Hub
	// version at runtime while shipping an older request shape would advertise a
	// build that never behaved that way. The changelist below was read from the
	// official darwin-arm archive of the same release (language_server -stamp).
	antigravityPinnedVersion    = "2.31.2"
	antigravityPinnedChangelist = "1019359392"
	antigravityHubPlatform      = "os_type=darwin; arch=arm64"
	AntigravityNodeAPIClientUA  = "google-api-nodejs-client/10.3.0"
	AntigravityGoogAPIClientUA  = "gl-node/22.21.1"
)

// AntigravitySessionID is the constant request.sessionId every official
// Antigravity client sends. It is not a conversation id: the client derives it
// once from the empty string through FNV-1a/64 (offset basis 0xcbf29ce484222325
// read as a signed 64-bit integer) and reuses it for every request, so one
// account never shows more than one sessionId. Conversation grouping lives in
// request.labels.cascade_id.
const AntigravitySessionID = "-3750763034362895579"

type antigravityPinnedIdentity struct {
	version    string
	changelist string
}

var (
	antigravityPinnedMu       sync.RWMutex
	antigravityActiveIdentity = antigravityPinnedIdentity{
		version:    antigravityPinnedVersion,
		changelist: antigravityPinnedChangelist,
	}
)

// ConfigureAntigravityPinnedIdentity atomically overrides the reported
// Antigravity Hub (version, changelist) pair when both values are non-empty and
// syntactically valid. Passing empty values resets back to the built-in pin.
// Partial or malformed overrides also reset back to the built-in pin and return
// false so hot-reloading an incomplete override never leaves stale values from
// a previous generation active and reported version/changelist never drift.
func ConfigureAntigravityPinnedIdentity(version, changelist string) bool {
	version = strings.TrimSpace(version)
	changelist = strings.TrimSpace(changelist)
	antigravityPinnedMu.Lock()
	defer antigravityPinnedMu.Unlock()
	if version == "" && changelist == "" {
		antigravityActiveIdentity = antigravityPinnedIdentity{
			version:    antigravityPinnedVersion,
			changelist: antigravityPinnedChangelist,
		}
		return true
	}
	if !isValidAntigravityPinnedVersion(version) || !isValidAntigravityPinnedChangelist(changelist) {
		antigravityActiveIdentity = antigravityPinnedIdentity{
			version:    antigravityPinnedVersion,
			changelist: antigravityPinnedChangelist,
		}
		return false
	}
	antigravityActiveIdentity = antigravityPinnedIdentity{
		version:    version,
		changelist: changelist,
	}
	return true
}

func isValidAntigravityPinnedVersion(version string) bool {
	if version == "" || strings.ContainsAny(version, " \t\r\n/();") {
		return false
	}
	return strings.Contains(version, ".")
}

func isValidAntigravityPinnedChangelist(changelist string) bool {
	if changelist == "" {
		return false
	}
	for i := 0; i < len(changelist); i++ {
		if changelist[i] < '0' || changelist[i] > '9' {
			return false
		}
	}
	return true
}

func currentAntigravityPinnedIdentity() antigravityPinnedIdentity {
	antigravityPinnedMu.RLock()
	id := antigravityActiveIdentity
	antigravityPinnedMu.RUnlock()
	return id
}

// AntigravityLatestVersion returns the Antigravity Hub version reported upstream.
func AntigravityLatestVersion() string {
	return currentAntigravityPinnedIdentity().version
}

// AntigravityChangelist returns the changelist id paired with the reported version.
func AntigravityChangelist() string {
	return currentAntigravityPinnedIdentity().changelist
}

// AntigravityUserAgent returns the User-Agent string used by the Antigravity Hub family.
func AntigravityUserAgent() string {
	id := currentAntigravityPinnedIdentity()
	return fmt.Sprintf("antigravity/hub/%s (aidev_client; %s; cl=%s)", id.version, antigravityHubPlatform, id.changelist)
}

func isAntigravityFamilyUserAgent(lower string) bool {
	return strings.HasPrefix(lower, "antigravity/hub/") || strings.HasPrefix(lower, "antigravity/")
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

// AntigravityOnboardUserUserAgent returns the long Antigravity control-plane UA
// used by onboardUser requests.
func AntigravityOnboardUserUserAgent(userAgent string) string {
	userAgent = strings.TrimSpace(userAgent)
	if userAgent == "" {
		return AntigravityUserAgent() + " " + AntigravityNodeAPIClientUA
	}
	lower := strings.ToLower(userAgent)
	if !isAntigravityFamilyUserAgent(lower) {
		return userAgent
	}
	if strings.Contains(lower, "google-api-nodejs-client/") {
		return userAgent
	}
	return antigravityBaseUserAgent(userAgent) + " " + AntigravityNodeAPIClientUA
}

// AntigravityVersionFromUserAgent extracts the Antigravity version prefix from
// either the short or long Antigravity UA forms.
func AntigravityVersionFromUserAgent(userAgent string) string {
	base := antigravityBaseUserAgent(userAgent)
	lower := strings.ToLower(base)
	if strings.HasPrefix(lower, "antigravity/hub/") {
		rest := base[len("antigravity/hub/"):]
		if idx := strings.IndexAny(rest, " 	"); idx >= 0 {
			rest = rest[:idx]
		}
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return AntigravityLatestVersion()
		}
		return rest
	}
	const legacyPrefix = "antigravity/"
	if !strings.HasPrefix(lower, legacyPrefix) {
		return AntigravityLatestVersion()
	}
	rest := base[len(legacyPrefix):]
	if idx := strings.IndexAny(rest, " 	"); idx >= 0 {
		rest = rest[:idx]
	}
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return AntigravityLatestVersion()
	}
	return rest
}
