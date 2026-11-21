package usage

import "testing"

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
