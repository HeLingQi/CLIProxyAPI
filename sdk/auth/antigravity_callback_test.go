package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAntigravityCallbackListenAddrBindsLoopbackForEphemeralPort(t *testing.T) {
	if got := antigravityCallbackListenAddr(0); got != "127.0.0.1:0" {
		t.Fatalf("antigravityCallbackListenAddr(0) = %q, want 127.0.0.1:0", got)
	}
	if got := antigravityCallbackListenAddr(51121); got != ":51121" {
		t.Fatalf("antigravityCallbackListenAddr(51121) = %q, want :51121", got)
	}
	if got := antigravityCallbackListenAddr(-1); got != ":51121" {
		t.Fatalf("antigravityCallbackListenAddr(-1) = %q, want :51121", got)
	}
}

func TestAntigravityCallbackHandlerIgnoresProbesAndStateMismatches(t *testing.T) {
	resultCh := make(chan callbackResult, 1)
	handler := newAntigravityCallbackHandler("/auth/callback", "expected-state-123", resultCh)

	// 1. Empty query probe must return 400 and never occupy resultCh.
	probeReq := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
	probeRec := httptest.NewRecorder()
	handler.ServeHTTP(probeRec, probeReq)
	if probeRec.Code != http.StatusBadRequest {
		t.Fatalf("empty callback probe status = %d, want 400", probeRec.Code)
	}
	if len(resultCh) != 0 {
		t.Fatal("empty callback probe must not populate resultCh")
	}

	// 2. Stale/foreign OAuth callback with wrong state must return 400 and never occupy resultCh.
	wrongStateReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=stale-code&state=wrong-state", nil)
	wrongStateRec := httptest.NewRecorder()
	handler.ServeHTTP(wrongStateRec, wrongStateReq)
	if wrongStateRec.Code != http.StatusBadRequest {
		t.Fatalf("mismatched state callback status = %d, want 400", wrongStateRec.Code)
	}
	if len(resultCh) != 0 {
		t.Fatal("mismatched state callback must not populate resultCh")
	}

	// 3. Legitimate callback with matching state succeeds and delivers code.
	validReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=real-auth-code&state=expected-state-123", nil)
	validRec := httptest.NewRecorder()
	handler.ServeHTTP(validRec, validReq)
	if validRec.Code != http.StatusOK {
		t.Fatalf("valid callback status = %d, want 200", validRec.Code)
	}

	// 4. Duplicate browser retry must not block handler even while resultCh is full.
	dupReq := httptest.NewRequest(http.MethodGet, "/auth/callback?code=dup-code&state=expected-state-123", nil)
	dupRec := httptest.NewRecorder()
	handler.ServeHTTP(dupRec, dupReq)
	if dupRec.Code != http.StatusOK {
		t.Fatalf("duplicate callback status = %d, want 200", dupRec.Code)
	}

	got := <-resultCh
	if got.Code != "real-auth-code" || got.State != "expected-state-123" || got.Error != "" {
		t.Fatalf("callback result = %+v, want Code=real-auth-code State=expected-state-123", got)
	}
}

func TestAntigravityCallbackHandlerDeliversStatelessOAuthError(t *testing.T) {
	resultCh := make(chan callbackResult, 1)
	handler := newAntigravityCallbackHandler("/auth/callback", "expected-state-123", resultCh)

	// Foreign error redirect carrying a DIFFERENT non-empty state must still be rejected.
	foreignErrReq := httptest.NewRequest(http.MethodGet, "/auth/callback?error=access_denied&state=other-tab-state", nil)
	foreignErrRec := httptest.NewRecorder()
	handler.ServeHTTP(foreignErrRec, foreignErrReq)
	if foreignErrRec.Code != http.StatusBadRequest || len(resultCh) != 0 {
		t.Fatalf("foreign error callback: code=%d len=%d, want 400 and empty channel", foreignErrRec.Code, len(resultCh))
	}

	// Provider error redirect omitting state (e.g. invalid_request) must notify Login immediately instead of hanging 5m.
	statelessErrReq := httptest.NewRequest(http.MethodGet, "/auth/callback?error=invalid_request", nil)
	statelessErrRec := httptest.NewRecorder()
	handler.ServeHTTP(statelessErrRec, statelessErrReq)
	if statelessErrRec.Code != http.StatusOK {
		t.Fatalf("stateless error callback status = %d, want 200", statelessErrRec.Code)
	}
	got := <-resultCh
	if got.Error != "invalid_request" || got.Code != "" {
		t.Fatalf("stateless error callback result = %+v, want Error=invalid_request", got)
	}
}
