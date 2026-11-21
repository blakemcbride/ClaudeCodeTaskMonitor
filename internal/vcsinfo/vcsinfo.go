// Package vcsinfo reports the version-control state of a working directory,
// for git and Subversion. Results are cached and refreshed in the background
// so callers never wait on either tool.
package vcsinfo

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Info describes a working directory's version-control state.
type Info struct {
	System string `json:"system"` // "git" or "svn"
	// Branch is the git branch, or the Subversion path within the
	// repository (for example "^/trunk").
	Branch   string `json:"branch"`
	Revision string `json:"revision,omitempty"` // Subversion only
	Upstream string `json:"upstream,omitempty"`
	Ahead    int    `json:"ahead"`
	Behind   int    `json:"behind"`
	Dirty    int    `json:"dirty"` // changed or untracked paths
}

type entry struct {
	info     *Info
	at       time.Time
	fetching bool
}

// Cache holds one entry per directory.
type Cache struct {
	mu  sync.Mutex
	m   map[string]*entry
	TTL time.Duration
}

func NewCache() *Cache { return &Cache{m: map[string]*entry{}, TTL: 30 * time.Second} }

// Get returns the cached state for dir (nil if unknown or not under version control)
// and starts a background refresh when the entry is stale.
func (c *Cache) Get(dir string) *Info {
	if dir == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[dir]
	if e == nil {
		e = &entry{}
		c.m[dir] = e
	}
	if !e.fetching && time.Since(e.at) > c.TTL {
		e.fetching = true
		go func() {
			info := Query(dir)
			c.mu.Lock()
			e.info, e.at, e.fetching = info, time.Now(), false
			c.mu.Unlock()
		}()
	}
	return e.info
}

// Query asks git, then Subversion. It returns nil when dir is under neither,
// or the tool is not installed.
func Query(dir string) *Info {
	if info := queryGit(dir); info != nil {
		return info
	}
	return querySVN(dir)
}

func run(dir string, env []string, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	return cmd.Output()
}

func queryGit(dir string) *Info {
	// GIT_OPTIONAL_LOCKS=0 stops "git status" from rewriting the index, so
	// this stays read-only and never contends with a session's own git use.
	out, err := run(dir, []string{"GIT_OPTIONAL_LOCKS=0"}, "git", "status", "--porcelain=v2", "--branch")
	if err != nil {
		return nil
	}
	return parseGit(out)
}

func parseGit(out []byte) *Info {
	info := &Info{System: "git"}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			info.Branch = strings.TrimPrefix(line, "# branch.head ")
		case strings.HasPrefix(line, "# branch.upstream "):
			info.Upstream = strings.TrimPrefix(line, "# branch.upstream ")
		case strings.HasPrefix(line, "# branch.ab "):
			for _, f := range strings.Fields(strings.TrimPrefix(line, "# branch.ab ")) {
				n, _ := strconv.Atoi(f[1:])
				if f[0] == '+' {
					info.Ahead = n
				} else if f[0] == '-' {
					info.Behind = n
				}
			}
		case line != "" && line[0] != '#':
			info.Dirty++
		}
	}
	return info
}

// querySVN reads the working copy only. It never contacts the repository
// server, so it cannot say whether the copy is out of date. XML output is
// used because the plain output changes with the user's language.
func querySVN(dir string) *Info {
	out, err := run(dir, nil, "svn", "info", "--xml", "--non-interactive")
	if err != nil {
		return nil
	}
	info := parseSVNInfo(out)
	if info == nil {
		return nil
	}
	if st, err := run(dir, nil, "svn", "status", "--xml", "--non-interactive"); err == nil {
		info.Dirty = parseSVNStatus(st)
	}
	return info
}

func parseSVNInfo(out []byte) *Info {
	var doc struct {
		Entry struct {
			Revision    string `xml:"revision,attr"`
			RelativeURL string `xml:"relative-url"`
			URL         string `xml:"url"`
		} `xml:"entry"`
	}
	if xml.Unmarshal(out, &doc) != nil || (doc.Entry.URL == "" && doc.Entry.RelativeURL == "") {
		return nil
	}
	info := &Info{System: "svn", Branch: doc.Entry.RelativeURL, Revision: doc.Entry.Revision}
	if info.Branch == "" {
		info.Branch = doc.Entry.URL
	}
	return info
}

// parseSVNStatus counts changed versioned paths. Unversioned files are left
// out: working copies often hold thousands of build outputs that were never
// added to svn:ignore.
func parseSVNStatus(out []byte) int {
	var doc struct {
		Targets []struct {
			Entries []struct {
				Status struct {
					Item  string `xml:"item,attr"`
					Props string `xml:"props,attr"`
				} `xml:"wc-status"`
			} `xml:"entry"`
		} `xml:"target"`
	}
	if xml.Unmarshal(out, &doc) != nil {
		return 0
	}
	n := 0
	for _, t := range doc.Targets {
		for _, e := range t.Entries {
			changed := true
			switch e.Status.Item {
			case "normal", "external", "ignored", "unversioned":
				changed = false
			}
			if changed || e.Status.Props == "modified" || e.Status.Props == "conflicted" {
				n++
			}
		}
	}
	return n
}
