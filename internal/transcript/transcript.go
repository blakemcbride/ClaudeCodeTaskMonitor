// Package transcript summarises a Claude Code session transcript (a JSONL
// file that only grows). Transcripts reach hundreds of megabytes, so a file is
// never held in memory: the first read takes only the tail, later reads take
// only what was appended, and the older part is summed once in the background.
package transcript

import (
	"bufio"
	"bytes"
	"encoding/json"
	"hash/fnv"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	tailBytes    = 4 << 20  // read this much of an existing file at first sight
	maxLineBytes = 32 << 20 // longer lines are skipped, not buffered
	maxReply     = 2000     // runes of reply text kept
	maxDesc      = 200      // runes of tool description kept
	maxErrors    = 100      // tool error timestamps kept
)

// Usage is a token count, split the way prices are split.
type Usage struct {
	In           int64 `json:"in"`
	CacheWrite5m int64 `json:"cacheWrite5m"`
	CacheWrite1h int64 `json:"cacheWrite1h"`
	CacheRead    int64 `json:"cacheRead"`
	Out          int64 `json:"out"`
}

func (u *Usage) add(o Usage) {
	u.In += o.In
	u.CacheWrite5m += o.CacheWrite5m
	u.CacheWrite1h += o.CacheWrite1h
	u.CacheRead += o.CacheRead
	u.Out += o.Out
}

// Total is every token in the count.
func (u Usage) Total() int64 {
	return u.In + u.CacheWrite5m + u.CacheWrite1h + u.CacheRead + u.Out
}

// ToolCall is one tool use requested by the model.
type ToolCall struct {
	Name string
	Desc string
	At   time.Time
	sig  uint64
}

// Summary is what the dashboard shows for one transcript.
type Summary struct {
	Path           string
	Size           int64
	Title          string
	Reply          string
	ReplyAt        time.Time
	LastTool       *ToolCall
	Pending        []ToolCall // tool calls with no result yet, oldest first
	Model          string
	ContextTokens  int64
	PermissionMode string
	LastTyped      time.Time
	LastRecord     time.Time
	ErrorTimes     []time.Time
	// RepeatFailures counts how many of the most recent tool results were
	// the same call failing again.
	RepeatFailures int
	// ByDay is usage per local calendar day ("2006-01-02") and model.
	ByDay map[string]map[string]Usage
	// TotalsComplete is false while the older part of a large transcript is
	// still being summed.
	TotalsComplete bool
}

// state is the running summary of a stream of transcript lines.
type state struct {
	title, reply, model, permMode  string
	replyAt, lastTyped, lastRecord time.Time
	lastTool                       *ToolCall
	pending                        map[string]ToolCall
	pendingOrder                   []string
	contextTokens                  int64
	errorTimes                     []time.Time
	repeat                         int
	lastFailSig                    uint64
	byDay                          map[string]map[string]Usage
	// The usage of the message being streamed. Claude Code writes one
	// record per content block, each repeating the message's usage, so it
	// is counted once, when the next message begins.
	curID, curModel, curDay string
	curUsage                Usage
}

func newState() *state {
	return &state{pending: map[string]ToolCall{}, byDay: map[string]map[string]Usage{}}
}

type rawUsage struct {
	Input         int64 `json:"input_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
	Output        int64 `json:"output_tokens"`
	Split         *struct {
		H1 int64 `json:"ephemeral_1h_input_tokens"`
		M5 int64 `json:"ephemeral_5m_input_tokens"`
	} `json:"cache_creation"`
}

type record struct {
	Type           string `json:"type"`
	Timestamp      string `json:"timestamp"`
	IsSidechain    bool   `json:"isSidechain"`
	IsMeta         bool   `json:"isMeta"`
	PermissionMode string `json:"permissionMode"`
	PromptSource   string `json:"promptSource"`
	AiTitle        string `json:"aiTitle"`
	Message        *struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *rawUsage       `json:"usage"`
	} `json:"message"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
}

