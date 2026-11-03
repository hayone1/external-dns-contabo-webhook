// Command webhook is an external-dns webhook provider for Contabo DNS.
//
// Usage:
//
//	webhook              run the webhook (the default)
//	webhook check        test the configuration and credentials, list zones, exit
//	webhook healthcheck  exit 0 if the local /healthz answers ok (for Docker HEALTHCHECK)
//	webhook version      print the version
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"sigs.k8s.io/external-dns/endpoint"

	"github.com/hayone1/external-dns-contabo-webhook/internal/config"
	"github.com/hayone1/external-dns-contabo-webhook/internal/contabo"
	"github.com/hayone1/external-dns-contabo-webhook/internal/metrics"
	"github.com/hayone1/external-dns-contabo-webhook/internal/provider"
	"github.com/hayone1/external-dns-contabo-webhook/internal/server"
)

// Set at build time with -ldflags "-X main.version=... -X main.commit=...".
var (
	version = "dev"
	commit  = "none"
)

func main() {
	cmd := ""
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "", "serve":
		os.Exit(serve())
	case "check":
		os.Exit(check())
	case "healthcheck":
		os.Exit(healthcheck())
	case "version", "--version", "-v":
		fmt.Printf("external-dns-contabo-webhook %s (commit %s)\n", version, commit)
	case "help", "--help", "-h":
		fmt.Println("usage: webhook [serve|check|healthcheck|version]")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\nusage: webhook [serve|check|healthcheck|version]\n", cmd)
		os.Exit(2)
	}
}

func serve() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		return 2
	}
	log := newLogger(cfg, os.Stderr)
	log.Info("starting external-dns-contabo-webhook", "version", version, "commit", commit,
		"dryRun", cfg.DryRun, "domainFilter", cfg.DomainFilter, "excludeDomains", cfg.ExcludeDomainFilter)
	if cfg.DryRun {
		log.Warn("DRY_RUN is on: changes are logged but never sent to Contabo")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	m := metrics.New(version)
	status := &server.Status{}
	status.SetHealthy(true)

	healthErr := make(chan error, 1)
	healthSrv := server.New(cfg.HealthAddress(), server.HealthHandler(status, m.Registry))
	go func() { healthErr <- server.Serve(ctx, healthSrv, nil) }()
	log.Info("health and metrics server listening", "address", cfg.HealthAddress())

	client, err := newClient(cfg, log, m)
	if err != nil {
		log.Error("cannot create Contabo client", "error", err)
		return 1
	}

	// Wait for working credentials. Network problems are retried while the
	// pod stays alive but not ready; wrong credentials stop the process with
	// a clear message.
	if err := waitForCredentials(ctx, client, log); err != nil {
		if errors.Is(err, contabo.ErrInvalidCredentials) {
			log.Error("Contabo rejected the credentials. Check CONTABO_CLIENT_ID, CONTABO_CLIENT_SECRET, "+
				"CONTABO_API_USER and CONTABO_API_PASSWORD (see https://my.contabo.com/api/details)", "error", err)
			return 1
		}
		log.Info("stopped before startup finished", "reason", err)
		return 0
	}
	log.Info("Contabo credentials accepted")

	p := provider.New(client, provider.Options{
		DomainFilter: domainFilter(cfg),
		DefaultTTL:   cfg.DefaultTTL,
		DryRun:       cfg.DryRun,
		Logger:       log,
		Metrics:      m,
	})

	started := make(chan struct{})
	webhookErr := make(chan error, 1)
	webhookSrv := server.New(cfg.WebhookAddress(), server.WebhookHandler(p, log))
	go func() { webhookErr <- server.Serve(ctx, webhookSrv, started) }()

	select {
	case <-started:
		status.SetReady(true)
		log.Info("webhook server listening", "address", cfg.WebhookAddress())
	case err := <-webhookErr:
		log.Error("webhook server failed to start", "address", cfg.WebhookAddress(), "error", err)
		return 1
	}

	code := 0
	select {
	case <-ctx.Done():
		log.Info("shutdown signal received, stopping")
	case err := <-webhookErr:
		log.Error("webhook server stopped", "error", err)
		code = 1
	case err := <-healthErr:
		log.Error("health server stopped", "error", err)
		code = 1
	}
	status.SetReady(false)
	status.SetHealthy(false)
	stop()
	// Give both servers time to finish in-flight requests.
	for _, ch := range []chan error{webhookErr, healthErr} {
		select {
		case err := <-ch:
			if err != nil {
				log.Warn("server did not shut down cleanly", "error", err)
			}
		case <-time.After(35 * time.Second):
		}
	}
	log.Info("stopped")
	return code
}

