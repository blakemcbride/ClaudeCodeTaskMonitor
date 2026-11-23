package model

import (
	"testing"
	"time"

	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/discover"
	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/procinfo"
	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/transcript"
)

var now = time.Date(2026, 1, 2, 12, 0, 0, 0, time.Local)

func file(pid int, cwd, status string) discover.Session {
	return discover.Session{PID: pid, Cwd: cwd, Name: "s", Status: status, StatusUpdatedAt: now.Add(-time.Hour).UnixMilli()}
}

func TestLookupPrefersLongestPrefix(t *testing.T) {
	models := map[string]ModelInfo{"claude-a": {Input: 1}, "claude-a-2": {Input: 2}}
	if m, ok := Lookup(models, "claude-a-2-20260101"); !ok || m.Input != 2 {
		t.Fatalf("got %+v %v", m, ok)
	}
	if _, ok := Lookup(models, "other"); ok {
		t.Fatal("matched an unknown model")
	}
}

func TestWaitingShowsWhatIsAsked(t *testing.T) {
	f := file(1, "/p", "waiting")
	f.WaitingFor = "input needed"
	sum := &transcript.Summary{Pending: []transcript.ToolCall{{Name: "AskUserQuestion", Desc: "Which license?"}}}
	s := BuildSession(Input{File: f, Summary: sum}, DefaultOptions(), now)
	if s.State != Waiting || s.WaitingFor != "input needed" || s.Asking != "AskUserQuestion: Which license?" {
		t.Fatalf("%+v", s)
	}
}

func TestContextAndCost(t *testing.T) {
	opt := DefaultOptions()
	opt.Models = map[string]ModelInfo{"claude-test": {Input: 10, Output: 20, CacheRead: 1, Context: 1000}}
	sum := &transcript.Summary{
		Model: "claude-test-1", ContextTokens: 900, TotalsComplete: true,
		ByDay: map[string]map[string]transcript.Usage{
			"2026-01-02": {"claude-test-1": {In: 1_000_000, Out: 500_000}},
			"2026-01-01": {"claude-test-1": {CacheRead: 2_000_000}},
		},
	}
	s := BuildSession(Input{File: file(1, "/p", "idle"), Summary: sum}, opt, now)
	if s.ContextPct != 90 || !s.ContextNear || s.ContextLimit != 1000 {
		t.Fatalf("context: %+v", s)
	}
	if s.Cost != 22 || s.CostToday != 20 || !s.CostKnown {
		t.Fatalf("cost=%v today=%v known=%v", s.Cost, s.CostToday, s.CostKnown)
	}
	sum.Model = "mystery"
	sum.ByDay["2026-01-02"]["mystery"] = transcript.Usage{In: 5}
	s = BuildSession(Input{File: file(1, "/p", "idle"), Summary: sum}, opt, now)
	if s.CostKnown || s.ContextLimit != 0 {
		t.Fatal("an unpriced model must not produce a confident cost or a context percentage")
	}
}

func TestStuck(t *testing.T) {
	opt := DefaultOptions()
	quiet := Input{File: file(1, "/p", "busy"), Summary: &transcript.Summary{}, Activity: now.Add(-20 * time.Minute)}
	if s := BuildSession(quiet, opt, now); !s.Stuck {
		t.Fatal("busy and silent for 20 minutes should be flagged")
	}
	quiet.Activity = now.Add(-time.Minute)
	if s := BuildSession(quiet, opt, now); s.Stuck {
		t.Fatal("recent activity should not be flagged")
	}
	quiet.Summary.RepeatFailures = 3
	if s := BuildSession(quiet, opt, now); !s.Stuck {
		t.Fatal("three identical failures should be flagged")
	}
	idle := Input{File: file(1, "/p", "idle"), Summary: &transcript.Summary{RepeatFailures: 9}, Activity: now.Add(-time.Hour)}
	if s := BuildSession(idle, opt, now); s.Stuck {
		t.Fatal("only a busy session can be stuck")
	}
}

func TestBackgroundShell(t *testing.T) {
	in := Input{File: file(1, "/p", "shell"), Summary: &transcript.Summary{}, Activity: now.Add(-time.Hour)}
	s := BuildSession(in, DefaultOptions(), now)
	if s.State != Busy || !s.Background || s.Stuck || s.RawStatus != "" {
		t.Fatalf("a background command is working, not unknown or stuck: %+v", s)
	}
	s = BuildSession(Input{File: file(1, "/p", "napping")}, DefaultOptions(), now)
	if s.State != Unknown || s.RawStatus != "napping" {
		t.Fatalf("an unrecognised status should be kept as written: %+v", s)
	}
}

func TestRemoteControl(t *testing.T) {
	on := file(1, "/p", "idle")
	on.BridgeSessionID = "session_abc"
	snap := Build([]Input{{File: on}, {File: file(2, "/p", "idle")}}, procinfo.Host{}, DefaultOptions(), now)
	if snap.Counts.Remote != 1 || !snap.Projects[0].Sessions[0].Remote || snap.Projects[0].Sessions[1].Remote {
		t.Fatalf("%+v", snap.Counts)
	}
}

func TestPrivateWithholdsReply(t *testing.T) {
	opt := DefaultOptions()
	opt.Private = true
	s := BuildSession(Input{File: file(1, "/p", "idle"), Summary: &transcript.Summary{Reply: "secret"}}, opt, now)
	if s.Reply != "" || !s.ReplyHidden {
		t.Fatalf("%+v", s)
	}
}

func TestBuildGroupsAndSortsByUrgency(t *testing.T) {
	inputs := []Input{
		{File: file(1, "/work/alpha", "idle")},
		{File: file(2, "/work/beta", "busy")},
		{File: file(3, "/work/gamma", "idle")},
		{File: file(4, "/work/gamma", "waiting")},
	}
	snap := Build(inputs, procinfo.Host{}, DefaultOptions(), now)
	if len(snap.Projects) != 3 || snap.Projects[0].Name != "gamma" || snap.Projects[1].Name != "beta" {
		t.Fatalf("order: %v %v", snap.Projects[0].Name, snap.Projects[1].Name)
	}
	if g := snap.Projects[0]; g.State != Waiting || g.Sessions[0].PID != 4 {
		t.Fatalf("gamma: %+v", g)
	}
	if c := snap.Counts; c.Sessions != 4 || c.Waiting != 1 || c.Busy != 1 || c.Idle != 2 {
		t.Fatalf("counts: %+v", c)
	}
}
