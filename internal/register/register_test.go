package register

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRegisterHostedFlow(t *testing.T) {
	var sawRegister bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Authorization"); got != "RemoteAuth secret" {
			t.Fatalf("authorization = %q", got)
		}
		write := func(status int, value any) (*http.Response, error) {
			var buf bytes.Buffer
			if err := json.NewEncoder(&buf).Encode(value); err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: status,
				Status:     http.StatusText(status),
				Header:     make(http.Header),
				Body:       io.NopCloser(&buf),
			}, nil
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/actions/runner-registration":
			return write(http.StatusOK, githubAuthResult{
				TenantURL:          "https://tenant.example",
				TokenSchema:        "OAuthAccessToken",
				Token:              "ignored",
				UseRunnerAdminFlow: true,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/orgs/pipery-dev/actions/runner-groups":
			return write(http.StatusOK, runnerGroupList{RunnerGroups: []runnerGroup{{ID: 1, Name: "Default", Default: true}}})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v3/orgs/pipery-dev/actions/runners":
			return write(http.StatusOK, listRunnersResponse{})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v3/actions/runners/register":
			var body registerRunnerRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Name != "test-runner" || body.GroupID != 1 || body.PublicKey == "" {
				t.Fatalf("bad register body: %#v", body)
			}
			sawRegister = true
			return write(http.StatusOK, registeredRunner{
				ID: 42,
				Authorization: runnerAuthorization{
					AuthorizationURL: "https://auth.example",
					ServerURL:        "https://broker.example",
					ClientID:         "11111111-1111-1111-1111-111111111111",
				},
				Properties: runnerRegistrationProperties{
					UseV2Flow:   true,
					ServerURLV2: "https://broker.actions.githubusercontent.com/",
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		return nil, nil
	})}

	reg, err := NewClient(client).Register(context.Background(), Options{
		URL:   "http://example.test/pipery-dev",
		Token: "secret",
		Name:  "test-runner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawRegister {
		t.Fatal("did not call register endpoint")
	}
	if reg.Settings.AgentID != 42 || reg.Settings.PoolName != "Default" {
		t.Fatalf("settings = %#v", reg.Settings)
	}
	if !reg.Settings.UseV2Flow || reg.Settings.ServerURLV2 != "https://broker.actions.githubusercontent.com/" {
		t.Fatalf("settings = %#v", reg.Settings)
	}
	if reg.Credentials.Data["clientId"] == "" || reg.Credentials.Data["authorizationUrl"] == "" {
		t.Fatalf("credentials = %#v", reg.Credentials)
	}
}

func TestRegisterLegacyFlowWhenUseV2FlowIsOmitted(t *testing.T) {
	var sawAddAgent bool
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		write := func(status int, value any) (*http.Response, error) {
			var buf bytes.Buffer
			if err := json.NewEncoder(&buf).Encode(value); err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: status,
				Status:     http.StatusText(status),
				Header:     make(http.Header),
				Body:       io.NopCloser(&buf),
			}, nil
		}
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/actions/runner-registration"):
			return write(http.StatusOK, githubAuthResult{
				TenantURL:   "https://tenant.example",
				TokenSchema: "OAuthAccessToken",
				Token:       "tenant-token",
			})
		case r.Method == http.MethodGet && r.URL.Host == "tenant.example" && r.URL.Path == "/_apis/distributedtask/pools":
			if got := r.Header.Get("Authorization"); got != "Bearer tenant-token" {
				t.Fatalf("authorization = %q", got)
			}
			return write(http.StatusOK, legacyAgentPoolList{Value: []legacyAgentPool{{ID: 1, Name: "Default", IsInternal: true}}})
		case r.Method == http.MethodGet && r.URL.Host == "tenant.example" && r.URL.Path == "/_apis/distributedtask/pools/1/agents":
			return write(http.StatusOK, legacyTaskAgentList{})
		case r.Method == http.MethodPost && r.URL.Host == "tenant.example" && r.URL.Path == "/_apis/distributedtask/pools/1/agents":
			var body legacyTaskAgent
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Name != "test-runner" || body.Authorization.PublicKey.Modulus == "" {
				t.Fatalf("bad legacy body: %#v", body)
			}
			sawAddAgent = true
			return write(http.StatusOK, legacyTaskAgent{
				ID: 42,
				Authorization: legacyAgentAuthorization{
					AuthorizationURL: "https://auth.example",
					ClientID:         "11111111-1111-1111-1111-111111111111",
				},
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		return nil, nil
	})}

	_, err := NewClient(client).Register(context.Background(), Options{
		URL:   "http://example.test/pipery-dev",
		Token: "secret",
		Name:  "test-runner",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawAddAgent {
		t.Fatal("did not call legacy add-agent endpoint")
	}
}

func TestRegisterExistingRequiresReplace(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		write := func(status int, value any) (*http.Response, error) {
			var buf bytes.Buffer
			if err := json.NewEncoder(&buf).Encode(value); err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: status,
				Status:     http.StatusText(status),
				Header:     make(http.Header),
				Body:       io.NopCloser(&buf),
			}, nil
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/actions/runner-registration"):
			return write(http.StatusOK, githubAuthResult{UseRunnerAdminFlow: true})
		case strings.HasSuffix(r.URL.Path, "/runner-groups"):
			return write(http.StatusOK, runnerGroupList{RunnerGroups: []runnerGroup{{ID: 1, Name: "Default", Default: true}}})
		case strings.HasSuffix(r.URL.Path, "/runners"):
			return write(http.StatusOK, listRunnersResponse{Runners: []registeredRunner{{ID: 42, Name: "test-runner"}}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		return nil, nil
	})}

	_, err := NewClient(client).Register(context.Background(), Options{
		URL:   "http://example.test/pipery-dev",
		Token: "secret",
		Name:  "test-runner",
	})
	if err == nil || !strings.Contains(err.Error(), "--replace") {
		t.Fatalf("err = %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}
