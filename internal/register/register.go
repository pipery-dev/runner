package register

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"
)

const RunnerVersion = "2.334.0"

type Options struct {
	URL         string
	Token       string
	Name        string
	WorkFolder  string
	Replace     bool
	Ephemeral   bool
	Labels      []string
	HTTPClient  *http.Client
	Debug       bool
	DebugWriter io.Writer
}

type Settings struct {
	AgentID            uint64 `json:"agentId,omitempty"`
	AgentName          string `json:"agentName,omitempty"`
	PoolID             int    `json:"poolId,omitempty"`
	PoolName           string `json:"poolName,omitempty"`
	DisableUpdate      bool   `json:"disableUpdate,omitempty"`
	Ephemeral          bool   `json:"ephemeral,omitempty"`
	ServerURL          string `json:"serverUrl,omitempty"`
	GitHubURL          string `json:"gitHubUrl,omitempty"`
	WorkFolder         string `json:"workFolder,omitempty"`
	UseV2Flow          bool   `json:"useV2Flow,omitempty"`
	UseRunnerAdminFlow bool   `json:"useRunnerAdminFlow,omitempty"`
	ServerURLV2        string `json:"serverUrlV2,omitempty"`
}

type Credentials struct {
	Scheme string            `json:"scheme"`
	Data   map[string]string `json:"data"`
}

type Registration struct {
	Settings    Settings        `json:"settings"`
	Credentials Credentials     `json:"credentials"`
	PrivateKey  *rsa.PrivateKey `json:"-"`
}

type Client struct {
	httpClient *http.Client
	debug      bool
	debugOut   io.Writer
}

func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{httpClient: httpClient}
}

func (c *Client) Register(ctx context.Context, opts Options) (Registration, error) {
	if err := ValidateURL(opts.URL); err != nil {
		return Registration{}, err
	}
	if opts.Token == "" {
		return Registration{}, fmt.Errorf("token is required")
	}
	if opts.Name == "" {
		hostname, err := os.Hostname()
		if err != nil || hostname == "" {
			hostname = "runner"
		}
		opts.Name = hostname
	}
	if opts.WorkFolder == "" {
		opts.WorkFolder = "_work"
	}
	if opts.HTTPClient != nil {
		c.httpClient = opts.HTTPClient
	}
	c.debug = opts.Debug
	c.debugOut = opts.DebugWriter

	auth, err := c.tenantCredential(ctx, opts.URL, opts.Token, "register")
	if err != nil {
		return Registration{}, err
	}
	if !auth.UseRunnerAdminFlow {
		return c.registerLegacy(ctx, opts, auth)
	}

	groups, err := c.runnerGroups(ctx, opts.URL, opts.Token)
	if err != nil {
		return Registration{}, err
	}
	group, ok := defaultRunnerGroup(groups)
	if !ok {
		return Registration{}, fmt.Errorf("could not find a self-hosted runner group")
	}

	existing, err := c.runnerByName(ctx, opts.URL, opts.Token, opts.Name)
	if err != nil {
		return Registration{}, err
	}
	if len(existing.Runners) > 0 && !opts.Replace {
		return Registration{}, fmt.Errorf("runner %q already exists; pass --replace to replace it", opts.Name)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return Registration{}, err
	}
	publicXML := rsaPublicKeyXML(&key.PublicKey)

	req := registerRunnerRequest{
		URL:             opts.URL,
		GroupID:         group.ID,
		Name:            opts.Name,
		Version:         RunnerVersion,
		UpdatesDisabled: false,
		Ephemeral:       opts.Ephemeral,
		Labels:          labels(opts.Labels),
		PublicKey:       publicXML,
	}
	if len(existing.Runners) > 0 {
		req.RunnerID = existing.Runners[0].ID
		req.Replace = true
	}

	registered, err := c.addOrReplaceRunner(ctx, opts.URL, opts.Token, req)
	if err != nil {
		return Registration{}, err
	}
	if registered.Authorization.ClientID == "" || registered.Authorization.AuthorizationURL == "" {
		return Registration{}, fmt.Errorf("registration response did not include OAuth authorization data")
	}

	settings := Settings{
		AgentID:            registered.ID,
		AgentName:          opts.Name,
		PoolID:             group.ID,
		PoolName:           group.Name,
		Ephemeral:          opts.Ephemeral,
		ServerURL:          auth.TenantURL,
		GitHubURL:          opts.URL,
		WorkFolder:         opts.WorkFolder,
		UseV2Flow:          registered.Properties.UseV2Flow || registered.Properties.ServerURLV2 != "",
		UseRunnerAdminFlow: registered.Properties.UseV2Flow || registered.Properties.ServerURLV2 != "",
		ServerURLV2:        registered.Properties.ServerURLV2,
	}
	authorizationURL := registered.Authorization.AuthorizationURL
	if registered.Authorization.LegacyAuthorizationURL != "" {
		authorizationURL = registered.Authorization.LegacyAuthorizationURL
	}

	return Registration{
		Settings: settings,
		Credentials: Credentials{
			Scheme: "OAuth",
			Data: map[string]string{
				"clientId":         registered.Authorization.ClientID,
				"authorizationUrl": authorizationURL,
				"accessToken":      auth.Token,
			},
		},
		PrivateKey: key,
	}, nil
}

