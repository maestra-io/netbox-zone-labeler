package netbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestGetDeviceRack_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/dcim/devices/" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("name") != "node-1" {
			t.Errorf("unexpected name param: %s", r.URL.Query().Get("name"))
		}
		if r.Header.Get("Authorization") != "Token test-token" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"count":1,"results":[{"id":1,"name":"node-1","rack":{"id":1,"name":"Rack-42"}}]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	rack, err := client.GetDeviceRack(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rack != "Rack-42" {
		t.Errorf("expected Rack-42, got %s", rack)
	}
}

func TestGetDeviceRack_NotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"count":0,"results":[]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	_, err := client.GetDeviceRack(context.Background(), "unknown")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsNotFound(err) {
		t.Errorf("expected not found error, got: %v", err)
	}
}

func TestGetDeviceRack_NoRack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"count":1,"results":[{"id":1,"name":"node-1","rack":null}]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	_, err := client.GetDeviceRack(context.Background(), "node-1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsNoRack(err) {
		t.Errorf("expected no rack error, got: %v", err)
	}
}

func TestGetDeviceRack_ServerErrorRetries(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	_, err := client.GetDeviceRack(context.Background(), "node-1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := attempts.Load(); got != int32(maxRetries+1) {
		t.Errorf("expected %d attempts (retries on 5xx), got %d", maxRetries+1, got)
	}
}

func TestGetDeviceRack_RetryOnTransientThenSuccess(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		if attempts.Load() < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"count":1,"results":[{"id":1,"name":"node-1","rack":{"id":1,"name":"Rack-1"}}]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	rack, err := client.GetDeviceRack(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rack != "Rack-1" {
		t.Errorf("expected Rack-1, got %s", rack)
	}
	if got := attempts.Load(); got != 3 {
		t.Errorf("expected 3 attempts, got %d", got)
	}
}

func TestGetDeviceRack_RetryOn429(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		if attempts.Load() < 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"count":1,"results":[{"id":1,"name":"node-1","rack":{"id":1,"name":"Rack-1"}}]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	rack, err := client.GetDeviceRack(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rack != "Rack-1" {
		t.Errorf("expected Rack-1, got %s", rack)
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("expected 2 attempts (retry on 429), got %d", got)
	}
}

func TestGetDeviceRack_NoRetryOn4xx(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	_, err := client.GetDeviceRack(context.Background(), "node-1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("expected 1 attempt (no retry for 403), got %d", got)
	}
}

func TestGetDeviceRack_NoRetryOnNotFound(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"count":0,"results":[]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	_, err := client.GetDeviceRack(context.Background(), "unknown")
	if !IsNotFound(err) {
		t.Fatalf("expected not found error, got: %v", err)
	}
	// 2 attempts: device lookup (not found) + VM fallback (not found), no retries
	if got := attempts.Load(); got != 2 {
		t.Errorf("expected 2 attempts (device + VM lookup, no retry), got %d", got)
	}
}

func TestGetDeviceRack_NoRetryOnNoRack(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"count":1,"results":[{"id":1,"name":"node-1","rack":null}]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	_, _ = client.GetDeviceRack(context.Background(), "node-1")
	if got := attempts.Load(); got != 1 {
		t.Errorf("expected 1 attempt (no retry for no rack), got %d", got)
	}
}

func TestGetDeviceRack_NoRetryOnInvalidJSON(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`not json`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	_, err := client.GetDeviceRack(context.Background(), "node-1")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if got := attempts.Load(); got != 1 {
		t.Errorf("expected 1 attempt (no retry for decode error), got %d", got)
	}
}

func TestGetRack_FallbackToVM(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/dcim/devices/" && r.URL.Query().Get("name") == "us-omic-lw-dc1":
			w.Write([]byte(`{"count":0,"results":[]}`))
		case r.URL.Path == "/api/virtualization/virtual-machines/" && r.URL.Query().Get("name") == "us-omic-lw-dc1":
			w.Write([]byte(`{"count":1,"results":[{"id":10,"name":"us-omic-lw-dc1","device":{"id":49,"name":"us-omicron-lw-proxmox-01"}}]}`))
		case r.URL.Path == "/api/dcim/devices/49/":
			w.Write([]byte(`{"id":49,"name":"us-omicron-lw-proxmox-01","rack":{"id":35,"name":"L130-B15"}}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	rack, err := client.GetDeviceRack(context.Background(), "us-omic-lw-dc1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rack != "L130-B15" {
		t.Errorf("expected L130-B15, got %s", rack)
	}

	expectedPaths := []string{
		"/api/dcim/devices/",
		"/api/virtualization/virtual-machines/",
		"/api/dcim/devices/49/",
	}
	if len(paths) != len(expectedPaths) {
		t.Fatalf("expected %d requests, got %d: %v", len(expectedPaths), len(paths), paths)
	}
	for i, p := range expectedPaths {
		if paths[i] != p {
			t.Errorf("request %d: expected path %s, got %s", i, p, paths[i])
		}
	}
}

func TestGetRack_VMNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"count":0,"results":[]}`))
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	_, err := client.GetDeviceRack(context.Background(), "unknown-vm")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsNotFound(err) {
		t.Errorf("expected not found error, got: %v", err)
	}
}

func TestGetRack_VMNoHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/dcim/devices/":
			w.Write([]byte(`{"count":0,"results":[]}`))
		case r.URL.Path == "/api/virtualization/virtual-machines/":
			w.Write([]byte(`{"count":1,"results":[{"id":10,"name":"orphan-vm","device":null}]}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	_, err := client.GetDeviceRack(context.Background(), "orphan-vm")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsNoHost(err) {
		t.Errorf("expected no host error, got: %v", err)
	}
}

func TestGetRack_VMHostNoRack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/dcim/devices/" && r.URL.Query().Get("name") != "":
			w.Write([]byte(`{"count":0,"results":[]}`))
		case r.URL.Path == "/api/virtualization/virtual-machines/":
			w.Write([]byte(`{"count":1,"results":[{"id":10,"name":"vm-no-rack","device":{"id":99,"name":"host-no-rack"}}]}`))
		case r.URL.Path == "/api/dcim/devices/99/":
			w.Write([]byte(`{"id":99,"name":"host-no-rack","rack":null}`))
		default:
			t.Errorf("unexpected request: %s", r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, "test-token")
	_, err := client.GetDeviceRack(context.Background(), "vm-no-rack")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !IsNoRack(err) {
		t.Errorf("expected no rack error, got: %v", err)
	}
}
