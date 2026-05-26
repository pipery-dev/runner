package listener

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/pipery-dev/runner/internal/register"
)

func TestLoadConfigReadsPrivateKey(t *testing.T) {
	dir := t.TempDir()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	reg := register.Registration{
		Settings: register.Settings{
			AgentID:    7,
			AgentName:  "runner",
			PoolID:     1,
			ServerURL:  "https://tenant.example",
			WorkFolder: "_work",
		},
		Credentials: register.Credentials{
			Scheme: "OAuth",
			Data: map[string]string{
				"accessToken": "token-value",
			},
		},
		PrivateKey: key,
	}
	if err := register.Save(dir, reg); err != nil {
		t.Fatal(err)
	}

	settings, credentials, loadedKey, err := loadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if settings.AgentID != 7 || settings.PoolID != 1 {
		t.Fatalf("settings = %#v", settings)
	}
	if credentials.Data["accessToken"] != "token-value" {
		t.Fatalf("credentials = %#v", credentials)
	}
	if loadedKey == nil {
		t.Fatal("expected private key")
	}

	if _, err := os.Stat(filepath.Join(dir, ".credentials_rsaparams")); err != nil {
		t.Fatalf("missing private key file: %v", err)
	}
}

func TestOAuthTokenUsesAuthorizationURLAsIs(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var sawRequest bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		sawRequest = true
		if r.URL.String() != "https://tokenghub.actions.githubusercontent.com/_apis/oauth2/token/client-id" {
			t.Fatalf("url = %s", r.URL.String())
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("content-type = %q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, []byte("client_id=client-id")) {
			t.Fatalf("missing client_id in %q", string(body))
		}
		if !bytes.Contains(body, []byte("client_assertion_type=urn%3Aietf%3Aparams%3Aoauth%3Aclient-assertion-type%3Ajwt-bearer")) {
			t.Fatalf("missing client_assertion_type in %q", string(body))
		}
		if !bytes.Contains(body, []byte("grant_type=client_credentials")) {
			t.Fatalf("missing grant_type in %q", string(body))
		}
		respBody, _ := json.Marshal(map[string]string{"access_token": "oauth-token"})
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     http.StatusText(http.StatusOK),
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(respBody)),
		}, nil
	})}

	l := &Listener{HTTPClient: client}
	token, err := l.oauthToken(context.Background(), register.Settings{}, register.Credentials{
		Data: map[string]string{
			"clientId":         "client-id",
			"authorizationUrl": "https://tokenghub.actions.githubusercontent.com/_apis/oauth2/token/client-id",
		},
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	if !sawRequest {
		t.Fatal("did not issue oauth request")
	}
	if token != "oauth-token" {
		t.Fatalf("token = %q", token)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