func (c *Client) registerLegacy(ctx context.Context, opts Options, auth githubAuthResult) (Registration, error) {
	if auth.TenantURL == "" || auth.Token == "" {
		return Registration{}, fmt.Errorf("legacy registration response did not include tenant URL and access token")
	}

	groups, err := c.legacyAgentPools(ctx, auth)
	if err != nil {
		return Registration{}, err
	}
	group, ok := defaultLegacyAgentPool(groups)
	if !ok {
		return Registration{}, fmt.Errorf("could not find a self-hosted runner group")
	}

	existing, err := c.legacyAgentsByName(ctx, auth, group.ID, opts.Name)
	if err != nil {
		return Registration{}, err
	}
	if len(existing.Value) > 0 && !opts.Replace {
		return Registration{}, fmt.Errorf("runner %q already exists; pass --replace to replace it", opts.Name)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return Registration{}, err
	}

	agent := legacyTaskAgent{
		Name:              opts.Name,
		Version:           RunnerVersion,
		OSDescription:     runtime.GOOS + "/" + runtime.GOARCH,
		Ephemeral:         opts.Ephemeral,
		DisableUpdate:     false,
		MaxParallelism:    1,
		ProvisioningState: "Provisioned",
		Authorization: legacyAgentAuthorization{
			PublicKey: rsaPublicKeyJSON(&key.PublicKey),
		},
		Labels: labels(opts.Labels),
	}

	var registered legacyTaskAgent
	if len(existing.Value) > 0 {
		agent.ID = existing.Value[0].ID
		registered, err = c.replaceLegacyAgent(ctx, auth, group.ID, agent)
	} else {
		registered, err = c.addLegacyAgent(ctx, auth, group.ID, agent)
	}
	if err != nil {
		return Registration{}, err
	}
	if registered.Authorization.ClientID == "" || registered.Authorization.AuthorizationURL == "" {
		return Registration{}, fmt.Errorf("legacy registration response did not include OAuth authorization data")
	}

	return Registration{
		Settings: Settings{
			AgentID:       registered.ID,
			AgentName:     opts.Name,
			PoolID:        group.ID,
			PoolName:      group.Name,
			Ephemeral:     opts.Ephemeral,
			ServerURL:     auth.TenantURL,
			GitHubURL:     opts.URL,
			WorkFolder:    opts.WorkFolder,
			UseV2Flow:     false,
			ServerURLV2:   "",
			DisableUpdate: false,
		},
		Credentials: Credentials{
			Scheme: "OAuth",
			Data: map[string]string{
				"clientId":                registered.Authorization.ClientID,
				"authorizationUrl":        registered.Authorization.AuthorizationURL,
				"accessToken":             auth.Token,
				"requireFipsCryptography": "true",
			},
		},
		PrivateKey: key,
	}, nil
}

