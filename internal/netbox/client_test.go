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
	if got := attempts.Load(); got != 1 {
		t.Errorf("expected 1 attempt (no retry for not found), got %d", got)
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
