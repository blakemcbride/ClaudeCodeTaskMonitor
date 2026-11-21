// Package procinfo answers questions about processes through gopsutil, so
// that this project has no OS-specific code of its own.
package procinfo

import (
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"
)

// startSlackMs allows for clock rounding between the process start time the
// OS reports and the start time Claude Code wrote into its session file.
const startSlackMs = 5000

// Alive reports whether pid is still the process that wrote a session file
// with the given startedAt (ms since epoch). A reused pid belongs to a
// process created after the old session started, so it fails the check.
func Alive(pid int, startedAt int64) bool {
	ok, err := process.PidExists(int32(pid))
	if err != nil || !ok {
		return false
	}
	if startedAt <= 0 {
		return true
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return false
	}
	created, err := p.CreateTime()
	if err != nil || created <= 0 {
		return true // cannot tell; prefer showing a session to hiding one
	}
	return created <= startedAt+startSlackMs
}

// Stats is the resource use of one process and all of its descendants.
type Stats struct {
	RSS      uint64  `json:"rss"`      // bytes
	CPU      float64 `json:"cpu"`      // percent of one core since last sample
	Children int     `json:"children"` // descendant process count
}

// Host is machine-wide memory.
type Host struct {
	MemTotal     uint64 `json:"memTotal"`
	MemAvailable uint64 `json:"memAvailable"`
}

// Sampler remembers CPU times between calls so it can report a rate.
type Sampler struct {
	mu   sync.Mutex
	prev map[int32]cpuSample
}

type cpuSample struct {
	secs float64
	at   time.Time
}

func NewSampler() *Sampler { return &Sampler{prev: map[int32]cpuSample{}} }

// Sample returns stats for each requested pid, walking the process table once.
func (s *Sampler) Sample(pids []int) map[int]Stats {
	out := map[int]Stats{}
	if len(pids) == 0 {
		return out
	}
	procs, err := process.Processes()
	if err != nil {
		return out
	}
	byPID := map[int32]*process.Process{}
	kids := map[int32][]int32{}
	for _, p := range procs {
		byPID[p.Pid] = p
		if pp, err := p.Ppid(); err == nil {
			kids[pp] = append(kids[pp], p.Pid)
		}
	}
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	next := map[int32]cpuSample{}
	for _, pid := range pids {
		root := int32(pid)
		if byPID[root] == nil {
			continue
		}
		var st Stats
		var secs float64
		seen := map[int32]bool{}
		stack := []int32{root}
		for len(stack) > 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[id] {
				continue
			}
			seen[id] = true
			p := byPID[id]
			if p == nil {
				continue
			}
			if mi, err := p.MemoryInfo(); err == nil && mi != nil {
				st.RSS += mi.RSS
			}
			if t, err := p.Times(); err == nil && t != nil {
				secs += t.User + t.System
			}
			if id != root {
				st.Children++
			}
			stack = append(stack, kids[id]...)
		}
		if old, ok := s.prev[root]; ok {
			if dt := now.Sub(old.at).Seconds(); dt > 0 && secs >= old.secs {
				st.CPU = (secs - old.secs) / dt * 100
			}
		}
		next[root] = cpuSample{secs, now}
		out[pid] = st
	}
	s.prev = next
	return out
}

// HostMemory returns machine-wide memory figures.
func HostMemory() Host {
	vm, err := mem.VirtualMemory()
	if err != nil || vm == nil {
		return Host{}
	}
	return Host{MemTotal: vm.Total, MemAvailable: vm.Available}
}
