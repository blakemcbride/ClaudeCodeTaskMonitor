// Package usage reads the plan usage figures that Claude Code caches on disk:
// the same percentages its /usage command shows. Nothing is fetched from the
// network; the figures are as fresh as Claude Code last left them.
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Limit is one usage allowance, such as the weekly limit across all models.
type Limit struct {
	Label    string  `json:"label"`
	Percent  float64 `json:"percent"`            // of the allowance used
	ResetsAt int64   `json:"resetsAt,omitempty"` // ms since epoch
	Severity string  `json:"severity,omitempty"` // as Claude Code reported it
}

// Info is what the dashboard shows.
type Info struct {
	Plan      string  `json:"plan,omitempty"`
	FetchedAt int64   `json:"fetchedAt"` // when Claude Code last refreshed it
	Limits    []Limit `json:"limits"`
}

// Only these fields of the settings file are decoded. The file also holds
// account details, which are never read into memory as values.
type settingsFile struct {
	OAuthAccount *struct {
		Tier string `json:"organizationRateLimitTier"`
	} `json:"oauthAccount"`
	Cached *struct {
		FetchedAtMs int64                      `json:"fetchedAtMs"`
		Utilization map[string]json.RawMessage `json:"utilization"`
	} `json:"cachedUsageUtilization"`
}

type rawLimit struct {
	Kind     string  `json:"kind"`
	Group    string  `json:"group"`
	Percent  float64 `json:"percent"`
	Severity string  `json:"severity"`
	ResetsAt string  `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

type rawBucket struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    string   `json:"resets_at"`
}

// Older or simpler caches have only these named buckets.
var buckets = []struct{ key, label string }{
	{"five_hour", "5-hour session"},
	{"seven_day", "Weekly, all models"},
	{"seven_day_opus", "Weekly, Opus"},
	{"seven_day_sonnet", "Weekly, Sonnet"},
}

func parseTime(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func words(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "_", " ")) }

func label(l rawLimit) string {
	var base string
	switch {
	case strings.HasPrefix(l.Kind, "weekly") || l.Group == "weekly":
		base = "Weekly"
	case strings.Contains(l.Kind, "five_hour") || strings.Contains(l.Kind, "session") || l.Group == "session":
		base = "5-hour session"
	default:
		base = words(l.Kind)
		if base == "" {
			base = words(l.Group)
		}
		if base != "" {
			base = strings.ToUpper(base[:1]) + base[1:]
		}
	}
	if l.Scope != nil && l.Scope.Model != nil && l.Scope.Model.DisplayName != "" {
		return base + ", " + l.Scope.Model.DisplayName
	}
	if base == "Weekly" {
		return "Weekly, all models"
	}
	return base
}

// Parse extracts the usage figures from the settings file's contents. It
// returns nil when there are none.
func Parse(data []byte) *Info {
	var f settingsFile
	if json.Unmarshal(data, &f) != nil || f.Cached == nil {
		return nil
	}
	info := &Info{FetchedAt: f.Cached.FetchedAtMs, Limits: []Limit{}}
	if f.OAuthAccount != nil {
		info.Plan = words(strings.TrimPrefix(strings.TrimPrefix(f.OAuthAccount.Tier, "default_"), "claude_"))
	}
	u := f.Cached.Utilization
	var limits []rawLimit
	if raw, ok := u["limits"]; ok {
		json.Unmarshal(raw, &limits)
	}
	for _, l := range limits {
		if name := label(l); name != "" {
			info.Limits = append(info.Limits, Limit{Label: name, Percent: l.Percent, ResetsAt: parseTime(l.ResetsAt), Severity: l.Severity})
		}
	}
	hasSession := false
	for _, l := range info.Limits {
		if l.Label == "5-hour session" {
			hasSession = true
		}
	}
	for _, b := range buckets {
		if len(limits) > 0 && (b.key != "five_hour" || hasSession) {
			continue // the limits list already covers the weekly figures
		}
		var rb rawBucket
		if raw, ok := u[b.key]; !ok || json.Unmarshal(raw, &rb) != nil || rb.Utilization == nil {
			continue
		}
		l := Limit{Label: b.label, Percent: *rb.Utilization, ResetsAt: parseTime(rb.ResetsAt)}
		if b.key == "five_hour" {
			info.Limits = append([]Limit{l}, info.Limits...)
		} else {
			info.Limits = append(info.Limits, l)
		}
	}
	if len(info.Limits) == 0 {
		return nil
	}
	return info
}

// Reader re-parses the settings file only when it has changed.
type Reader struct {
	mu      sync.Mutex
	paths   []string
	path    string
	modTime time.Time
	size    int64
	info    *Info
}

// NewReader looks for .claude.json inside the Claude config directory (where
// it lives when CLAUDE_CONFIG_DIR is set) and then beside it (the default,
// ~/.claude.json next to ~/.claude).
func NewReader(configDir string) *Reader {
	return &Reader{paths: []string{
		filepath.Join(configDir, ".claude.json"),
		filepath.Join(filepath.Dir(filepath.Clean(configDir)), ".claude.json"),
	}}
}

// Read returns the current figures, or nil when none are available.
func (r *Reader) Read() *Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if p == r.path && fi.ModTime().Equal(r.modTime) && fi.Size() == r.size {
			return r.info
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		r.path, r.modTime, r.size = p, fi.ModTime(), fi.Size()
		// A file caught mid-write fails to parse; keep the last good figures.
		if info := Parse(data); info != nil || json.Valid(data) {
			r.info = info
		}
		return r.info
	}
	return nil
}
