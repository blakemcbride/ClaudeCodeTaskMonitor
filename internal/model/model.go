// Package model turns what was read from disk into the snapshot the page and
// the text output display.
package model

import (
	"path/filepath"
	"sort"
	"time"

	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/discover"
	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/procinfo"
	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/transcript"
	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/usage"
	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/vcsinfo"
)

// Session states, in the order they sort (most urgent first).
const (
	Waiting = "waiting" // stopped on a question or permission prompt
	Blocked = "blocked" // cannot continue for an outside reason
	Busy    = "busy"    // working
	Idle    = "idle"    // finished its turn; your move
	Unknown = "unknown"
)

func rank(state string) int {
	switch state {
	case Waiting:
		return 0
	case Blocked:
		return 1
	case Busy:
		return 2
	case Idle:
		return 3
	}
	return 4
}

// Options are the tunable rules.
type Options struct {
	Models         map[string]ModelInfo
	StuckAfter     time.Duration // busy with no transcript activity for this long
	RepeatFailures int           // the same call failing this many times in a row
	ErrorWindow    time.Duration // how far back "recent" tool errors reach
	ContextWarn    float64       // percent of the window that counts as near the limit
	Private        bool          // withhold reply text
	ShowCost       bool
}

func DefaultOptions() Options {
	return Options{
		Models:         DefaultModels,
		StuckAfter:     15 * time.Minute,
		RepeatFailures: 3,
		ErrorWindow:    15 * time.Minute,
		ContextWarn:    85,
		ShowCost:       true,
	}
}