func (c *Client) tenantCredential(ctx context.Context, githubURL, token, event string) (githubAuthResult, error) {
	endpoint, err := hostedAPIURL(githubURL, "/actions/runner-registration")
	if err != nil {
		return githubAuthResult{}, err
	}
	body := map[string]string{"url": githubURL, "runner_event": event}
	var result githubAuthResult
	if err := c.doJSON(ctx, http.MethodPost, endpoint, token, body, &result); err != nil {
		return githubAuthResult{}, err
	}
	return result, nil
}

func (c *Client) runnerGroups(ctx context.Context, githubURL, token string) (runnerGroupList, error) {
	endpoint, err := entityURL(githubURL, "/runner-groups")
	if err != nil {
		return runnerGroupList{}, err
	}
	var result runnerGroupList
	if err := c.doJSON(ctx, http.MethodGet, endpoint, token, nil, &result); err != nil {
		return runnerGroupList{}, err
	}
	return result, nil
}

func (c *Client) runnerByName(ctx context.Context, githubURL, token, name string) (listRunnersResponse, error) {
	endpoint, err := entityURL(githubURL, "/runners?name="+url.QueryEscape(name))
	if err != nil {
		return listRunnersResponse{}, err
	}
	var result listRunnersResponse
	if err := c.doJSON(ctx, http.MethodGet, endpoint, token, nil, &result); err != nil {
		return listRunnersResponse{}, err
	}
	return result, nil
}

func (c *Client) addOrReplaceRunner(ctx context.Context, githubURL, token string, body registerRunnerRequest) (registeredRunner, error) {
	endpoint, err := hostedAPIURL(githubURL, "/actions/runners/register")
	if err != nil {
		return registeredRunner{}, err
	}
	var result registeredRunner
	if err := c.doJSON(ctx, http.MethodPost, endpoint, token, body, &result); err != nil {
		return registeredRunner{}, err
	}
	return result, nil
}

func (c *Client) legacyAgentPools(ctx context.Context, auth githubAuthResult) (legacyAgentPoolList, error) {
	endpoint, err := legacyAPIURL(auth.TenantURL, "/_apis/distributedtask/pools", url.Values{
		"poolType":    {"Automation"},
		"api-version": {"5.1-preview.1"},
	})
	if err != nil {
		return legacyAgentPoolList{}, err
	}
	var result legacyAgentPoolList
	if err := c.doLegacyJSON(ctx, http.MethodGet, endpoint, auth.Token, nil, &result); err != nil {
		return legacyAgentPoolList{}, err
	}
	return result, nil
}

func (c *Client) legacyAgentsByName(ctx context.Context, auth githubAuthResult, poolID int, name string) (legacyTaskAgentList, error) {
	endpoint, err := legacyAPIURL(auth.TenantURL, fmt.Sprintf("/_apis/distributedtask/pools/%d/agents", poolID), url.Values{
		"agentName":   {name},
		"api-version": {"6.0-preview.2"},
	})
	if err != nil {
		return legacyTaskAgentList{}, err
	}
	var result legacyTaskAgentList
	if err := c.doLegacyJSON(ctx, http.MethodGet, endpoint, auth.Token, nil, &result); err != nil {
		return legacyTaskAgentList{}, err
	}
	return result, nil
}

