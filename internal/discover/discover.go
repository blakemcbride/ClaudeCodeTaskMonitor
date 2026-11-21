// Package discover reads the session files Claude Code keeps for each
// running session. It never writes to the Claude config directory.
package discover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// Session is one <config>/sessions/<pid>.json file. Every field is optional:
// the format is undocumented and changes between Claude Code versions.
type Session struct {
	PID             int    `json:"pid"`
	SessionID       string `json:"sessionId"`
	Cwd             string `json:"cwd"`
	StartedAt       int64  `json:"startedAt"` // ms since epoch
	Version         string `json:"version"`
	Kind            string `json:"kind"`
	Entrypoint      string `json:"entrypoint"`
	Name            string `json:"name"`
	NameSource      string `json:"nameSource"`
	Status          string `json:"status"`
	WaitingFor      string `json:"waitingFor"`
	UpdatedAt       int64  `json:"updatedAt"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"`
}

// ConfigDir returns the Claude Code config directory.
func ConfigDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".claude"
	}
	return filepath.Join(home, ".claude")
}

// Scan returns every readable session file, sorted by pid. Files that are
// missing a pid or cannot be parsed are skipped.
func Scan(configDir string) []Session {
	files, _ := filepath.Glob(filepath.Join(configDir, "sessions", "*.json"))
	var out []Session
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s Session
		if json.Unmarshal(b, &s) != nil || s.PID <= 0 {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// TranscriptPath finds the transcript for a session id. The project folder
// name is derived from the working directory in a way that is not worth
// reproducing, so the file is found by its session id instead.
func TranscriptPath(configDir, sessionID string) string {
	if sessionID == "" {
		return ""
	}
	m, _ := filepath.Glob(filepath.Join(configDir, "projects", "*", sessionID+".jsonl"))
	if len(m) == 0 {
		return ""
	}
	return m[0]
}
