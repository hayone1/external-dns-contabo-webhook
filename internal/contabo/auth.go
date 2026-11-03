package contabo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// Contabo access tokens live for 5 minutes. Getting a new one 30 seconds
// early avoids sending a token that expires while the request is in flight.
const tokenEarlyExpiry = 30 * time.Second

// Credentials are the four values from https://my.contabo.com/api/details.
type Credentials struct {
	ClientID     string
	ClientSecret string
	APIUser      string
	APIPassword  string
}

// tokenCache runs the OAuth2 password grant whenever it has no usable token.
//
// It does not use refresh tokens on purpose. The Contabo refresh token
// expires after 30 minutes, and the standard oauth2 refresher never falls
// back to the password grant, so a webhook built on it would stop working
// after a quiet half hour. Running the password grant again always works.
type tokenCache struct {
	cfg     *oauth2.Config
	creds   Credentials
	http    *http.Client
	timeout time.Duration
	onError func()
	now     func() time.Time

	mu  sync.Mutex
	tok *oauth2.Token
}

func newTokenCache(authURL string, creds Credentials, httpClient *http.Client, timeout time.Duration, onError func()) *tokenCache {
	return &tokenCache{
		cfg: &oauth2.Config{
			ClientID:     creds.ClientID,
			ClientSecret: creds.ClientSecret,
			Endpoint:     oauth2.Endpoint{TokenURL: authURL, AuthStyle: oauth2.AuthStyleInParams},
		},
		creds:   creds,
		http:    httpClient,
		timeout: timeout,
		onError: onError,
		now:     time.Now,
	}
}

// accessToken returns a token that is valid for at least tokenEarlyExpiry.
func (c *tokenCache) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.usable(c.tok) {
		return c.tok.AccessToken, nil
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, c.http)

	tok, err := c.cfg.PasswordCredentialsToken(ctx, c.creds.APIUser, c.creds.APIPassword)
	if err != nil {
		if c.onError != nil {
			c.onError()
		}
		return "", classifyTokenError(err)
	}
	if tok.AccessToken == "" {
		if c.onError != nil {
			c.onError()
		}
		return "", errors.New("contabo: token response did not contain an access token")
	}
	c.tok = tok
	return tok.AccessToken, nil
}

// invalidate drops the cached token if it is still the one that failed.
// Comparing first stops a slow request from throwing away a newer token.
func (c *tokenCache) invalidate(failed string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tok != nil && c.tok.AccessToken == failed {
		c.tok = nil
	}
}

func (c *tokenCache) usable(t *oauth2.Token) bool {
	if t == nil || t.AccessToken == "" {
		return false
	}
	return t.Expiry.IsZero() || t.Expiry.Sub(c.now()) > tokenEarlyExpiry
}

// classifyTokenError turns OAuth2 errors into ErrInvalidCredentials when the
// server says the credentials are wrong. It never includes the raw response
// body, which could echo request details.
func classifyTokenError(err error) error {
	if re, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
		switch re.ErrorCode {
		case "invalid_grant", "invalid_client", "unauthorized_client":
			return fmt.Errorf("%w: %s: %s", ErrInvalidCredentials, re.ErrorCode, re.ErrorDescription)
		}
		status := 0
		if re.Response != nil {
			status = re.Response.StatusCode
		}
		if status == http.StatusUnauthorized || status == http.StatusForbidden {
			return fmt.Errorf("%w: HTTP %d", ErrInvalidCredentials, status)
		}
		return fmt.Errorf("contabo: token request failed: HTTP %d %s", status, re.ErrorCode)
	}
	return fmt.Errorf("contabo: token request failed: %w", err)
}
