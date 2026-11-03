package config

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func envOf(m map[string]string) LookupFunc {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func noFiles(string) ([]byte, error) { return nil, os.ErrNotExist }

func baseEnv() map[string]string {
	return map[string]string{
		"CONTABO_CLIENT_ID":     "INT-1",
		"CONTABO_CLIENT_SECRET": "secret",
		"CONTABO_API_USER":      "user@example.com",
		"CONTABO_API_PASSWORD":  "pw",
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := LoadFrom(envOf(baseEnv()), noFiles)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.APIURL != DefaultAPIURL || c.AuthURL != DefaultAuthURL {
		t.Errorf("wrong URLs: %s %s", c.APIURL, c.AuthURL)
	}
	if c.WebhookAddress() != "localhost:8888" {
		t.Errorf("webhook address = %s", c.WebhookAddress())
	}
	if c.HealthAddress() != "0.0.0.0:8080" {
		t.Errorf("health address = %s", c.HealthAddress())
	}
	if c.DefaultTTL != DefaultTTL || c.RateLimit != DefaultRateLimit || c.MaxRetries != DefaultMaxRetries {
		t.Errorf("wrong numeric defaults: %+v", c)
	}
	if c.DryRun {
		t.Error("dry run should default to false")
	}
	if c.LogLevel != "info" || c.LogFormat != "text" {
		t.Errorf("wrong log defaults: %s %s", c.LogLevel, c.LogFormat)
	}
}

func TestLoadOverrides(t *testing.T) {
	env := baseEnv()
	env["DOMAIN_FILTER"] = " example.com, ,example.org "
	env["EXCLUDE_DOMAIN_FILTER"] = "private.example.com"
	env["DRY_RUN"] = "TRUE"
	env["CONTABO_DEFAULT_TTL"] = "300"
	env["CONTABO_REQUEST_TIMEOUT"] = "5s"
	env["CONTABO_RATE_LIMIT"] = "2.5"
	env["CONTABO_MAX_RETRIES"] = "0"
	env["WEBHOOK_HOST"] = "::1"
	env["WEBHOOK_PORT"] = "9999"
	env["LOG_LEVEL"] = "DEBUG"
	env["LOG_FORMAT"] = "json"

	c, err := LoadFrom(envOf(env), noFiles)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Join(c.DomainFilter, "|") != "example.com|example.org" {
		t.Errorf("domain filter = %v", c.DomainFilter)
	}
	if len(c.ExcludeDomainFilter) != 1 {
		t.Errorf("exclude filter = %v", c.ExcludeDomainFilter)
	}
	if !c.DryRun || c.DefaultTTL != 300 || c.RequestTimeout != 5*time.Second || c.RateLimit != 2.5 || c.MaxRetries != 0 {
		t.Errorf("overrides not applied: %+v", c)
	}
	if c.WebhookAddress() != "[::1]:9999" {
		t.Errorf("IPv6 address not bracketed: %s", c.WebhookAddress())
	}
	if c.LogLevel != "debug" {
		t.Errorf("log level not lower-cased: %s", c.LogLevel)
	}
}

func TestSecretFromFile(t *testing.T) {
	env := baseEnv()
	delete(env, "CONTABO_API_PASSWORD")
	env["CONTABO_API_PASSWORD_FILE"] = "/secrets/pw"
	read := func(name string) ([]byte, error) {
		if name == "/secrets/pw" {
			return []byte("from-file\n"), nil
		}
		return nil, os.ErrNotExist
	}
	c, err := LoadFrom(envOf(env), read)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.APIPassword != "from-file" {
		t.Errorf("password = %q, want trimmed file content", c.APIPassword)
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name string
		mod  func(map[string]string)
		want string
	}{
		{"missing client id", func(e map[string]string) { delete(e, "CONTABO_CLIENT_ID") }, "CONTABO_CLIENT_ID is required"},
		{"blank secret", func(e map[string]string) { e["CONTABO_CLIENT_SECRET"] = "   " }, "CONTABO_CLIENT_SECRET is required"},
		{"value and file", func(e map[string]string) { e["CONTABO_API_USER_FILE"] = "/x" }, "set only one of CONTABO_API_USER"},
		{"missing file", func(e map[string]string) {
			delete(e, "CONTABO_API_USER")
			e["CONTABO_API_USER_FILE"] = "/missing"
		}, "CONTABO_API_USER_FILE"},
		{"bad bool", func(e map[string]string) { e["DRY_RUN"] = "maybe" }, "DRY_RUN must be true or false"},
		{"bad int", func(e map[string]string) { e["WEBHOOK_PORT"] = "abc" }, "WEBHOOK_PORT must be a whole number"},
		{"port range", func(e map[string]string) { e["HEALTH_PORT"] = "70000" }, "HEALTH_PORT must be between"},
		{"same address", func(e map[string]string) {
			e["WEBHOOK_HOST"] = "0.0.0.0"
			e["WEBHOOK_PORT"] = "8080"
		}, "cannot use the same host and port"},
		{"bad regex", func(e map[string]string) { e["REGEX_DOMAIN_FILTER"] = "([" }, "not a valid regular expression"},
		{"mixed filters", func(e map[string]string) {
			e["DOMAIN_FILTER"] = "a.com"
			e["REGEX_DOMAIN_FILTER"] = ".*"
		}, "not both"},
		{"ttl zero", func(e map[string]string) { e["CONTABO_DEFAULT_TTL"] = "0" }, "CONTABO_DEFAULT_TTL"},
		{"bad url", func(e map[string]string) { e["CONTABO_API_URL"] = "api.contabo.com" }, "CONTABO_API_URL must be"},
		{"bad duration", func(e map[string]string) { e["CONTABO_REQUEST_TIMEOUT"] = "10" }, "duration"},
		{"negative timeout", func(e map[string]string) { e["CONTABO_REQUEST_TIMEOUT"] = "-1s" }, "must be positive"},
		{"zero rate", func(e map[string]string) { e["CONTABO_RATE_LIMIT"] = "0" }, "CONTABO_RATE_LIMIT"},
		{"bad float", func(e map[string]string) { e["CONTABO_RATE_LIMIT"] = "fast" }, "must be a number"},
		{"negative retries", func(e map[string]string) { e["CONTABO_MAX_RETRIES"] = "-1" }, "must not be negative"},
		{"bad log level", func(e map[string]string) { e["LOG_LEVEL"] = "trace" }, "LOG_LEVEL"},
		{"bad log format", func(e map[string]string) { e["LOG_FORMAT"] = "xml" }, "LOG_FORMAT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := baseEnv()
			tt.mod(env)
			_, err := LoadFrom(envOf(env), noFiles)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not contain %q", err, tt.want)
			}
		})
	}
}

func TestErrorsNeverLeakSecrets(t *testing.T) {
	env := baseEnv()
	env["CONTABO_CLIENT_SECRET"] = "super-secret-value"
	env["LOG_LEVEL"] = "nope"
	_, err := LoadFrom(envOf(env), noFiles)
	if err == nil || strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("error must exist and must not contain the secret: %v", err)
	}
	if errors.Unwrap(err) == nil && !strings.Contains(err.Error(), "LOG_LEVEL") {
		t.Fatalf("unexpected error: %v", err)
	}
}
