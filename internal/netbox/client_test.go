package netbox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// recorder is an httptest handler that records every request and answers
// from a per-path function.
type recorder struct {
	mu    sync.Mutex
	reqs  []*http.Request
	serve func(w http.ResponseWriter, r *http.Request)
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec.mu.Lock()
	rec.reqs = append(rec.reqs, r.Clone(r.Context()))
	rec.mu.Unlock()
	rec.serve(w, r)
}

func (rec *recorder) paths() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	out := make([]string, len(rec.reqs))
	for i, r := range rec.reqs {
		out[i] = r.URL.Path
	}
	return out
}

func newClient(t *testing.T, serve func(w http.ResponseWriter, r *http.Request)) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{serve: serve}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)
	return NewClient(srv.URL, "test-token", WithRetry(3, 0)), rec
}

func jsonOK(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

const (
	deviceWithRack = `{"count":1,"results":[{"id":1,"name":"node-1","rack":{"id":1,"name":"Rack-42"}}]}`
	empty          = `{"count":0,"results":[]}`
)

func TestLookupRack_Device(t *testing.T) {
	c, rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Token test-token" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.URL.Query().Get("name"); got != "node-1" {
			t.Errorf("name = %q", got)
		}
		if got := r.URL.Query().Get("limit"); got != "2" {
			t.Errorf("limit = %q, want 2 (ambiguity detection)", got)
		}
		jsonOK(w, deviceWithRack)
	})
	rack, err := c.LookupRack(context.Background(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if rack != "Rack-42" {
		t.Errorf("rack = %q", rack)
	}
	if p := rec.paths(); len(p) != 1 || p[0] != "/api/dcim/devices/" {
		t.Errorf("paths = %v", p)
	}
}

func TestLookupRack_TrailingSlashInBaseURL(t *testing.T) {
	rec := &recorder{serve: func(w http.ResponseWriter, _ *http.Request) { jsonOK(w, deviceWithRack) }}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	c := NewClient(srv.URL+"/", "tok")
	if _, err := c.LookupRack(context.Background(), "node-1"); err != nil {
		t.Fatal(err)
	}
	if p := rec.paths(); p[0] != "/api/dcim/devices/" {
		t.Errorf("path = %q, double slash not trimmed", p[0])
	}
}

func TestLookupRack_VMFallback(t *testing.T) {
	c, rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/dcim/devices/":
			jsonOK(w, empty)
		case "/api/virtualization/virtual-machines/":
			if r.URL.Query().Get("name") != "vm-1" {
				t.Errorf("vm name = %q", r.URL.Query().Get("name"))
			}
			jsonOK(w, `{"count":1,"results":[{"id":10,"name":"vm-1","device":{"id":49,"name":"hv-1"}}]}`)
		case "/api/dcim/devices/49/":
			jsonOK(w, `{"id":49,"name":"hv-1","rack":{"id":35,"name":"L130-B15"}}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	rack, err := c.LookupRack(context.Background(), "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	if rack != "L130-B15" {
		t.Errorf("rack = %q", rack)
	}
	want := []string{"/api/dcim/devices/", "/api/virtualization/virtual-machines/", "/api/dcim/devices/49/"}
	if got := rec.paths(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("paths = %v, want %v", got, want)
	}
}

func TestLookupRack_NoZone(t *testing.T) {
	cases := map[string]struct {
		devices, vms, host string
		wantRequests       int
	}{
		"device without rack": {devices: `{"count":1,"results":[{"id":1,"name":"n","rack":null}]}`, wantRequests: 1},
		"unknown everywhere":  {devices: empty, vms: empty, wantRequests: 2},
		"vm without host":     {devices: empty, vms: `{"count":1,"results":[{"id":10,"name":"n","device":null}]}`, wantRequests: 2},
		"vm host without rack": {
			devices: empty,
			vms:     `{"count":1,"results":[{"id":10,"name":"n","device":{"id":49,"name":"hv"}}]}`,
			host:    `{"id":49,"name":"hv","rack":null}`, wantRequests: 3,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/dcim/devices/":
					jsonOK(w, tc.devices)
				case "/api/virtualization/virtual-machines/":
					jsonOK(w, tc.vms)
				case "/api/dcim/devices/49/":
					jsonOK(w, tc.host)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			})
			_, err := c.LookupRack(context.Background(), "n")
			if !errors.Is(err, ErrNoZone) {
				t.Fatalf("err = %v, want ErrNoZone", err)
			}
			if got := len(rec.paths()); got != tc.wantRequests {
				t.Errorf("requests = %d, want %d (permanent miss must not retry)", got, tc.wantRequests)
			}
		})
	}
}

func TestLookupRack_Ambiguous(t *testing.T) {
	c, rec := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		jsonOK(w, `{"count":2,"results":[{"id":1,"name":"n","rack":{"id":1,"name":"A"}},{"id":2,"name":"n","rack":{"id":2,"name":"B"}}]}`)
	})
	_, err := c.LookupRack(context.Background(), "n")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("err = %v, want ErrAmbiguous", err)
	}
	if errors.Is(err, ErrNoZone) {
		t.Error("ambiguous must not read as a miss")
	}
	if got := len(rec.paths()); got != 1 {
		t.Errorf("requests = %d, want 1 (no VM fallback, no retry)", got)
	}
}

func TestLookupRack_Retries(t *testing.T) {
	cases := map[string]struct {
		status       int
		wantAttempts int
	}{
		"5xx exhausts retries": {status: http.StatusInternalServerError, wantAttempts: 4},
		"429 is retried":       {status: http.StatusTooManyRequests, wantAttempts: 4},
		"403 is permanent":     {status: http.StatusForbidden, wantAttempts: 1},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, rec := newClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status) })
			_, err := c.LookupRack(context.Background(), "n")
			if err == nil {
				t.Fatal("expected error")
			}
			if errors.Is(err, ErrNoZone) {
				t.Error("HTTP failure must not read as a miss")
			}
			if got := len(rec.paths()); got != tc.wantAttempts {
				t.Errorf("attempts = %d, want %d", got, tc.wantAttempts)
			}
		})
	}
}

func TestLookupRack_TransientThenSuccess(t *testing.T) {
	var n int
	var mu sync.Mutex
	c, rec := newClient(t, func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		n++
		attempt := n
		mu.Unlock()
		if attempt < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		jsonOK(w, deviceWithRack)
	})
	rack, err := c.LookupRack(context.Background(), "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if rack != "Rack-42" || len(rec.paths()) != 3 {
		t.Errorf("rack = %q after %d attempts", rack, len(rec.paths()))
	}
}

func TestLookupRack_InvalidJSONIsPermanent(t *testing.T) {
	c, rec := newClient(t, func(w http.ResponseWriter, _ *http.Request) { jsonOK(w, "not json") })
	if _, err := c.LookupRack(context.Background(), "n"); err == nil {
		t.Fatal("expected error")
	}
	if got := len(rec.paths()); got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
}

func TestLookupRack_CancelledDuringBackoff(t *testing.T) {
	rec := &recorder{serve: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) }}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	c := NewClient(srv.URL, "tok", WithRetry(3, time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := c.LookupRack(ctx, "n")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("cancellation did not interrupt the backoff")
	}
}

func TestPing(t *testing.T) {
	c, _ := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status/" {
			t.Errorf("path = %s", r.URL.Path)
		}
		jsonOK(w, `{"netbox-version":"4.2.0"}`)
	})
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	down, _ := newClient(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	if err := down.Ping(context.Background()); err == nil {
		t.Fatal("expected error from a 502")
	}
}
