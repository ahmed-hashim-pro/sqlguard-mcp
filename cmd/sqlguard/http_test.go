package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahmed-hashim-pro/sqlguard-mcp/internal/db"
)

func testDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Exec(context.Background(), "CREATE TABLE t (a INTEGER)"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return database
}

func TestProbes(t *testing.T) {
	database := testDB(t)
	mux := newHTTPMux(mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil), database)

	for _, path := range []string{"/healthz", "/readyz"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))

			if recorder.Code != http.StatusOK {
				t.Errorf("%s = %d, want 200", path, recorder.Code)
			}
		})
	}
}

// Liveness must not depend on the database. If it did, a volume blip would
// restart a healthy pod and turn a brief outage into a crash loop.
func TestLivenessSurvivesALostDatabase(t *testing.T) {
	database := testDB(t)
	mux := newHTTPMux(mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil), database)
	database.Close()

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if recorder.Code != http.StatusOK {
		t.Errorf("/healthz = %d after the database closed, want 200", recorder.Code)
	}
}

// Readiness must depend on it, so a pod that cannot serve is taken out of the
// Service's endpoints rather than being handed traffic it will fail.
func TestReadinessFailsWithoutADatabase(t *testing.T) {
	database := testDB(t)
	mux := newHTTPMux(mcp.NewServer(&mcp.Implementation{Name: "test", Version: "test"}, nil), database)
	database.Close()

	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Errorf("/readyz = %d after the database closed, want 503", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "database unreachable") {
		t.Errorf("body = %q, want it to say why", recorder.Body.String())
	}
}

func TestMCPEndpointSpeaksTheProtocol(t *testing.T) {
	database := testDB(t)
	server := mcp.NewServer(&mcp.Implementation{Name: "sqlguard", Version: "test"}, nil)
	httpServer := httptest.NewServer(newHTTPMux(server, database))
	t.Cleanup(httpServer.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "test"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: httpServer.URL + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("connect over HTTP: %v", err)
	}
	defer session.Close()

	if _, err := session.ListTools(ctx, nil); err != nil {
		t.Errorf("ListTools over HTTP: %v", err)
	}
}

func TestServeHTTPShutsDownOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		done <- serveHTTP(ctx, "127.0.0.1:0", http.NewServeMux())
	}()

	time.Sleep(50 * time.Millisecond)
	cancel() // stands in for the SIGTERM Kubernetes sends

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveHTTP returned %v, want a clean shutdown", err)
		}
	case <-time.After(20 * time.Second):
		t.Error("serveHTTP did not return after its context was cancelled")
	}
}
