package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// All fixtures here are made up; no real transcript is used.

func user(ts, text string) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"promptSource":"typed","permissionMode":"default","message":{"role":"user","content":%q}}`, ts, text)
}

func assistantText(ts, id, text string, in, out int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":"claude-test-1","content":[{"type":"text","text":%q}],"usage":{"input_tokens":%d,"cache_read_input_tokens":1000,"cache_creation_input_tokens":300,"cache_creation":{"ephemeral_1h_input_tokens":200,"ephemeral_5m_input_tokens":100},"output_tokens":%d}}}`, ts, id, text, in, out)
}

func toolUse(ts, id, toolID, cmd string, in, out int) string {
	return fmt.Sprintf(`{"type":"assistant","timestamp":%q,"message":{"id":%q,"model":"claude-test-1","content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":%q,"description":"Run the tests"}}],"usage":{"input_tokens":%d,"cache_read_input_tokens":1000,"cache_creation_input_tokens":300,"output_tokens":%d}}}`, ts, id, toolID, cmd, in, out)
}

func toolResult(ts, toolID string, isErr bool) string {
	return fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"is_error":%v,"content":"x"}]}}`, ts, toolID, isErr)
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, l := range lines {
		if _, err := f.WriteString(l); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSummary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path,
		`{"type":"ai-title","aiTitle":"Fix the parser"}`+"\n",
		`{"type":"api-request","params":{"note":"ignored"}}`+"\n",
		`not json at all`+"\n",
		user("2026-01-02T10:00:00Z", "please fix it")+"\n",
		// One message written as two records: its usage must count once.
		assistantText("2026-01-02T10:00:05Z", "msg_1", "Looking at it now.", 10, 5)+"\n",
		toolUse("2026-01-02T10:00:06Z", "msg_1", "tool_a", "make test", 10, 50)+"\n",
	)
	tr := NewTracker(path)
	tr.Refresh()
	s := tr.Summary()
	if s.Title != "Fix the parser" || s.Reply != "Looking at it now." || s.Model != "claude-test-1" {
		t.Fatalf("basic fields wrong: %+v", s)
	}
	if s.PermissionMode != "default" || s.LastTyped.IsZero() {
		t.Fatalf("user fields wrong: mode=%q typed=%v", s.PermissionMode, s.LastTyped)
	}
	if s.ContextTokens != 1310 {
		t.Fatalf("context = %d, want 1310", s.ContextTokens)
	}
	if len(s.Pending) != 1 || s.Pending[0].Name != "Bash" || s.Pending[0].Desc != "Run the tests" {
		t.Fatalf("pending tool wrong: %+v", s.Pending)
	}
	var total Usage
	for _, models := range s.ByDay {
		for _, u := range models {
			total.add(u)
		}
	}
	// Second record of msg_1 has no 1h/5m split, so all 300 count as 5m.
	want := Usage{In: 10, CacheWrite5m: 300, CacheRead: 1000, Out: 50}
	if total != want {
		t.Fatalf("usage = %+v, want %+v", total, want)
	}

	// The same call fails three times in a row.
	write(t, path, toolResult("2026-01-02T10:00:07Z", "tool_a", true)+"\n")
	for i := 2; i <= 3; i++ {
		id := fmt.Sprintf("tool_%d", i)
		write(t, path,
			toolUse("2026-01-02T10:00:10Z", fmt.Sprintf("msg_%d", i), id, "make test", 10, 50)+"\n",
			toolResult("2026-01-02T10:00:11Z", id, true)+"\n")
	}
	tr.Refresh()
	s = tr.Summary()
	if s.RepeatFailures != 3 || len(s.ErrorTimes) != 3 || len(s.Pending) != 0 {
		t.Fatalf("repeat=%d errors=%d pending=%d", s.RepeatFailures, len(s.ErrorTimes), len(s.Pending))
	}

	// A different call succeeding clears the streak.
	write(t, path,
		toolUse("2026-01-02T10:01:00Z", "msg_9", "tool_9", "ls", 10, 5)+"\n",
		toolResult("2026-01-02T10:01:01Z", "tool_9", false)+"\n")
	tr.Refresh()
	if s = tr.Summary(); s.RepeatFailures != 0 {
		t.Fatalf("repeat = %d after a success", s.RepeatFailures)
	}
}

func TestPartialLineIsNotConsumed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	line := assistantText("2026-01-02T10:00:05Z", "msg_1", "Done.", 1, 1) + "\n"
	write(t, path, line[:40])
	tr := NewTracker(path)
	tr.Refresh()
	if s := tr.Summary(); s.Reply != "" {
		t.Fatalf("read half a line: %q", s.Reply)
	}
	write(t, path, line[40:])
	tr.Refresh()
	if s := tr.Summary(); s.Reply != "Done." {
		t.Fatalf("reply = %q after the line was completed", s.Reply)
	}
}

func TestInjectedTextIsNotTyping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, `{"type":"user","timestamp":"2026-01-02T10:00:00Z","message":{"content":"<command-name>/model</command-name>"}}`+"\n")
	tr := NewTracker(path)
	tr.Refresh()
	if s := tr.Summary(); !s.LastTyped.IsZero() {
		t.Fatal("a slash command counted as typing")
	}
}

func TestLargeFileReadsTailAndSumsTheRest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.jsonl")
	var b strings.Builder
	filler := `{"type":"attachment","data":"` + strings.Repeat("x", 4000) + `"}` + "\n"
	n := 0
	for b.Len() < tailBytes+tailBytes/2 {
		b.WriteString(filler)
		if n%50 == 0 {
			b.WriteString(assistantText("2026-01-02T10:00:05Z", fmt.Sprintf("old_%d", n), "old reply", 1, 1) + "\n")
		}
		n++
	}
	olderMsgs := strings.Count(b.String(), `"old reply"`)
	b.WriteString(assistantText("2026-01-02T11:00:00Z", "new_1", "newest reply", 1, 1) + "\n")
	write(t, path, b.String())

	tr := NewTracker(path)
	tr.Refresh()
	deadline := time.Now().Add(10 * time.Second)
	for !tr.Summary().TotalsComplete {
		if time.Now().After(deadline) {
			t.Fatal("older part was never summed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := tr.Summary()
	if s.Reply != "newest reply" {
		t.Fatalf("reply = %q", s.Reply)
	}
	var total Usage
	for _, models := range s.ByDay {
		for _, u := range models {
			total.add(u)
		}
	}
	if want := int64(olderMsgs + 1); total.Out != want {
		t.Fatalf("counted %d messages, want %d (each exactly once)", total.Out, want)
	}
}

func TestTruncatedFileStartsAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, assistantText("2026-01-02T10:00:05Z", "msg_1", "first", 1, 1)+"\n", assistantText("2026-01-02T10:00:06Z", "msg_2", "second", 1, 1)+"\n")
	tr := NewTracker(path)
	tr.Refresh()
	os.WriteFile(path, []byte(assistantText("2026-01-02T10:00:07Z", "msg_3", "fresh", 1, 1)+"\n"), 0o600)
	tr.Refresh()
	if s := tr.Summary(); s.Reply != "fresh" {
		t.Fatalf("reply = %q after the file was replaced", s.Reply)
	}
}
