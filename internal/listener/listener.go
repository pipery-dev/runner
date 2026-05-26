package listener

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pipery-dev/runner/internal/job"
	"github.com/pipery-dev/runner/internal/register"
	"github.com/pipery-dev/runner/internal/result"
	"github.com/pipery-dev/runner/internal/runner"
)

type Listener struct {
	Root        string
	Stdout      io.Writer
	Stderr      io.Writer
	HTTPClient  *http.Client
	Debug       bool
	DebugWriter io.Writer
}

func (l *Listener) Run(ctx context.Context) error {
	if l.Stdout == nil {
		l.Stdout = io.Discard
	}
	if l.Stderr == nil {
		l.Stderr = io.Discard
	}
	if l.Root == "" {
		l.Root = "."
	}
	if l.HTTPClient == nil {
		l.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}

	settings, credentials, key, err := loadConfig(l.Root)
	if err != nil {
		return err
	}

	authToken, err := l.oauthToken(ctx, settings, credentials, key)
	if err != nil {
		return err
	}

	brokerMode := settings.UseV2Flow && strings.TrimSpace(settings.ServerURLV2) != ""
	session, err := l.createSession(ctx, settings, credentials, authToken, key, brokerMode)
	if err != nil {
		return err
	}
	defer func() {
		_ = l.deleteSession(context.Background(), settings, authToken, session.SessionID, brokerMode)
	}()

	workDir := filepath.Join(l.Root, settings.WorkFolder)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}

	runnerRoot := runner.Runner{
		WorkDir: workDir,
		TempDir: filepath.Join(workDir, "_temp"),
		Stdout:  l.Stdout,
		Stderr:  l.Stderr,
	}

	fmt.Fprintf(l.Stdout, "Current runner version: %q\n", register.RunnerVersion)
	fmt.Fprintf(l.Stdout, "%s: Listening for Jobs\n", time.Now().UTC().Format("2006-01-02 15:04:05Z"))

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		message, err := l.getNextMessage(ctx, settings, authToken, session.SessionID, brokerMode)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			return err
		}
		if message == nil {
			continue
		}

		decrypted, err := decryptMessage(session, message)
		if err != nil {
			return err
		}
		if decrypted == nil || strings.TrimSpace(decrypted.Body) == "" {
			continue
		}

		switch decrypted.MessageType {
		case "PipelineAgentJobRequest", "PipelineAgentJobRequestMessage":
			var msg job.Message
			if err := json.Unmarshal([]byte(decrypted.Body), &msg); err != nil {
				return fmt.Errorf("decode job message: %w", err)
			}
			if msg.RequestID == 0 {
				msg.RequestID = decrypted.MessageId
			}
			if msg.JobID == "" {
				msg.JobID = fmt.Sprintf("%d", msg.RequestID)
			}
			if brokerMode {
				runnerRequestID := fmt.Sprintf("%d", msg.RequestID)
				if msg.RequestID == 0 {
					runnerRequestID = fmt.Sprintf("%d", decrypted.MessageId)
				}
				if err := l.acknowledgeRunnerRequest(ctx, settings, authToken, runnerRequestID, session.SessionID); err != nil {
					return err
				}
			}
			jobCtx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func(requestID int64) {
				defer close(done)
				l.renewJobRequestLoop(jobCtx, settings, authToken, requestID)
			}(msg.RequestID)
			resultValue, runErr := runnerRoot.Run(jobCtx, msg)
			cancel()
			<-done
			if completeErr := l.completeJobRequest(context.Background(), settings, authToken, msg.RequestID, resultValue, runErr); completeErr != nil {
				return completeErr
			}
			if settings.Ephemeral {
				return nil
			}
		default:
			l.debugf("ignoring message type=%s id=%d\n", decrypted.MessageType, decrypted.MessageId)
		}
	}
}

func loadConfig(root string) (register.Settings, register.Credentials, *rsa.PrivateKey, error) {
	settingsBytes, err := os.ReadFile(filepath.Join(root, ".runner"))
	if err != nil {
		return register.Settings{}, register.Credentials{}, nil, err
	}
	credentialsBytes, err := os.ReadFile(filepath.Join(root, ".credentials"))
	if err != nil {
		return register.Settings{}, register.Credentials{}, nil, err
	}

	var settings register.Settings
	if err := json.Unmarshal(settingsBytes, &settings); err != nil {
		return register.Settings{}, register.Credentials{}, nil, err
	}
	var credentials register.Credentials
	if err := json.Unmarshal(credentialsBytes, &credentials); err != nil {
		return register.Settings{}, register.Credentials{}, nil, err
	}
	key, err := loadPrivateKey(filepath.Join(root, ".credentials_rsaparams"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return register.Settings{}, register.Credentials{}, nil, err
	}
	return settings, credentials, key, nil
}

func loadPrivateKey(path string) (*rsa.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("decode %s: no pem block found", path)
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := keyAny.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s: expected RSA private key", path)
	}
	return key, nil
}