var wantedMarks = [][]byte{
	[]byte(`"type":"assistant"`),
	[]byte(`"type":"user"`),
	[]byte(`"type":"ai-title"`),
	[]byte(`"type":"permission-mode"`),
}

// wanted is a cheap test that lets most lines skip JSON decoding.
func wanted(line []byte) bool {
	for _, m := range wantedMarks {
		if bytes.Contains(line, m) {
			return true
		}
	}
	return false
}

func (s *state) apply(line []byte) {
	if !wanted(line) {
		return
	}
	var r record
	if json.Unmarshal(line, &r) != nil {
		return
	}
	var at time.Time
	if r.Timestamp != "" {
		at, _ = time.Parse(time.RFC3339Nano, r.Timestamp)
	}
	switch r.Type {
	case "ai-title":
		if r.AiTitle != "" {
			s.title = r.AiTitle
		}
	case "permission-mode":
		if r.PermissionMode != "" {
			s.permMode = r.PermissionMode
		}
	case "user":
		if r.IsSidechain || r.Message == nil {
			return
		}
		s.touch(at)
		if r.PermissionMode != "" {
			s.permMode = r.PermissionMode
		}
		s.applyUser(&r, at)
	case "assistant":
		if r.IsSidechain || r.Message == nil || r.Message.Model == "<synthetic>" {
			return
		}
		s.touch(at)
		s.applyAssistant(&r, at)
	}
}

func (s *state) touch(at time.Time) {
	if at.After(s.lastRecord) {
		s.lastRecord = at
	}
}

func (s *state) applyUser(r *record, at time.Time) {
	c := bytes.TrimSpace(r.Message.Content)
	if len(c) == 0 {
		return
	}
	if c[0] == '"' {
		var text string
		if json.Unmarshal(c, &text) != nil {
			return
		}
		// Commands and injected notices are wrapped in tags; a person's
		// own words are not.
		if !r.IsMeta && (r.PromptSource != "" || !strings.HasPrefix(strings.TrimSpace(text), "<")) {
			s.lastTyped = at
			s.repeat = 0
		}
		return
	}
	var blocks []block
	if json.Unmarshal(c, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		call, known := s.pending[b.ToolUseID]
		s.dropPending(b.ToolUseID)
		if !b.IsError {
			s.repeat = 0
			continue
		}
		s.errorTimes = append(s.errorTimes, at)
		if len(s.errorTimes) > maxErrors {
			s.errorTimes = s.errorTimes[len(s.errorTimes)-maxErrors:]
		}
		if known && s.repeat > 0 && call.sig == s.lastFailSig {
			s.repeat++
		} else {
			s.repeat = 1
		}
		s.lastFailSig = call.sig
	}
}

func (s *state) applyAssistant(r *record, at time.Time) {
	m := r.Message
	if m.Model != "" {
		s.model = m.Model
	}
	if u := m.Usage; u != nil {
		use := Usage{In: u.Input, CacheRead: u.CacheRead, Out: u.Output, CacheWrite5m: u.CacheCreation}
		if u.Split != nil {
			use.CacheWrite5m, use.CacheWrite1h = u.Split.M5, u.Split.H1
		}
		if ctx := u.Input + u.CacheRead + u.CacheCreation; ctx > 0 {
			s.contextTokens = ctx
		}
		if m.ID == "" || m.ID != s.curID {
			s.commit()
			s.curID, s.curModel, s.curDay = m.ID, m.Model, at.Local().Format("2006-01-02")
		}
		s.curUsage = use
	}
	var blocks []block
	if json.Unmarshal(m.Content, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				s.reply, s.replyAt = clip(t, maxReply), at
			}
		case "tool_use":
			call := ToolCall{Name: b.Name, Desc: describe(b.Name, b.Input), At: at, sig: signature(b.Name, b.Input)}
			s.lastTool = &call
			if b.ID != "" {
				if _, dup := s.pending[b.ID]; !dup {
					s.pendingOrder = append(s.pendingOrder, b.ID)
				}
				s.pending[b.ID] = call
			}
		}
	}
}