func waitForCredentials(ctx context.Context, client *contabo.Client, log *slog.Logger) error {
	wait := time.Second
	for {
		err := client.CheckCredentials(ctx)
		if err == nil || errors.Is(err, contabo.ErrInvalidCredentials) {
			return err
		}
		log.Warn("cannot reach Contabo yet, retrying", "error", err, "retryIn", wait)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
	}
}

// check validates the configuration and credentials, then prints the zones
// the webhook would manage. It is meant for people setting things up.
func check() int {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		return 2
	}
	log := newLogger(cfg, os.Stderr)
	client, err := newClient(cfg, log, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := client.CheckCredentials(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "login failed: %v\n", err)
		return 1
	}
	fmt.Println("login ok")

	zones, err := client.ListZones(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listing zones failed: %v\n", err)
		return 1
	}
	filter := domainFilter(cfg)
	managed := 0
	for _, z := range zones {
		mark := "skipped (domain filter)"
		if filter.Match(z.Name) || filter.MatchParent(z.Name) {
			mark = "managed"
			managed++
		}
		fmt.Printf("zone %-40s %s\n", z.Name, mark)
	}
	if len(zones) == 0 {
		fmt.Println("no DNS zones found in this Contabo account; create one at https://my.contabo.com/dns")
	}
	fmt.Printf("%d zone(s) found, %d managed\n", len(zones), managed)
	if managed == 0 {
		return 1
	}
	return 0
}

// healthcheck calls the local health endpoint. Distroless images have no
// curl or wget, so the binary checks itself.
func healthcheck() int {
	port := config.DefaultHealthPort
	if v, ok := os.LookupEnv("HEALTH_PORT"); ok {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port))+"/healthz", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "health check returned HTTP %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func newClient(cfg *config.Config, log *slog.Logger, m *metrics.Metrics) (*contabo.Client, error) {
	return contabo.New(contabo.Options{
		APIURL:  cfg.APIURL,
		AuthURL: cfg.AuthURL,
		Credentials: contabo.Credentials{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			APIUser:      cfg.APIUser,
			APIPassword:  cfg.APIPassword,
		},
		RequestTimeout: cfg.RequestTimeout,
		RateLimit:      cfg.RateLimit,
		MaxRetries:     cfg.MaxRetries,
		UserAgent:      "external-dns-contabo-webhook/" + version,
		Logger:         log,
		Metrics:        m,
	})
}

func domainFilter(cfg *config.Config) *endpoint.DomainFilter {
	if cfg.RegexDomainFilter != nil || cfg.RegexDomainExclude != nil {
		return endpoint.NewRegexDomainFilter(cfg.RegexDomainFilter, cfg.RegexDomainExclude)
	}
	return endpoint.NewDomainFilterWithExclusions(cfg.DomainFilter, cfg.ExcludeDomainFilter)
}

// newLogger sets up slog for our code and logrus for the external-dns
// handlers, with the same level and format.
func newLogger(cfg *config.Config, w io.Writer) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	opts := &slog.HandlerOptions{Level: level}

	var h slog.Handler = slog.NewTextHandler(w, opts)
	if cfg.LogFormat == "json" {
		h = slog.NewJSONHandler(w, opts)
		logrus.SetFormatter(&logrus.JSONFormatter{})
	}
	logrus.SetOutput(w)
	if lv, err := logrus.ParseLevel(cfg.LogLevel); err == nil {
		logrus.SetLevel(lv)
	}
	return slog.New(h)
}