func (l *Listener) oauthToken(ctx context.Context, settings register.Settings, credentials register.Credentials, key *rsa.PrivateKey) (string, error) {
	clientID := strings.TrimSpace(credentials.Data["clientId"])
	authURL := strings.TrimSpace(credentials.Data["authorizationUrl"])
	if clientID == "" || authURL == "" {
		return "", fmt.Errorf("credentials missing OAuth metadata")
	}
	if key == nil {
		return "", fmt.Errorf("credentials_rsaparams is required for OAuth authentication")
	}

	now := time.Now().UTC()
	audience := authURL
	if strings.HasSuffix(audience, "/"+clientID) {
		audience = strings.TrimSuffix(audience, "/"+clientID)
	}
	claims := map[string]any{
		"iss": clientID,
		"sub": clientID,
		"aud": audience,
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
		"jti": randomJTI(),
	}
	jwt, err := signJWT(key, claims)
	if err != nil {
		return "", err
	}

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_assertion_type", "urn:ietf:params:oauth:client-assertion-type:jwt-bearer")
	form.Set("client_assertion", jwt)
	form.Set("grant_type", "client_credentials")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "GitHubActionsRunner-runner/"+register.RunnerVersion)

	resp, err := l.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("oauth token request failed: %s body=%s", resp.Status, strings.TrimSpace(string(body)))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", err
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("oauth token response missing access_token")
	}
	l.debugf("obtained OAuth access token via client assertion\n")
	return tokenResp.AccessToken, nil
}

func signJWT(key *rsa.PrivateKey, claims map[string]any) (string, error) {
	header := map[string]string{"alg": "RS256", "typ": "JWT"}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encodedHeader := base64.RawURLEncoding.EncodeToString(headerJSON)
	encodedClaims := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := encodedHeader + "." + encodedClaims
	sum := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func randomJTI() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", b[:])
}

func (l *Listener) createSession(ctx context.Context, settings register.Settings, credentials register.Credentials, token string, key *rsa.PrivateKey, brokerMode bool) (*taskAgentSession, error) {
	agent := taskAgentReference{
		ID:            settings.AgentID,
		Name:          settings.AgentName,
		Version:       register.RunnerVersion,
		OSDescription: runtime.GOOS + "/" + runtime.GOARCH,
	}
	sessionName, _ := os.Hostname()
	if sessionName == "" {
		sessionName = "runner"
	}
	body := taskAgentSession{
		OwnerName:         sessionName,
		Agent:             agent,
		UseFipsEncryption: strings.EqualFold(credentials.Data["requireFipsCryptography"], "true"),
	}
	endpoint := l.sessionEndpoint(settings, brokerMode)
	var session taskAgentSession
	if err := l.doJSON(ctx, http.MethodPost, endpoint, "Bearer "+token, body, &session); err != nil {
		return nil, err
	}
	session.PrivateKey = key
	return &session, nil
}

func (l *Listener) deleteSession(ctx context.Context, settings register.Settings, token string, sessionID string, brokerMode bool) error {
	if sessionID == "" {
		return nil
	}
	endpoint := l.deleteSessionEndpoint(settings, sessionID, brokerMode)
	return l.doJSON(ctx, http.MethodDelete, endpoint, "Bearer "+token, nil, nil)
}

func (l *Listener) getNextMessage(ctx context.Context, settings register.Settings, token string, sessionID string, brokerMode bool) (*taskAgentMessage, error) {
	endpoint := l.messageEndpoint(settings, sessionID, "Online", brokerMode)
	var message taskAgentMessage
	if err := l.doJSON(ctx, http.MethodGet, endpoint, "Bearer "+token, nil, &message); err != nil {
		return nil, err
	}
	if message.MessageType == "" && message.Body == "" {
		return nil, nil
	}
	return &message, nil
}

func (l *Listener) acknowledgeRunnerRequest(ctx context.Context, settings register.Settings, token, runnerRequestID, sessionID string) error {
	if runnerRequestID == "" {
		return nil
	}
	endpoint := l.acknowledgeEndpoint(settings, sessionID, "Online")
	body := map[string]string{"runnerRequestId": runnerRequestID}
	return l.doJSON(ctx, http.MethodPost, endpoint, "Bearer "+token, body, nil)
}