func (c *Client) addLegacyAgent(ctx context.Context, auth githubAuthResult, poolID int, agent legacyTaskAgent) (legacyTaskAgent, error) {
	endpoint, err := legacyAPIURL(auth.TenantURL, fmt.Sprintf("/_apis/distributedtask/pools/%d/agents", poolID), url.Values{
		"api-version": {"6.0-preview.2"},
	})
	if err != nil {
		return legacyTaskAgent{}, err
	}
	var result legacyTaskAgent
	if err := c.doLegacyJSON(ctx, http.MethodPost, endpoint, auth.Token, agent, &result); err != nil {
		return legacyTaskAgent{}, err
	}
	return result, nil
}

func (c *Client) replaceLegacyAgent(ctx context.Context, auth githubAuthResult, poolID int, agent legacyTaskAgent) (legacyTaskAgent, error) {
	endpoint, err := legacyAPIURL(auth.TenantURL, fmt.Sprintf("/_apis/distributedtask/pools/%d/agents/%d", poolID, agent.ID), url.Values{
		"api-version": {"6.0-preview.2"},
	})
	if err != nil {
		return legacyTaskAgent{}, err
	}
	var result legacyTaskAgent
	if err := c.doLegacyJSON(ctx, http.MethodPut, endpoint, auth.Token, agent, &result); err != nil {
		return legacyTaskAgent{}, err
	}
	return result, nil
}

func (c *Client) doLegacyJSON(ctx context.Context, method, endpoint, token string, body any, into any) error {
	return c.doJSONWithAuth(ctx, method, endpoint, "Bearer "+token, body, into)
}

func (c *Client) doJSON(ctx context.Context, method, endpoint, token string, body any, into any) error {
	return c.doJSONWithAuth(ctx, method, endpoint, "RemoteAuth "+token, body, into)
}

func (c *Client) doJSONWithAuth(ctx context.Context, method, endpoint, authHeader string, body any, into any) error {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		err := c.doJSONOnce(ctx, method, endpoint, authHeader, body, into, attempt)
		if err == nil {
			return nil
		}
		lastErr = err
		var apiErr apiError
		if !errors.As(err, &apiErr) || apiErr.StatusCode < 500 || attempt == 3 {
			break
		}
		c.debugf("api retry scheduled attempt=%d next_attempt=%d delay=%s reason=%q\n", attempt, attempt+1, time.Duration(attempt)*time.Second, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * time.Second):
		}
	}
	return lastErr
}

func (c *Client) doJSONOnce(ctx context.Context, method, endpoint, authHeader string, body any, into any, attempt int) error {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	c.debugf("api request attempt=%d method=%s endpoint=%s body=%s\n", attempt, method, endpoint, debugJSON(body))
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("User-Agent", "GitHubActionsRunner-runner/"+RunnerVersion)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.debugf("api transport_error attempt=%d method=%s endpoint=%s error=%q\n", attempt, method, endpoint, err)
		return err
	}
	defer resp.Body.Close()
	requestID := resp.Header.Get("X-GitHub-Request-Id")
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		responseBody, _ := io.ReadAll(resp.Body)
		var msg map[string]any
		_ = json.Unmarshal(responseBody, &msg)
		c.debugf("api response attempt=%d method=%s endpoint=%s status=%q request_id=%s headers=%s body=%s\n", attempt, method, endpoint, resp.Status, requestID, debugHeaders(resp.Header), debugJSONBytes(responseBody))
		if text, ok := msg["message"].(string); ok && text != "" {
			return apiError{Method: method, Endpoint: endpoint, Status: resp.Status, StatusCode: resp.StatusCode, Message: text, RequestID: requestID}
		}
		text := strings.TrimSpace(string(responseBody))
		return apiError{Method: method, Endpoint: endpoint, Status: resp.Status, StatusCode: resp.StatusCode, Message: text, RequestID: requestID}
	}
	if into == nil {
		c.debugf("api response attempt=%d method=%s endpoint=%s status=%q request_id=%s headers=%s\n", attempt, method, endpoint, resp.Status, requestID, debugHeaders(resp.Header))
		return nil
	}
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	c.debugf("api response attempt=%d method=%s endpoint=%s status=%q request_id=%s headers=%s body=%s\n", attempt, method, endpoint, resp.Status, requestID, debugHeaders(resp.Header), debugJSONBytes(responseBody))
	return json.Unmarshal(responseBody, into)
}

