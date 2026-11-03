// Command fakecontabo serves the in-memory fake Contabo API from
// internal/contabo/contabotest over HTTP. The end-to-end test runs it inside
// the cluster, so CI never needs a real Contabo account.
//
// It is a test tool only: no TLS, fixed credentials, data lost on restart.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hayone1/external-dns-contabo-webhook/internal/contabo/contabotest"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	zones := flag.String("zones", "example.com", "comma separated zones to create at start")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fake := contabotest.NewHandler()
	for _, z := range strings.Split(*zones, ",") {
		if z = strings.ToLower(strings.TrimSpace(z)); z != "" {
			fake.AddZone(z)
			log.Info("zone created", "zone", z)
		}
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           logRequests(fake.Handler(), log),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Info("fake Contabo API listening", "addr", *addr, "clientID", contabotest.ClientID, "apiUser", contabotest.APIUser)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server failed", "error", err)
		stop()
		os.Exit(1) //nolint:gocritic // stop() already ran
	}
}

func logRequests(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &status{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path != "/healthz" {
			log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.code)
		}
	})
}

type status struct {
	http.ResponseWriter
	code int
}

func (s *status) WriteHeader(code int) {
	s.code = code
	s.ResponseWriter.WriteHeader(code)
}