func (l *Listener) renewJobRequestLoop(ctx context.Context, settings register.Settings, token string, requestID int64) {
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = l.renewJobRequest(ctx, settings, token, requestID)
		}
	}
}

func (l *Listener) renewJobRequest(ctx context.Context, settings register.Settings, token string, requestID int64) error {
	request, err := l.getJobRequest(ctx, settings, token, requestID)
	if err != nil {
		return err
	}
	if request == nil {
		return nil
	}
	if request["lockedUntil"] == nil {
		request["lockedUntil"] = time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano)
	} else {
		request["lockedUntil"] = time.Now().UTC().Add(5 * time.Minute).Format(time.RFC3339Nano)
	}
	return l.patchJobRequest(ctx, settings, token, requestID, request)
}

func (l *Listener) completeJobRequest(ctx context.Context, settings register.Settings, token string, requestID int64, jobResult result.Result, runErr error) error {
	request, err := l.getJobRequest(ctx, settings, token, requestID)
	if err != nil {
		return err
	}
	if request == nil {
		request = map[string]any{}
	}
	request["finishTime"] = time.Now().UTC().Format(time.RFC3339Nano)
	request["result"] = string(jobResult)
	if runErr != nil {
		request["statusMessage"] = runErr.Error()
	}
	return l.patchJobRequest(ctx, settings, token, requestID, request)
}

func (l *Listener) getJobRequest(ctx context.Context, settings register.Settings, token string, requestID int64) (map[string]any, error) {
	endpoint := fmt.Sprintf("%s/_apis/distributedtask/pools/%d/jobrequests/%d?api-version=6.0-preview", strings.TrimRight(settings.ServerURL, "/"), settings.PoolID, requestID)
	var request map[string]any
	if err := l.doJSON(ctx, http.MethodGet, endpoint, "Bearer "+token, nil, &request); err != nil {
		return nil, err
	}
	return request, nil
}

func (l *Listener) patchJobRequest(ctx context.Context, settings register.Settings, token string, requestID int64, request map[string]any) error {
	endpoint := fmt.Sprintf("%s/_apis/distributedtask/pools/%d/jobrequests/%d?api-version=6.0-preview", strings.TrimRight(settings.ServerURL, "/"), settings.PoolID, requestID)
	return l.doJSON(ctx, http.MethodPatch, endpoint, "Bearer "+token, request, nil)
}

func decryptMessage(session *taskAgentSession, message *taskAgentMessage) (*taskAgentMessage, error) {
	if session == nil || message == nil || len(message.IV) == 0 || session.EncryptionKey == nil || len(session.EncryptionKey.Value) == 0 {
		return message, nil
	}
	keyBytes := session.EncryptionKey.Value
	if session.EncryptionKey.Encrypted {
		// The runner key stored on disk is the RSA private key used to unwrap the session key.
		// The service sends an RSA-encrypted AES key in the session response.
		// This uses OAEP SHA-1 by default, matching the current runner behavior for non-FIPS.
		// If that fails, try SHA-256.
		var decryptErr error
		for _, hashFunc := range []func() hash.Hash{sha1.New, sha256.New} {
			keyBytes, decryptErr = rsa.DecryptOAEP(hashFunc(), rand.Reader, session.PrivateKey, session.EncryptionKey.Value, nil)
			if decryptErr == nil {
				break
			}
		}
		if decryptErr != nil {
			return nil, decryptErr
		}
	}
	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return nil, err
	}
	if len(message.IV) != block.BlockSize() {
		return nil, fmt.Errorf("invalid iv length %d", len(message.IV))
	}
	cipherText, err := base64.StdEncoding.DecodeString(message.Body)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(cipherText))
	mode := cipher.NewCBCDecrypter(block, message.IV)
	mode.CryptBlocks(plain, cipherText)
	plain, err = pkcs7Unpad(plain)
	if err != nil {
		return nil, err
	}
	message.Body = string(plain)
	return message, nil
}

func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty padded data")
	}
	pad := int(data[len(data)-1])
	if pad == 0 || pad > len(data) {
		return nil, fmt.Errorf("invalid padding")
	}
	for _, b := range data[len(data)-pad:] {
		if int(b) != pad {
			return nil, fmt.Errorf("invalid padding")
		}
	}
	return data[:len(data)-pad], nil
}

