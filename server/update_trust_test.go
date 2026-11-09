package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gondar/selfupdate"
)

func TestUpdateRequiresConfiguredTrustBeforeContactingReleaseService(t *testing.T) {
	previousRepo, previousKey, previousCache := selfupdate.Repo, selfupdate.ReleasePublicKey, relCache
	t.Cleanup(func() {
		selfupdate.Repo, selfupdate.ReleasePublicKey, relCache = previousRepo, previousKey, previousCache
	})
	selfupdate.Repo, selfupdate.ReleasePublicKey = "fixture/gondar", ""
	requests := 0
	relCache = &releaseCache{fetch: func(context.Context, *http.Client) (*selfupdate.Release, error) {
		requests++
		return &selfupdate.Release{TagName: "v999.0.0"}, nil
	}}
	s := &Server{}
	check := httptest.NewRecorder()
	s.updateCheck(check, httptest.NewRequest(http.MethodGet, "/api/update/check", nil))
	var body struct {
		HasUpdate    bool                          `json:"has_update"`
		Verification selfupdate.VerificationPolicy `json:"verification"`
		Error        string                        `json:"error"`
	}
	if err := json.Unmarshal(check.Body.Bytes(), &body); err != nil || check.Code != http.StatusOK || body.HasUpdate || body.Verification.Ready || body.Error == "" {
		t.Fatalf("unconfigured check response=%s error=%v", check.Body.String(), err)
	}
	apply := httptest.NewRecorder()
	s.updateApply(apply, httptest.NewRequest(http.MethodPost, "/api/update/apply", nil))
	if apply.Code != http.StatusForbidden || requests != 0 {
		t.Fatalf("untrusted update: status=%d release lookups=%d", apply.Code, requests)
	}
}
