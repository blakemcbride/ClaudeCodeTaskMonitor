package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGuard(t *testing.T) {
	h := guard("127.0.0.1:7433", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	cases := []struct {
		name, method, host, origin string
		want                       int
	}{
		{"own page", "GET", "127.0.0.1:7433", "", 204},
		{"localhost name", "GET", "localhost:7433", "http://localhost:7433", 204},
		{"dns rebinding", "GET", "evil.example:7433", "", 403},
		{"wrong port", "GET", "127.0.0.1:9999", "", 403},
		{"another site's script", "GET", "127.0.0.1:7433", "https://evil.example", 403},
		{"write attempt", "POST", "127.0.0.1:7433", "", 405},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/api/snapshot", nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, w.Code, c.want)
		}
	}
}
