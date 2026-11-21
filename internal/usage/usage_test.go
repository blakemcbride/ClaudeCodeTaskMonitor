package usage

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseLimitsList(t *testing.T) {
	info := Parse([]byte(`{
	  "oauthAccount": {"organizationRateLimitTier": "default_claude_max_20x", "emailAddress": "someone@example.com"},
	  "cachedUsageUtilization": {"fetchedAtMs": 1700000000000, "accountUuid": "x", "utilization": {
	    "five_hour": {"utilization": 31, "resets_at": "2026-01-02T15:00:00+00:00"},
	    "seven_day": {"utilization": 4, "resets_at": "2026-01-08T16:00:00+00:00"},
	    "limits": [
	      {"kind": "weekly_all", "group": "weekly", "percent": 4, "severity": "normal", "resets_at": "2026-01-08T16:00:00.5+00:00", "scope": null},
	      {"kind": "weekly_scoped", "group": "weekly", "percent": 7, "severity": "warning", "resets_at": "2026-01-08T16:00:00+00:00",
	       "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}}
	    ]}}}`))
	if info == nil || info.Plan != "max 20x" || info.FetchedAt != 1700000000000 {
		t.Fatalf("header wrong: %+v", info)
	}
	want := []struct {
		label string
		pct   float64
	}{{"5-hour session", 31}, {"Weekly, all models", 4}, {"Weekly, Fable", 7}}
	if len(info.Limits) != len(want) {
		t.Fatalf("limits: %+v", info.Limits)
	}
	for i, w := range want {
		if l := info.Limits[i]; l.Label != w.label || l.Percent != w.pct || l.ResetsAt == 0 {
			t.Errorf("limit %d = %+v, want %s %v", i, l, w.label, w.pct)
		}
	}
	if info.Limits[2].Severity != "warning" {
		t.Errorf("severity lost: %+v", info.Limits[2])
	}
}

func TestParseBucketsOnly(t *testing.T) {
	info := Parse([]byte(`{"cachedUsageUtilization": {"fetchedAtMs": 1, "utilization": {
	  "five_hour": null,
	  "seven_day": {"utilization": 12, "resets_at": "2026-01-08T16:00:00+00:00"},
	  "seven_day_opus": {"utilization": 40, "resets_at": ""}}}}`))
	if info == nil || len(info.Limits) != 2 || info.Limits[0].Label != "Weekly, all models" || info.Limits[1].Label != "Weekly, Opus" {
		t.Fatalf("%+v", info)
	}
}

func TestParseNothingUseful(t *testing.T) {
	for _, in := range []string{`{}`, `not json`, `{"cachedUsageUtilization": {"utilization": {"five_hour": null}}}`} {
		if info := Parse([]byte(in)); info != nil {
			t.Errorf("%s: got %+v, want nil", in, info)
		}
	}
}

func TestFetch(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Write([]byte(`{"five_hour": null, "limits": [
		  {"kind": "weekly_all", "group": "weekly", "percent": 13, "resets_at": "2026-01-08T16:00:00+00:00"}]}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	creds := func(expires int64) {
		os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(fmt.Sprintf(
			`{"claudeAiOauth": {"accessToken": "made-up", "expiresAt": %d, "rateLimitTier": "default_claude_max_20x"}}`, expires)), 0o600)
	}
	// An older cached figure, which the fetched one must replace.
	os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(
		`{"cachedUsageUtilization": {"fetchedAtMs": 1, "utilization": {"seven_day": {"utilization": 4}}}}`), 0o600)
	r := NewReader(dir)
	r.URL = srv.URL

	creds(1) // expired: no request is made
	if err := r.Fetch(); err == nil || auth != "" {
		t.Fatalf("expired token was used: err=%v auth=%q", err, auth)
	}
	if info := r.Read(); info == nil || info.Limits[0].Percent != 4 {
		t.Fatalf("cached figures lost: %+v", info)
	}

	creds(time.Now().Add(time.Hour).UnixMilli())
	if err := r.Fetch(); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer made-up" {
		t.Errorf("auth = %q", auth)
	}
	info := r.Read()
	if info == nil || info.Plan != "max 20x" || len(info.Limits) != 1 || info.Limits[0].Percent != 13 || info.FetchedAt <= 1 {
		t.Fatalf("%+v", info)
	}

	r.NoFetch, auth = true, ""
	if r.Fetch(); auth != "" {
		t.Error("fetched with NoFetch set")
	}
}
