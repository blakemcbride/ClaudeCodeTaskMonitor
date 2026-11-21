// Package server serves the dashboard page and a stream of snapshots to it.
// It listens on the loopback address only and refuses requests that did not
// come from its own page.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/blakemcbride/ClaudeCodeTaskMonitor/internal/monitor"
)

// Server pushes a snapshot to every connected page whenever it changes.
type Server struct {
	Collector *monitor.Collector
	Static    fs.FS // holds index.html
	Interval  time.Duration

	mu      sync.Mutex
	current []byte // latest snapshot as JSON
	compare []byte // the same without its timestamp, to detect change
	subs    map[chan []byte]struct{}
}

// Run collects on a timer until stop is closed.
func (s *Server) Run(stop <-chan struct{}) {
	s.tick()
	t := time.NewTicker(s.Interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			s.tick()
		}
	}
}

func (s *Server) tick() {
	snap := s.Collector.Collect()
	full, err := json.Marshal(snap)
	if err != nil {
		return
	}
	snap.Updated = 0
	cmp, _ := json.Marshal(snap)
	s.mu.Lock()
	defer s.mu.Unlock()
	if bytes.Equal(cmp, s.compare) {
		return
	}
	s.current, s.compare = full, cmp
	for ch := range s.subs {
		select { // a page that is not keeping up gets the next one instead
		case ch <- full:
		default:
		}
	}
}

func (s *Server) snapshot() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current
}

// Handler returns the HTTP handler. addr is the host:port being listened on.
func (s *Server) Handler(addr string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(s.snapshot())
	})
	mux.HandleFunc("/api/events", s.events)
	mux.HandleFunc("/api/reply", func(w http.ResponseWriter, r *http.Request) {
		pid, _ := strconv.Atoi(r.URL.Query().Get("pid"))
		text, ok := s.Collector.Reply(pid)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"reply": text})
	})
	mux.Handle("/", http.FileServer(http.FS(s.Static)))
	return guard(addr, mux)
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan []byte, 1)
	s.mu.Lock()
	if s.subs == nil {
		s.subs = map[chan []byte]struct{}{}
	}
	s.subs[ch] = struct{}{}
	first := s.current
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subs, ch)
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	send := func(event string, data []byte) {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		fl.Flush()
	}
	if first != nil {
		send("snapshot", first)
	}
	ping := time.NewTicker(10 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			send("snapshot", b)
		case <-ping.C:
			send("ping", []byte("{}"))
		}
	}
}

// guard keeps other web pages away from the session text. A page on another
// site can make the browser send requests here, so the Host header must name
// this server (which defeats DNS rebinding) and any Origin must be this
// server's own.
func guard(addr string, next http.Handler) http.Handler {
	_, port, _ := net.SplitHostPort(addr)
	allowed := map[string]bool{
		net.JoinHostPort("127.0.0.1", port): true,
		net.JoinHostPort("localhost", port): true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Host] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || !allowed[u.Host] {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data:; connect-src 'self'; base-uri 'none'; form-action 'none'")
		next.ServeHTTP(w, r)
	})
}