// commit moves the usage of the finished message into the totals.
func (s *state) commit() {
	if s.curUsage == (Usage{}) {
		return
	}
	day := s.byDay[s.curDay]
	if day == nil {
		day = map[string]Usage{}
		s.byDay[s.curDay] = day
	}
	u := day[s.curModel]
	u.add(s.curUsage)
	day[s.curModel] = u
	s.curUsage = Usage{}
}

func (s *state) dropPending(id string) {
	if _, ok := s.pending[id]; !ok {
		return
	}
	delete(s.pending, id)
	for i, v := range s.pendingOrder {
		if v == id {
			s.pendingOrder = append(s.pendingOrder[:i], s.pendingOrder[i+1:]...)
			break
		}
	}
}

func signature(name string, input json.RawMessage) uint64 {
	h := fnv.New64a()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write(input)
	return h.Sum64()
}

// describe picks the most telling field of a tool's input.
func describe(name string, input json.RawMessage) string {
	var in map[string]json.RawMessage
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	if q, ok := in["questions"]; ok {
		var qs []struct {
			Question string `json:"question"`
		}
		if json.Unmarshal(q, &qs) == nil && len(qs) > 0 && qs[0].Question != "" {
			return clip(qs[0].Question, maxDesc)
		}
	}
	for _, k := range []string{"description", "command", "file_path", "pattern", "query", "url", "skill", "prompt", "message"} {
		var v string
		if raw, ok := in[k]; ok && json.Unmarshal(raw, &v) == nil && strings.TrimSpace(v) != "" {
			return clip(strings.Join(strings.Fields(v), " "), maxDesc)
		}
	}
	return ""
}

func clip(s string, n int) string {
	s = strings.ToValidUTF8(s, "�")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

// scanLines calls fn for each complete line read from r and returns how many
// bytes those lines occupied. A trailing line with no newline is left for the
// next call, since the writer may still be in the middle of it.
func scanLines(r io.Reader, fn func([]byte)) int64 {
	br := bufio.NewReaderSize(r, 1<<20)
	var consumed, held int64
	var long []byte
	skipping := false
	for {
		part, err := br.ReadSlice('\n')
		held += int64(len(part))
		if err == bufio.ErrBufferFull {
			if !skipping {
				long = append(long, part...)
				if len(long) > maxLineBytes {
					skipping, long = true, nil
				}
			}
			continue
		}
		if err != nil {
			return consumed
		}
		consumed += held
		held = 0
		if skipping {
			skipping = false
			continue
		}
		if len(long) > 0 {
			long = append(long, part...)
			fn(long)
			long = long[:0]
			if cap(long) > 4<<20 {
				long = nil
			}
			continue
		}
		fn(part)
	}
}

// Tracker follows one transcript file.
type Tracker struct {
	mu       sync.Mutex
	path     string
	offset   int64
	size     int64
	opened   bool
	main     *state
	older    *state // summary of the part before the tail, once summed
	scanning bool
	gen      int // bumped when the file is replaced, to discard a stale scan
}

func NewTracker(path string) *Tracker { return &Tracker{path: path, main: newState()} }

// Path is the file being followed.
func (t *Tracker) Path() string { return t.path }

// Refresh reads whatever was appended since the last call.
func (t *Tracker) Refresh() {
	t.mu.Lock()
	defer t.mu.Unlock()
	fi, err := os.Stat(t.path)
	if err != nil {
		return
	}
	size := fi.Size()
	if size < t.offset { // replaced or truncated: start again
		t.offset, t.opened, t.main, t.older, t.scanning = 0, false, newState(), nil, false
		t.gen++
	}
	t.size = size
	if t.opened && size == t.offset {
		return
	}
	f, err := os.Open(t.path)
	if err != nil {
		return
	}
	defer f.Close()
	if !t.opened {
		t.opened = true
		if size > tailBytes {
			t.offset = lineStartAfter(f, size-tailBytes, size)
		}
		if t.offset > 0 {
			t.scanning = true
			go t.scanOlder(t.offset, t.gen)
		}
	}
	t.offset += scanLines(io.NewSectionReader(f, t.offset, size-t.offset), t.main.apply)
}

// lineStartAfter returns the offset just past the first newline at or after
// from, or end if there is none.
func lineStartAfter(f *os.File, from, end int64) int64 {
	buf := make([]byte, 64<<10)
	for pos := from; pos < end; {
		n, err := f.ReadAt(buf, pos)
		if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
			return pos + int64(i) + 1
		}
		if n == 0 || err != nil {
			break
		}
		pos += int64(n)
	}
	return end
}

