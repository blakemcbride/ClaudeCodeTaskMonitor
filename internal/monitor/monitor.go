// Package monitor gathers everything about the live sessions and hands it to
// the model package. It is the only place that touches the disk on a timer.
package monitor

import (
	"sync"
	"time"

	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/discover"
	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/model"
	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/procinfo"
	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/transcript"
	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/usage"
	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/vcsinfo"
)

// Collector keeps the state that makes repeated collection cheap: transcript
// offsets, CPU samples and the git cache.
type Collector struct {
	ConfigDir string
	Options   model.Options
	// Sync makes git queries block, for the one-shot text output.
	Sync bool

	mu         sync.Mutex
	trackers   map[string]*transcript.Tracker // by session id
	sampler    *procinfo.Sampler
	usage      *usage.Reader
	git        *vcsinfo.Cache
	stats      map[int]procinfo.Stats
	host       procinfo.Host
	statsAt    time.Time
	StatsEvery time.Duration
}

func New(configDir string, opt model.Options) *Collector {
	return &Collector{
		ConfigDir: configDir, Options: opt,
		trackers:   map[string]*transcript.Tracker{},
		sampler:    procinfo.NewSampler(),
		usage:      usage.NewReader(configDir),
		git:        vcsinfo.NewCache(),
		stats:      map[int]procinfo.Stats{},
		StatsEvery: 5 * time.Second,
	}
}

// live returns the session files whose process is still running.
func (c *Collector) live() []discover.Session {
	var out []discover.Session
	for _, s := range discover.Scan(c.ConfigDir) {
		if procinfo.Alive(s.PID, s.StartedAt) {
			out = append(out, s)
		}
	}
	return out
}

func (c *Collector) tracker(sessionID string) *transcript.Tracker {
	if t := c.trackers[sessionID]; t != nil {
		return t
	}
	path := discover.TranscriptPath(c.ConfigDir, sessionID)
	if path == "" {
		return nil // not written yet; look again next time
	}
	t := transcript.NewTracker(path)
	c.trackers[sessionID] = t
	return t
}

// Collect reads what changed and returns a fresh snapshot.
func (c *Collector) Collect() model.Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	sessions := c.live()

	if now.Sub(c.statsAt) >= c.StatsEvery {
		pids := make([]int, len(sessions))
		for i, s := range sessions {
			pids[i] = s.PID
		}
		c.stats = c.sampler.Sample(pids)
		c.host = procinfo.HostMemory()
		c.statsAt = now
	}

	inUse := map[string]bool{}
	inputs := make([]model.Input, 0, len(sessions))
	for _, s := range sessions {
		in := model.Input{File: s}
		if st, ok := c.stats[s.PID]; ok {
			in.Stats = &st
		}
		if t := c.tracker(s.SessionID); t != nil {
			inUse[s.SessionID] = true
			t.Refresh()
			sum := t.Summary()
			in.Summary = &sum
			in.Activity = transcript.LatestActivity(t.Path())
		}
		if c.Sync {
			in.VCS = vcsinfo.Query(s.Cwd)
		} else {
			in.VCS = c.git.Get(s.Cwd)
		}
		inputs = append(inputs, in)
	}
	for id := range c.trackers {
		if !inUse[id] {
			delete(c.trackers, id)
		}
	}
	snap := model.Build(inputs, c.host, c.Options, now)
	snap.Usage = c.usage.Read()
	return snap
}

// TotalsPending reports whether any transcript is still being summed.
func (c *Collector) TotalsPending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.trackers {
		if !t.Summary().TotalsComplete {
			return true
		}
	}
	return false
}

// Reply returns the latest reply text of one session, for the "Show" link in
// private mode.
func (c *Collector) Reply(pid int) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.live() {
		if s.PID != pid {
			continue
		}
		if t := c.trackers[s.SessionID]; t != nil {
			return t.Summary().Reply, true
		}
	}
	return "", false
}
