package api

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestHealthz(t *testing.T) {
	s := New(testLogger(), Deps{})
	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rr.Code)
	}
}

func TestReadyzReflectsReadyFunc(t *testing.T) {
	s := New(testLogger(), Deps{})

	rr := httptest.NewRecorder()
	s.Router().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("readyz with nil Ready = %d, want 200", rr.Code)
	}

	s.Ready = func() error { return errors.New("db down") }
	rr = httptest.NewRecorder()
	s.Router().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz failing = %d, want 503", rr.Code)
	}
}
