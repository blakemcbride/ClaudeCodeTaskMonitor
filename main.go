// ccmon shows every Claude Code session running on this machine: what each
// is doing and which ones are waiting on you. It reads Claude Code's own
// files and never writes to them.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/browser"

	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/discover"
	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/model"
	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/monitor"
	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/server"
)

//go:embed web/index.html
var webFiles embed.FS

var version = "dev"

// config is the optional settings file. Flags given on the command line win.
type config struct {
	Port               *int                       `json:"port"`
	Private            *bool                      `json:"private"`
	ShowCost           *bool                      `json:"showCost"`
	StuckMinutes       *float64                   `json:"stuckMinutes"`
	ContextWarnPercent *float64                   `json:"contextWarnPercent"`
	Models             map[string]model.ModelInfo `json:"models"`
}

func configPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "ccmon", "config.json")
}

func loadConfig(path string) (config, error) {
	var c config
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func main() {
	var (
		port      = flag.Int("port", 7433, "port to listen on (loopback only)")
		once      = flag.Bool("once", false, "print the current state and exit")
		asJSON    = flag.Bool("json", false, "with -once, print JSON")
		private   = flag.Bool("private", false, "hide reply text until asked for")
		noOpen    = flag.Bool("no-open", false, "do not open the browser")
		noCost    = flag.Bool("no-cost", false, "do not show cost estimates")
		noFetch   = flag.Bool("no-fetch", false, "do not fetch plan usage from Anthropic; show only what Claude Code cached")
		cfgFile   = flag.String("config", configPath(), "settings file")
		claudeDir = flag.String("claude-dir", discover.ConfigDir(), "Claude Code config directory")
		showVer   = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("ccmon", version)
		return
	}

	cfg, err := loadConfig(*cfgFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ccmon:", err)
		os.Exit(2)
	}
	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })

	opt := model.DefaultOptions()
	if len(cfg.Models) > 0 {
		merged := map[string]model.ModelInfo{}
		for k, v := range model.DefaultModels {
			merged[k] = v
		}
		for k, v := range cfg.Models {
			merged[k] = v
		}
		opt.Models = merged
	}
	if cfg.StuckMinutes != nil {
		opt.StuckAfter = time.Duration(*cfg.StuckMinutes * float64(time.Minute))
	}
	if cfg.ContextWarnPercent != nil {
		opt.ContextWarn = *cfg.ContextWarnPercent
	}
	if cfg.ShowCost != nil {
		opt.ShowCost = *cfg.ShowCost
	}
	if *noCost {
		opt.ShowCost = false
	}
	opt.Private = *private
	if cfg.Private != nil && !set["private"] {
		opt.Private = *cfg.Private
	}
	if cfg.Port != nil && !set["port"] {
		*port = *cfg.Port
	}

	col := monitor.New(*claudeDir, opt)

	if *once {
		runOnce(col, *asJSON)
		return
	}

	addr := net.JoinHostPort("127.0.0.1", fmt.Sprint(*port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ccmon: cannot listen on %s: %v\n(is ccmon already running? try -port)\n", addr, err)
		os.Exit(1)
	}
	static, _ := fs.Sub(webFiles, "web")
	srv := &server.Server{Collector: col, Static: static, Interval: time.Second}
	stop := make(chan struct{})
	go srv.Run(stop)
	if !*noFetch {
		go col.FetchUsage(stop, 5*time.Minute)
	}

	url := "http://" + addr + "/"
	fmt.Printf("ccmon: serving %s (pid %d; Ctrl-C or kill to stop)\n", url, os.Getpid())
	if !*noOpen {
		browser.Stdout, browser.Stderr = io.Discard, io.Discard
		if err := browser.OpenURL(url); err != nil {
			fmt.Println("ccmon: could not open a browser; open the address above yourself")
		}
	}
	// Keep running when the terminal that started ccmon is closed, so that
	// "ccmon &" behaves like a small service. Ctrl-C and kill still stop it.
	signal.Ignore(syscall.SIGHUP)
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		close(stop)
		ln.Close()
	}()
	hs := &http.Server{Handler: srv.Handler(addr), ReadHeaderTimeout: 5 * time.Second}
	if err := hs.Serve(ln); err != nil && !strings.Contains(err.Error(), "use of closed") {
		fmt.Fprintln(os.Stderr, "ccmon:", err)
		os.Exit(1)
	}
}

// runOnce prints one snapshot. It waits for large transcripts to be summed
// and takes two samples so that CPU use is a rate.
func runOnce(col *monitor.Collector, asJSON bool) {
	col.Sync = true
	col.StatsEvery = 0
	col.Collect()
	deadline := time.Now().Add(60 * time.Second)
	for {
		time.Sleep(300 * time.Millisecond)
		if !col.TotalsPending() || time.Now().After(deadline) {
			break
		}
	}
	snap := col.Collect()
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(snap)
		return
	}
	printText(os.Stdout, snap, time.Now())
}

