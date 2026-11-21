// Package usage reports the plan usage figures: the same percentages Claude
// Code's /usage command shows. They come from the copy Claude Code caches on
// disk and, unless turned off, from asking Anthropic directly on a timer.
package usage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	info := &Info{FetchedAt: f.Cached.FetchedAtMs, Limits: limitsOf(f.Cached.Utilization)}
	if f.OAuthAccount != nil {
		info.Plan = planName(f.OAuthAccount.Tier)
	}
	if len(info.Limits) == 0 {
		return nil
	}
	return info
}

func planName(tier string) string {
	return words(strings.TrimPrefix(strings.TrimPrefix(tier, "default_"), "claude_"))
}

// limitsOf reads a utilization object, as cached by Claude Code or as returned
// by the usage endpoint.
func limitsOf(u map[string]json.RawMessage) []Limit {
	info := &Info{Limits: []Limit{}}
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
	return info.Limits
}

// Reader re-parses the settings file only when it has changed. It can also
// fetch the figures itself (see Fetch); Read returns whichever is newer.
type Reader struct {
	// URL is the usage endpoint. NoFetch turns fetching off.
	URL     string
	NoFetch bool

	mu      sync.Mutex
	paths   []string
	creds   string // Claude Code's credentials file
	path    string
	modTime time.Time
	size    int64
	info    *Info
	fetched *Info
}

// The endpoint Claude Code's own /usage command reads.
const usageURL = "https://api.anthropic.com/api/oauth/usage?skip_spend=1"

// Only these fields of the credentials file are decoded.
type credentialsFile struct {
	OAuth *struct {
		AccessToken string `json:"accessToken"`
		ExpiresAt   int64  `json:"expiresAt"` // ms since epoch
		Tier        string `json:"rateLimitTier"`
	} `json:"claudeAiOauth"`
}

var client = &http.Client{
	Timeout: 15 * time.Second,
	// The request carries the login token, so it goes nowhere else.
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Fetch asks Anthropic for the current figures, using the login token Claude
// Code keeps on disk. The token is only read: an expired one is left for
// Claude Code to renew, because renewing it here would replace the token the
// running sessions hold. On any failure the earlier figures stand.
func (r *Reader) Fetch() error {
	if r.NoFetch {
		return nil
	}
	data, err := os.ReadFile(r.creds)
	if err != nil {
		return err // no file: API-key billing, or macOS, where it is in the Keychain
	}
	var c credentialsFile
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	if c.OAuth == nil || c.OAuth.AccessToken == "" {
		return errors.New("no login token")
	}
	if c.OAuth.ExpiresAt > 0 && time.Now().UnixMilli() >= c.OAuth.ExpiresAt {
		return errors.New("login token expired")
	}
	req, err := http.NewRequest(http.MethodGet, r.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.OAuth.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("User-Agent", "ccmon")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("usage endpoint: %s", resp.Status)
	}
	var u map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&u); err != nil {
		return err
	}
	limits := limitsOf(u)
	if len(limits) == 0 {
		return errors.New("usage endpoint: no figures")
	}
	r.mu.Lock()
	r.fetched = &Info{Plan: planName(c.OAuth.Tier), FetchedAt: time.Now().UnixMilli(), Limits: limits}
	r.mu.Unlock()
	return nil
}

// Run fetches now and then on a timer until stop is closed.
func (r *Reader) Run(stop <-chan struct{}, every time.Duration) {
	r.Fetch()
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			r.Fetch()
		}
	}
}

// NewReader looks for .claude.json inside the Claude config directory (where
// it lives when CLAUDE_CONFIG_DIR is set) and then beside it (the default,
// ~/.claude.json next to ~/.claude).
func NewReader(configDir string) *Reader {
	return &Reader{URL: usageURL, creds: filepath.Join(configDir, ".credentials.json"), paths: []string{
		filepath.Join(configDir, ".claude.json"),
		filepath.Join(filepath.Dir(filepath.Clean(configDir)), ".claude.json"),
	}}
}

// Read returns the current figures, or nil when none are available.
func (r *Reader) Read() *Info {
	r.mu.Lock()
	defer r.mu.Unlock()
	cached := r.readFile()
	if r.fetched != nil && (cached == nil || r.fetched.FetchedAt > cached.FetchedAt) {
		return r.fetched
	}
	return cached
}

func (r *Reader) readFile() *Info {
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