func (l *Listener) doJSON(ctx context.Context, method, endpoint, authHeader string, body any, into any) error {
	var reader io.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", authHeader)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "GitHubActionsRunner-runner/"+register.RunnerVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := l.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s: %s body=%s", method, endpoint, resp.Status, strings.TrimSpace(string(respBody)))
	}
	if into == nil {
		return nil
	}
	if len(bytes.TrimSpace(respBody)) == 0 {
		return nil
	}
	return json.Unmarshal(respBody, into)
}

func (l *Listener) sessionEndpoint(settings register.Settings, brokerMode bool) string {
	if brokerMode {
		return strings.TrimRight(settings.ServerURLV2, "/") + "/session"
	}
	return fmt.Sprintf("%s/_apis/distributedtask/pools/%d/sessions?api-version=6.0-preview", strings.TrimRight(settings.ServerURL, "/"), settings.PoolID)
}

func (l *Listener) deleteSessionEndpoint(settings register.Settings, sessionID string, brokerMode bool) string {
	if brokerMode {
		return strings.TrimRight(settings.ServerURLV2, "/") + "/session"
	}
	return fmt.Sprintf("%s/_apis/distributedtask/pools/%d/sessions/%s?api-version=6.0-preview", strings.TrimRight(settings.ServerURL, "/"), settings.PoolID, sessionID)
}

func (l *Listener) messageEndpoint(settings register.Settings, sessionID, status string, brokerMode bool) string {
	if brokerMode {
		return fmt.Sprintf("%s/message?sessionId=%s&status=%s&runnerVersion=%s&os=%s&architecture=%s&disableUpdate=%t",
			strings.TrimRight(settings.ServerURLV2, "/"),
			url.QueryEscape(sessionID),
			url.QueryEscape(status),
			url.QueryEscape(register.RunnerVersion),
			url.QueryEscape(strings.ToLower(runtime.GOOS)),
			url.QueryEscape(strings.ToUpper(runtime.GOARCH)),
			settings.DisableUpdate,
		)
	}
	return fmt.Sprintf("%s/_apis/distributedtask/pools/%d/messages?sessionId=%s&status=%s&runnerVersion=%s&os=%s&architecture=%s&disableUpdate=%t&api-version=6.0-preview",
		strings.TrimRight(settings.ServerURL, "/"),
		settings.PoolID,
		url.QueryEscape(sessionID),
		url.QueryEscape(status),
		url.QueryEscape(register.RunnerVersion),
		url.QueryEscape(strings.ToLower(runtime.GOOS)),
		url.QueryEscape(strings.ToUpper(runtime.GOARCH)),
		settings.DisableUpdate,
	)
}

func (l *Listener) acknowledgeEndpoint(settings register.Settings, sessionID, status string) string {
	return fmt.Sprintf("%s/acknowledge?sessionId=%s&status=%s&runnerVersion=%s&os=%s&architecture=%s",
		strings.TrimRight(settings.ServerURLV2, "/"),
		url.QueryEscape(sessionID),
		url.QueryEscape(status),
		url.QueryEscape(register.RunnerVersion),
		url.QueryEscape(strings.ToLower(runtime.GOOS)),
		url.QueryEscape(strings.ToUpper(runtime.GOARCH)),
	)
}

func (l *Listener) debugf(format string, args ...any) {
	if !l.Debug {
		return
	}
	out := l.DebugWriter
	if out == nil {
		out = io.Discard
	}
	fmt.Fprintf(out, "[listener-debug] "+format, args...)
}

type taskAgentReference struct {
	ID            uint64 `json:"id,omitempty"`
	Name          string `json:"name,omitempty"`
	Version       string `json:"version,omitempty"`
	OSDescription string `json:"osDescription,omitempty"`
}

type taskAgentSessionKey struct {
	Value     []byte `json:"value,omitempty"`
	Encrypted bool   `json:"encrypted,omitempty"`
}

type taskAgentSession struct {
	SessionID              string               `json:"sessionId,omitempty"`
	EncryptionKey          *taskAgentSessionKey `json:"encryptionKey,omitempty"`
	OwnerName              string               `json:"ownerName,omitempty"`
	Agent                  taskAgentReference   `json:"agent,omitempty"`
	UseFipsEncryption      bool                 `json:"useFipsEncryption,omitempty"`
	BrokerMigrationMessage any                  `json:"brokerMigrationMessage,omitempty"`
	PrivateKey             *rsa.PrivateKey      `json:"-"`
}

type taskAgentMessage struct {
	MessageId   int64  `json:"messageId,omitempty"`
	MessageType string `json:"messageType,omitempty"`
	IV          []byte `json:"iv,omitempty"`
	Body        string `json:"body,omitempty"`
}
