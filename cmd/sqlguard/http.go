package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahmed-hashim-pro/sqlguard-mcp/internal/db"
)

// newHTTPMux wires the MCP endpoint alongside the two probes Kubernetes needs.
//
// The probes answer different questions and must not be the same handler:
// liveness asks "is this process wedged, should I restart it", readiness asks
// "can this pod serve traffic right now". Pointing liveness at the database
// would restart a healthy pod every time the volume hiccuped, which turns a
// brief outage into a crash loop.
func newHTTPMux(server *mcp.Server, database *db.DB) *http.ServeMux {
	mux := http.NewServeMux()

	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		nil,
	))

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		if err := database.Ping(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "database unreachable: %v\n", err)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ready")
	})

	return mux
}

// serveHTTP runs until the context is cancelled, then drains in-flight requests.
//
// Kubernetes sends SIGTERM and waits before SIGKILL, so a server that exits
// immediately drops whatever it was mid-way through. Shutdown gives those
// requests a bounded window to finish.
func serveHTTP(ctx context.Context, addr string, handler http.Handler) error {
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}