// scanOlder sums the part of the file before the tail.
func (t *Tracker) scanOlder(limit int64, gen int) {
	st := newState()
	if f, err := os.Open(t.path); err == nil {
		scanLines(io.NewSectionReader(f, 0, limit), st.apply)
		f.Close()
	}
	st.commit()
	t.mu.Lock()
	defer t.mu.Unlock()
	if gen != t.gen {
		return
	}
	t.older, t.scanning = st, false
}

// Summary returns a copy of the current summary.
func (t *Tracker) Summary() Summary {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.main
	s := Summary{
		Path: t.path, Size: t.size,
		Title: m.title, Reply: m.reply, ReplyAt: m.replyAt, Model: m.model,
		ContextTokens: m.contextTokens, PermissionMode: m.permMode,
		LastTyped: m.lastTyped, LastRecord: m.lastRecord,
		RepeatFailures: m.repeat, TotalsComplete: !t.scanning,
		ErrorTimes: append([]time.Time(nil), m.errorTimes...),
		ByDay:      map[string]map[string]Usage{},
	}
	if m.lastTool != nil {
		c := *m.lastTool
		s.LastTool = &c
	}
	for _, id := range m.pendingOrder {
		s.Pending = append(s.Pending, m.pending[id])
	}
	merge := func(day, model string, u Usage) {
		if s.ByDay[day] == nil {
			s.ByDay[day] = map[string]Usage{}
		}
		cur := s.ByDay[day][model]
		cur.add(u)
		s.ByDay[day][model] = cur
	}
	for day, models := range m.byDay {
		for model, u := range models {
			merge(day, model, u)
		}
	}
	if m.curUsage != (Usage{}) {
		merge(m.curDay, m.curModel, m.curUsage)
	}
	if o := t.older; o != nil {
		for day, models := range o.byDay {
			for model, u := range models {
				merge(day, model, u)
			}
		}
		// The tail of a long transcript can lack some facts (for example
		// when it is one large pasted image); fall back to the older part.
		if s.Title == "" {
			s.Title = o.title
		}
		if s.PermissionMode == "" {
			s.PermissionMode = o.permMode
		}
		if s.Model == "" {
			s.Model = o.model
		}
		if s.ContextTokens == 0 {
			s.ContextTokens = o.contextTokens
		}
		if s.Reply == "" {
			s.Reply, s.ReplyAt = o.reply, o.replyAt
		}
		if s.LastTyped.IsZero() {
			s.LastTyped = o.lastTyped
		}
		if s.LastTool == nil && o.lastTool != nil {
			c := *o.lastTool
			s.LastTool = &c
		}
		if s.LastRecord.IsZero() {
			s.LastRecord = o.lastRecord
		}
	}
	return s
}

// LatestActivity is the most recent write to the transcript or to any of the
// session's subagent transcripts. A session whose subagents are working is
// not idle even though its own transcript is quiet.
func LatestActivity(path string) time.Time {
	var latest time.Time
	if fi, err := os.Stat(path); err == nil {
		latest = fi.ModTime()
	}
	dir := filepath.Join(strings.TrimSuffix(path, ".jsonl"), "subagents")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
	}
	return latest
}
