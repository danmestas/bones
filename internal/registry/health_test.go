package registry

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestIsAlive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	t.Run("alive: PID + healthy HTTP", func(t *testing.T) {
		e := Entry{HubPID: os.Getpid(), HubURL: srv.URL}
		if !IsAlive(e) {
			t.Fatalf("expected alive")
		}
	})

	t.Run("dead: PID alive but HTTP wrong port", func(t *testing.T) {
		e := Entry{HubPID: os.Getpid(), HubURL: "http://127.0.0.1:1"}
		if IsAlive(e) {
			t.Fatalf("expected dead (HTTP fails)")
		}
	})

	t.Run("dead: PID gone", func(t *testing.T) {
		e := Entry{HubPID: 0, HubURL: srv.URL}
		if IsAlive(e) {
			t.Fatalf("expected dead (PID invalid)")
		}
	})
}

func TestServerURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	if !strings.HasPrefix(srv.URL, "http://127.0.0.1:") {
		t.Fatalf("unexpected URL: %s", srv.URL)
	}
}