type apiError struct {
	Method     string
	Endpoint   string
	Status     string
	StatusCode int
	Message    string
	RequestID  string
}

func (e apiError) Error() string {
	msg := fmt.Sprintf("%s %s: %s", e.Method, e.Endpoint, e.Status)
	if e.RequestID != "" {
		msg += " request_id=" + e.RequestID
	}
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
}

func (c *Client) debugf(format string, args ...any) {
	if !c.debug {
		return
	}
	out := c.debugOut
	if out == nil {
		out = io.Discard
	}
	fmt.Fprintf(out, "[register-debug] "+format, args...)
}

func debugJSON(value any) string {
	if value == nil {
		return "<empty>"
	}
	return debugJSONValue(value)
}

func debugJSONBytes(data []byte) string {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return "<empty>"
	}
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Sprintf("<non-json bytes=%d>", len(data))
	}
	return debugJSONValue(value)
}

func debugHeaders(headers http.Header) string {
	out := make(map[string][]string, len(headers))
	for key, values := range headers {
		switch strings.ToLower(key) {
		case "authorization", "set-cookie", "cookie":
			out[key] = []string{"<redacted>"}
		default:
			out[key] = values
		}
	}
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Sprintf("<unmarshalable %T>", headers)
	}
	return string(data)
}

func debugJSONValue(value any) string {
	data, err := json.Marshal(sanitizeDebugValue(value))
	if err != nil {
		return fmt.Sprintf("<unmarshalable %T>", value)
	}
	return string(data)
}

func sanitizeDebugValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = sanitizeDebugField(key, item)
		}
		return out
	case map[string]string:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = sanitizeDebugField(key, item)
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, sanitizeDebugValue(item))
		}
		return out
	case string, float64, bool, nil:
		return v
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("<%T>", value)
		}
		var decoded any
		if err := json.Unmarshal(data, &decoded); err != nil {
			return fmt.Sprintf("<%T>", value)
		}
		return sanitizeDebugValue(decoded)
	}
}

func sanitizeDebugField(key string, value any) any {
	switch strings.ToLower(key) {
	case "token", "authorizationurl", "authorization_url", "legacy_authorization_url", "publickey", "public_key", "clientid", "client_id":
		return "<redacted>"
	default:
		return sanitizeDebugValue(value)
	}
}

func Save(root string, registration Registration) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	settings, err := json.MarshalIndent(registration.Settings, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(root+"/.runner", settings, 0o600); err != nil {
		return err
	}
	credentials, err := json.MarshalIndent(registration.Credentials, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(root+"/.credentials", credentials, 0o600); err != nil {
		return err
	}
	if registration.PrivateKey != nil {
		return os.WriteFile(root+"/.credentials_rsaparams", pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(registration.PrivateKey),
		}), 0o600)
	}
	return nil
}

func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("scheme must be https")
	}
	if strings.Trim(u.Host, "/") == "" {
		return fmt.Errorf("host is required")
	}
	if strings.Trim(u.Path, "/") == "" {
		return fmt.Errorf("repository, organization, or enterprise path is required")
	}
	return nil
}

func hostedAPIURL(githubURL, suffix string) (string, error) {
	u, err := url.Parse(githubURL)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(u.Host, "github.com") {
		return "https://api.github.com" + suffix, nil
	}
	return u.Scheme + "://" + u.Host + "/api/v3" + suffix, nil
}