// Session is one row of the dashboard. Times are milliseconds since the epoch
// (0 when unknown) so the page can show them as ages.
type Session struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	Name       string `json:"name"`
	Title      string `json:"title,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Version    string `json:"version,omitempty"`
	State      string `json:"state"`
	WaitingFor string `json:"waitingFor,omitempty"`
	Remote     bool   `json:"remote,omitempty"` // Remote Control is on
	Asking     string `json:"asking,omitempty"` // the pending tool call or question
	Since      int64  `json:"since"`            // when it entered State
	StartedAt  int64  `json:"startedAt"`

	Reply       string `json:"reply,omitempty"`
	ReplyHidden bool   `json:"replyHidden,omitempty"`
	ReplyAt     int64  `json:"replyAt,omitempty"`

	LastTool     string `json:"lastTool,omitempty"`
	LastToolDesc string `json:"lastToolDesc,omitempty"`
	LastToolAt   int64  `json:"lastToolAt,omitempty"`
	ToolRunning  bool   `json:"toolRunning,omitempty"`

	Model           string  `json:"model,omitempty"`
	ContextTokens   int64   `json:"contextTokens,omitempty"`
	ContextLimit    int64   `json:"contextLimit,omitempty"`
	ContextPct      float64 `json:"contextPct,omitempty"`
	ContextNear     bool    `json:"contextNear,omitempty"`
	TranscriptBytes int64   `json:"transcriptBytes,omitempty"`
	ToolErrors      int     `json:"toolErrors,omitempty"`
	PermissionMode  string  `json:"permissionMode,omitempty"`
	Unguarded       bool    `json:"unguarded,omitempty"` // permission prompts bypassed
	LastTyped       int64   `json:"lastTyped,omitempty"`

	Stuck       bool   `json:"stuck,omitempty"`
	StuckReason string `json:"stuckReason,omitempty"`

	RSS      uint64  `json:"rss,omitempty"`
	CPU      float64 `json:"cpu"`
	Children int     `json:"children,omitempty"`

	Tokens         int64   `json:"tokens,omitempty"`
	Cost           float64 `json:"cost,omitempty"`
	CostToday      float64 `json:"costToday,omitempty"`
	CostKnown      bool    `json:"costKnown,omitempty"`
	TotalsComplete bool    `json:"totalsComplete"`
}

// Project groups the sessions that share a working directory.
type Project struct {
	Name      string        `json:"name"`
	Path      string        `json:"path"`
	State     string        `json:"state"`
	VCS       *vcsinfo.Info `json:"vcs,omitempty"`
	LastTyped int64         `json:"lastTyped,omitempty"`
	Sessions  []Session     `json:"sessions"`
}

// Counts are the tiles at the top of the page.
type Counts struct {
	Sessions int `json:"sessions"`
	Waiting  int `json:"waiting"`
	Blocked  int `json:"blocked"`
	Busy     int `json:"busy"`
	Idle     int `json:"idle"`
	Stuck    int `json:"stuck"`
	Remote   int `json:"remote"` // sessions with Remote Control on
}

// Snapshot is everything shown at one moment.
type Snapshot struct {
	Updated   int64         `json:"updated"`
	Counts    Counts        `json:"counts"`
	Host      procinfo.Host `json:"host"`
	CostToday float64       `json:"costToday"`
	ShowCost  bool          `json:"showCost"`
	Private   bool          `json:"private"`
	Usage     *usage.Info   `json:"usage,omitempty"` // plan limits, when known
	Projects  []Project     `json:"projects"`
}

// Input is what was gathered for one live session.
type Input struct {
	File     discover.Session
	Summary  *transcript.Summary // nil when no transcript was found
	Activity time.Time           // latest transcript or subagent write
	Stats    *procinfo.Stats
	VCS      *vcsinfo.Info
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func stateOf(status string) string {
	switch status {
	case Waiting, Blocked, Busy, Idle:
		return status
	}
	return Unknown
}

// BuildSession applies the status rules to one session.
func BuildSession(in Input, opt Options, now time.Time) Session {
	f := in.File
	s := Session{
		PID: f.PID, SessionID: f.SessionID, Name: f.Name, Kind: f.Kind, Version: f.Version,
		State: stateOf(f.Status), Since: f.StatusUpdatedAt, StartedAt: f.StartedAt,
		TotalsComplete: true,
		Remote:         f.BridgeSessionID != "",
	}
	if s.Name == "" {
		s.Name = filepath.Base(f.Cwd)
	}
	if s.Since == 0 {
		s.Since = f.UpdatedAt
	}
	if s.State == Waiting {
		s.WaitingFor = f.WaitingFor
	}
	if in.Stats != nil {
		s.RSS, s.CPU, s.Children = in.Stats.RSS, in.Stats.CPU, in.Stats.Children
	}
	sum := in.Summary
	if sum == nil {
		return s
	}
	s.Title = sum.Title
	s.TranscriptBytes = sum.Size
	s.Model = sum.Model
	s.PermissionMode = sum.PermissionMode
	s.Unguarded = sum.PermissionMode == "bypassPermissions"
	s.LastTyped = ms(sum.LastTyped)
	s.ReplyAt = ms(sum.ReplyAt)
	s.TotalsComplete = sum.TotalsComplete
	if opt.Private {
		s.ReplyHidden = sum.Reply != ""
	} else {
		s.Reply = sum.Reply
	}
	if t := sum.LastTool; t != nil {
		s.LastTool, s.LastToolDesc, s.LastToolAt = t.Name, t.Desc, ms(t.At)
	}
	s.ToolRunning = s.State == Busy && len(sum.Pending) > 0
	if s.State == Waiting && len(sum.Pending) > 0 {
		p := sum.Pending[len(sum.Pending)-1]
		s.Asking = p.Name
		if p.Desc != "" {
			s.Asking += ": " + p.Desc
		}
	}
	for _, t := range sum.ErrorTimes {
		if now.Sub(t) <= opt.ErrorWindow {
			s.ToolErrors++
		}
	}

	info, known := Lookup(opt.Models, sum.Model)
	s.ContextTokens = sum.ContextTokens
	if known && info.Context > 0 && sum.ContextTokens > 0 {
		s.ContextLimit = info.Context
		s.ContextPct = float64(sum.ContextTokens) / float64(info.Context) * 100
		s.ContextNear = s.ContextPct >= opt.ContextWarn
	}

	today := now.Local().Format("2006-01-02")
	s.CostKnown = true
	for day, models := range sum.ByDay {
		for id, u := range models {
			s.Tokens += u.Total()
			mi, ok := Lookup(opt.Models, id)
			if !ok {
				s.CostKnown = false
				continue
			}
			c := mi.Cost(u)
			s.Cost += c
			if day == today {
				s.CostToday += c
			}
		}
	}

	if s.State == Busy {
		last := in.Activity
		if t := time.UnixMilli(f.StatusUpdatedAt); f.StatusUpdatedAt > 0 && t.After(last) {
			last = t
		}
		switch {
		case opt.RepeatFailures > 0 && sum.RepeatFailures >= opt.RepeatFailures:
			s.Stuck, s.StuckReason = true, "the same tool call keeps failing"
		case opt.StuckAfter > 0 && !last.IsZero() && now.Sub(last) >= opt.StuckAfter:
			s.Stuck, s.StuckReason = true, "busy with no transcript activity"
		}
	}
	return s
}

// Build assembles the snapshot: sessions grouped by directory, the most
// urgent first.
func Build(inputs []Input, host procinfo.Host, opt Options, now time.Time) Snapshot {
	snap := Snapshot{Updated: now.UnixMilli(), Host: host, ShowCost: opt.ShowCost, Private: opt.Private, Projects: []Project{}}
	byPath := map[string]*Project{}
	var order []string
	for _, in := range inputs {
		s := BuildSession(in, opt, now)
		snap.Counts.Sessions++
		switch s.State {
		case Waiting:
			snap.Counts.Waiting++
		case Blocked:
			snap.Counts.Blocked++
		case Busy:
			snap.Counts.Busy++
		case Idle:
			snap.Counts.Idle++
		}
		if s.Stuck {
			snap.Counts.Stuck++
		}
		if s.Remote {
			snap.Counts.Remote++
		}
		snap.CostToday += s.CostToday
		p := byPath[in.File.Cwd]
		if p == nil {
			p = &Project{Name: filepath.Base(in.File.Cwd), Path: in.File.Cwd, State: Unknown, VCS: in.VCS}
			byPath[in.File.Cwd] = p
			order = append(order, in.File.Cwd)
		}
		if rank(s.State) < rank(p.State) {
			p.State = s.State
		}
		if s.LastTyped > p.LastTyped {
			p.LastTyped = s.LastTyped
		}
		p.Sessions = append(p.Sessions, s)
	}
	for _, path := range order {
		p := byPath[path]
		sort.SliceStable(p.Sessions, func(i, j int) bool {
			a, b := p.Sessions[i], p.Sessions[j]
			if rank(a.State) != rank(b.State) {
				return rank(a.State) < rank(b.State)
			}
			return a.StartedAt < b.StartedAt
		})
		snap.Projects = append(snap.Projects, *p)
	}
	sort.SliceStable(snap.Projects, func(i, j int) bool {
		a, b := snap.Projects[i], snap.Projects[j]
		if rank(a.State) != rank(b.State) {
			return rank(a.State) < rank(b.State)
		}
		return a.Name < b.Name
	})
	return snap
}
