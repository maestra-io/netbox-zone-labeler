package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProbe(t *testing.T) {
	ok := probe(func() bool { return true }, "down")
	bad := probe(func() bool { return false }, "down")

	rec := httptest.NewRecorder()
	ok(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Errorf("ok probe: %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	bad(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable || rec.Body.String() != "down\n" {
		t.Errorf("failing probe: %d %q", rec.Code, rec.Body.String())
	}
}