func entityURL(githubURL, suffix string) (string, error) {
	u, err := url.Parse(githubURL)
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 1 {
		if strings.EqualFold(u.Host, "github.com") {
			return "https://api.github.com/orgs/" + parts[0] + "/actions" + suffix, nil
		}
		return u.Scheme + "://" + u.Host + "/api/v3/orgs/" + parts[0] + "/actions" + suffix, nil
	}
	if len(parts) == 2 && !strings.EqualFold(parts[0], "enterprises") {
		if strings.EqualFold(u.Host, "github.com") {
			return "https://api.github.com/repos/" + parts[0] + "/" + parts[1] + "/actions" + suffix, nil
		}
		return u.Scheme + "://" + u.Host + "/api/v3/repos/" + parts[0] + "/" + parts[1] + "/actions" + suffix, nil
	}
	if len(parts) == 2 && strings.EqualFold(parts[0], "enterprises") {
		if strings.EqualFold(u.Host, "github.com") {
			return "https://api.github.com/enterprises/" + parts[1] + "/actions" + suffix, nil
		}
		return u.Scheme + "://" + u.Host + "/api/v3/enterprises/" + parts[1] + "/actions" + suffix, nil
	}
	return "", fmt.Errorf("%q should point to an org, repo, or enterprise", githubURL)
}

func legacyAPIURL(tenantURL, path string, query url.Values) (string, error) {
	u, err := url.Parse(tenantURL)
	if err != nil {
		return "", err
	}
	if strings.Trim(u.Scheme, "/") == "" || strings.Trim(u.Host, "/") == "" {
		return "", fmt.Errorf("tenant URL is invalid")
	}
	basePath := strings.TrimRight(u.Path, "/")
	apiPath := "/" + strings.TrimLeft(path, "/")
	u.Path = basePath + apiPath
	u.RawQuery = query.Encode()
	return u.String(), nil
}

func defaultRunnerGroup(groups runnerGroupList) (runnerGroup, bool) {
	for _, group := range groups.RunnerGroups {
		if group.Default && !group.IsHosted {
			return group, true
		}
	}
	for _, group := range groups.RunnerGroups {
		if !group.IsHosted {
			return group, true
		}
	}
	return runnerGroup{}, false
}

func defaultLegacyAgentPool(groups legacyAgentPoolList) (legacyAgentPool, bool) {
	for _, group := range groups.Value {
		if group.IsInternal && !group.IsHosted {
			return group, true
		}
	}
	for _, group := range groups.Value {
		if !group.IsHosted {
			return group, true
		}
	}
	return legacyAgentPool{}, false
}

func labels(user []string) []agentLabel {
	out := []agentLabel{
		{Name: "self-hosted", Type: "System"},
		{Name: runnerOS(), Type: "System"},
		{Name: runnerArch(), Type: "System"},
	}
	for _, label := range user {
		label = strings.TrimSpace(label)
		if label != "" {
			out = append(out, agentLabel{Name: label, Type: "User"})
		}
	}
	return out
}

func runnerOS() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS"
	case "windows":
		return "Windows"
	default:
		return "Linux"
	}
}

func runnerArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "X64"
	case "arm64":
		return "ARM64"
	default:
		return runtime.GOARCH
	}
}

func rsaPublicKeyXML(key *rsa.PublicKey) string {
	type rsaKeyValue struct {
		XMLName  xml.Name `xml:"RSAKeyValue"`
		Modulus  string   `xml:"Modulus"`
		Exponent string   `xml:"Exponent"`
	}
	exponent := []byte{byte(key.E >> 16), byte(key.E >> 8), byte(key.E)}
	exponent = bytes.TrimLeft(exponent, "\x00")
	data, _ := xml.Marshal(rsaKeyValue{
		Modulus:  base64.StdEncoding.EncodeToString(key.N.Bytes()),
		Exponent: base64.StdEncoding.EncodeToString(exponent),
	})
	return string(data)
}

func rsaPublicKeyJSON(key *rsa.PublicKey) legacyTaskAgentPublicKey {
	exponent := []byte{byte(key.E >> 16), byte(key.E >> 8), byte(key.E)}
	exponent = bytes.TrimLeft(exponent, "\x00")
	return legacyTaskAgentPublicKey{
		Exponent: base64.StdEncoding.EncodeToString(exponent),
		Modulus:  base64.StdEncoding.EncodeToString(key.N.Bytes()),
	}
}