func printText(w io.Writer, snap model.Snapshot, now time.Time) {
	c := snap.Counts
	fmt.Fprintf(w, "%d sessions · %d waiting on you · %d blocked · %d working · %d idle",
		c.Sessions, c.Waiting, c.Blocked, c.Busy, c.Idle)
	if c.Stuck > 0 {
		fmt.Fprintf(w, " · %d possibly stuck", c.Stuck)
	}
	if c.Remote > 0 {
		fmt.Fprintf(w, " · %d with Remote Control", c.Remote)
	}
	if snap.ShowCost && snap.CostToday > 0 {
		fmt.Fprintf(w, " · about $%.2f today", snap.CostToday)
	}
	fmt.Fprintln(w)
	if h := snap.Host; h.MemTotal > 0 {
		used := h.MemTotal - min(h.MemAvailable, h.MemTotal)
		var sessions uint64
		for _, p := range snap.Projects {
			for _, s := range p.Sessions {
				sessions += s.RSS
			}
		}
		fmt.Fprintf(w, "RAM: %.0f%% used · %s free of %s · Claude sessions use %s",
			float64(used)/float64(h.MemTotal)*100, size(h.MemAvailable), size(h.MemTotal), size(sessions))
		if h.SwapTotal > 0 {
			fmt.Fprintf(w, " · swap %s of %s", size(h.SwapUsed), size(h.SwapTotal))
		}
		fmt.Fprintln(w)
	}
	if u := snap.Usage; u != nil && len(u.Limits) > 0 {
		var parts []string
		for _, l := range u.Limits {
			part := fmt.Sprintf("%s %.0f%%", l.Label, l.Percent)
			if l.ResetsAt > 0 {
				part += " (resets " + time.UnixMilli(l.ResetsAt).Local().Format("Mon 3:04 PM") + ")"
			}
			parts = append(parts, part)
		}
		line := "Plan usage"
		if u.Plan != "" {
			line += " [" + u.Plan + "]"
		}
		fmt.Fprintf(w, "%s: %s · as of %s\n", line, strings.Join(parts, " · "), agoText(u.FetchedAt, now))
	}
	for _, p := range snap.Projects {
		fmt.Fprintf(w, "\n%s  %s", p.Name, p.Path)
		if g := p.VCS; g != nil && g.Branch != "" {
			fmt.Fprintf(w, "  [%s %s", g.System, g.Branch)
			switch {
			case g.System == "svn":
				if g.Revision != "" {
					fmt.Fprintf(w, ", r%s", g.Revision)
				}
			case g.Upstream == "":
				fmt.Fprint(w, ", no upstream")
			default:
				fmt.Fprintf(w, ", %d ahead", g.Ahead)
				if g.Behind > 0 {
					fmt.Fprintf(w, ", %d behind", g.Behind)
				}
			}
			if g.Dirty > 0 {
				fmt.Fprintf(w, ", %d uncommitted", g.Dirty)
			}
			fmt.Fprint(w, "]")
		}
		fmt.Fprintln(w)
		for _, s := range p.Sessions {
			label := map[string]string{
				model.Waiting: "WAITING ON YOU", model.Blocked: "BLOCKED",
				model.Busy: "working", model.Idle: "idle",
			}[s.State]
			if label == "" {
				label = s.State
			}
			if s.Stuck {
				label += " (stuck?)"
			}
			fmt.Fprintf(w, "  %-15s %s  pid %d  %s\n", label, s.Name, s.PID, age(s.Since, now))
			if s.Model != "" || s.ContextTokens > 0 {
				line := strings.TrimPrefix(s.Model, "claude-")
				if s.ContextLimit > 0 {
					line += fmt.Sprintf("  context %.0f%% (%s of %s)", s.ContextPct, tokens(s.ContextTokens), tokens(s.ContextLimit))
					if s.ContextNear {
						line += "  NEAR THE LIMIT"
					}
				} else if s.ContextTokens > 0 {
					line += "  context " + tokens(s.ContextTokens)
				}
				fmt.Fprintf(w, "      %s\n", strings.TrimSpace(line))
			}
			if s.State == model.Waiting {
				fmt.Fprintf(w, "      needs: %s\n", strings.TrimPrefix(s.WaitingFor+" — "+s.Asking, " — "))
			}
			if s.Stuck {
				fmt.Fprintf(w, "      stuck: %s\n", s.StuckReason)
			}
			if s.Reply != "" {
				fmt.Fprintf(w, "      %s\n", oneLine(s.Reply, 110))
			} else if s.ReplyHidden {
				fmt.Fprintln(w, "      (reply hidden: private mode)")
			}
			var d []string
			if s.LastTool != "" {
				t := "last tool " + age(s.LastToolAt, now) + ": " + s.LastTool
				if s.ToolRunning {
					t = "running: " + s.LastTool
				}
				d = append(d, t)
			}
			if s.TranscriptBytes > 0 {
				d = append(d, "transcript "+size(uint64(s.TranscriptBytes)))
			}
			if s.Remote {
				d = append(d, "Remote Control on")
			}
			if s.PermissionMode != "" {
				d = append(d, "mode "+s.PermissionMode)
			}
			if s.RSS > 0 {
				d = append(d, fmt.Sprintf("%s RAM, %.0f%% CPU", size(s.RSS), s.CPU))
			}
			if s.ToolErrors > 0 {
				word := "errors"
				if s.ToolErrors == 1 {
					word = "error"
				}
				d = append(d, fmt.Sprintf("%d tool %s recently", s.ToolErrors, word))
			}
			if snap.ShowCost && s.CostKnown && s.Cost > 0 {
				d = append(d, fmt.Sprintf("about $%.2f", s.Cost))
			}
			if len(d) > 0 {
				fmt.Fprintf(w, "      %s\n", strings.Join(d, " · "))
			}
		}
	}
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func agoText(msEpoch int64, now time.Time) string {
	if a := age(msEpoch, now); a != "just now" && a != "?" {
		return a + " ago"
	} else {
		return a
	}
}

func age(msEpoch int64, now time.Time) string {
	if msEpoch <= 0 {
		return "?"
	}
	d := now.Sub(time.UnixMilli(msEpoch))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f h", d.Hours())
	}
	return fmt.Sprintf("%d d", int(d.Hours()/24))
}

func size(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

func tokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return strings.TrimSuffix(fmt.Sprintf("%.1f", float64(n)/1e6), ".0") + "M"
	case n >= 1000:
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprint(n)
}