type githubAuthResult struct {
	TenantURL          string `json:"url"`
	TokenSchema        string `json:"token_schema"`
	Token              string `json:"token"`
	UseRunnerAdminFlow bool   `json:"use_v2_flow"`
}

func (r githubAuthResult) SupportsRunnerAdminFlow() bool {
	return r.UseRunnerAdminFlow
}

type legacyAgentPoolList struct {
	Count int               `json:"count"`
	Value []legacyAgentPool `json:"value"`
}

type legacyAgentPool struct {
	ID         int    `json:"id"`
	Name       string `json:"name"`
	IsHosted   bool   `json:"isHosted"`
	IsInternal bool   `json:"isInternal"`
}

type legacyTaskAgentList struct {
	Count int               `json:"count"`
	Value []legacyTaskAgent `json:"value"`
}

type legacyTaskAgent struct {
	ID                uint64                   `json:"id,omitempty"`
	Name              string                   `json:"name,omitempty"`
	Version           string                   `json:"version,omitempty"`
	OSDescription     string                   `json:"osDescription,omitempty"`
	Enabled           *bool                    `json:"enabled,omitempty"`
	Ephemeral         bool                     `json:"ephemeral"`
	DisableUpdate     bool                     `json:"disableUpdate"`
	MaxParallelism    int                      `json:"maxParallelism,omitempty"`
	ProvisioningState string                   `json:"provisioningState,omitempty"`
	Authorization     legacyAgentAuthorization `json:"authorization,omitempty"`
	Labels            []agentLabel             `json:"labels,omitempty"`
}

type legacyAgentAuthorization struct {
	AuthorizationURL string                   `json:"authorizationUrl,omitempty"`
	ClientID         string                   `json:"clientId,omitempty"`
	PublicKey        legacyTaskAgentPublicKey `json:"publicKey,omitempty"`
}

type legacyTaskAgentPublicKey struct {
	Exponent string `json:"exponent,omitempty"`
	Modulus  string `json:"modulus,omitempty"`
}

type runnerGroupList struct {
	RunnerGroups []runnerGroup `json:"runner_groups"`
	Count        int           `json:"total_count"`
}

type runnerGroup struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	Default  bool   `json:"default"`
	IsHosted bool   `json:"is_hosted"`
}

type listRunnersResponse struct {
	TotalCount int                `json:"total_count"`
	Runners    []registeredRunner `json:"runners"`
}

type registeredRunner struct {
	ID            uint64              `json:"id"`
	Name          string              `json:"name"`
	Authorization runnerAuthorization `json:"authorization"`
	Properties    runnerRegistrationProperties `json:"properties"`
}

type runnerAuthorization struct {
	AuthorizationURL       string `json:"authorization_url"`
	LegacyAuthorizationURL string `json:"legacy_authorization_url"`
	ServerURL              string `json:"server_url"`
	ClientID               string `json:"client_id"`
}

type runnerRegistrationProperties struct {
	UseV2Flow   bool   `json:"useV2Flow"`
	ServerURLV2 string `json:"serverUrlV2"`
}

type registerRunnerRequest struct {
	URL             string       `json:"url"`
	GroupID         int          `json:"group_id"`
	Name            string       `json:"name"`
	Version         string       `json:"version"`
	UpdatesDisabled bool         `json:"updates_disabled"`
	Ephemeral       bool         `json:"ephemeral"`
	Labels          []agentLabel `json:"labels"`
	PublicKey       string       `json:"public_key"`
	RunnerID        uint64       `json:"runner_id,omitempty"`
	Replace         bool         `json:"replace,omitempty"`
}

type agentLabel struct {
	ID   int    `json:"Id"`
	Name string `json:"Name"`
	Type string `json:"Type"`
}
