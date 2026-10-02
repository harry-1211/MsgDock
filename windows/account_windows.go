//go:build windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	defaultAccountAPIURL = "https://msgdock.dpdns.org"
	accountPollInterval  = 3 * time.Second
	accountMaxBackoff    = 5 * time.Minute
	accountMessageLimit  = 50
)

// AccountCredentials are intentionally separate from CloudCredentials. The
// account API has a session token for account/device management and a device
// token for message access; neither token is shared with the legacy E2EE
// relay credentials.
type AccountCredentials struct {
	APIURL       string `json:"api_url,omitempty"`
	UserID       string `json:"user_id,omitempty"`
	Username     string `json:"username,omitempty"`
	Email        string `json:"email,omitempty"`
	SessionToken string `json:"session_token,omitempty"`
	DeviceID     string `json:"device_id,omitempty"`
	DeviceToken  string `json:"device_token,omitempty"`
	LastSeq      int64  `json:"last_seq,omitempty"`
}

type accountUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Email    string `json:"email"`
}

type accountDevice struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	LastSeenAt int64  `json:"last_seen_at"`
	CreatedAt  int64  `json:"created_at"`
}

type accountAuthResponse struct {
	User         accountUser     `json:"user"`
	SessionToken string          `json:"session_token"`
	ExpiresAt    json.RawMessage `json:"expires_at"`
}

type accountDeviceResponse struct {
	Device      accountDevice `json:"device"`
	DeviceToken string        `json:"device_token"`
}

type accountSourceDevice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type accountMessage struct {
	Seq             int64               `json:"seq"`
	ClientMessageID string              `json:"client_message_id"`
	Sender          string              `json:"sender"`
	Body            string              `json:"body"`
	ReceivedAt      int64               `json:"received_at"`
	CreatedAt       int64               `json:"created_at"`
	SourceDevice    accountSourceDevice `json:"source_device"`
}

type accountMessagesResponse struct {
	Messages []accountMessage `json:"messages"`
	NextSeq  int64            `json:"next_seq"`
}

type accountStatusSnapshot struct {
	State    string
	Detail   string
	Backoff  time.Duration
	LastSeq  int64
	DeviceID string
	LoggedIn bool
	// LastSuccessAt and FailingSince are read-only bookkeeping for the status
	// model (DESIGN.md 同步状态模型). setState maintains them from State; they
	// never influence polling or backoff.
	LastSuccessAt time.Time
	FailingSince  time.Time
}

type accountHTTPError struct {
	Status  int
	Message string
}

func (e *accountHTTPError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("account API returned HTTP %d", e.Status)
	}
	return fmt.Sprintf("account API returned HTTP %d: %s", e.Status, e.Message)
}

func accountStatusText(snapshot accountStatusSnapshot, credentials AccountCredentials) string {
	if snapshot.State != "" {
		if snapshot.Detail != "" {
			return snapshot.State + " · " + snapshot.Detail
		}
		return snapshot.State
	}
	if credentials.SessionToken == "" {
		return "未登录"
	}
	if credentials.DeviceToken == "" {
		return "账号已登录 · 尚未注册本机"
	}
	return "已连接 · 游标 " + fmt.Sprint(credentials.LastSeq)
}

func nextAccountErrorDelay(current time.Duration) time.Duration {
	if current < accountPollInterval {
		return accountPollInterval
	}
	next := current * 2
	if next > accountMaxBackoff {
		return accountMaxBackoff
	}
	return next
}

type accountClient struct {
	app         *App
	httpClient  *http.Client
	stopCh      chan struct{}
	doneCh      chan struct{}
	stopOnce    sync.Once
	operationMu sync.Mutex
	stateMu     sync.RWMutex
	state       accountStatusSnapshot
	devices     []accountDevice
}

func newAccountClient(app *App) *accountClient {
	state := accountStatusSnapshot{State: "未登录", Backoff: accountPollInterval}
	if app != nil {
		credentials := app.accountCredentials()
		state.LastSeq = credentials.LastSeq
		state.DeviceID = credentials.DeviceID
		state.LoggedIn = credentials.SessionToken != "" || credentials.UserID != ""
		if credentials.SessionToken != "" {
			state.State = "账号已登录"
			state.Detail = accountStatusDetail(credentials)
		}
		if credentials.DeviceToken != "" {
			state.State = "已连接"
			// Seed the delay timer with the start time so the first 3 s before the
			// initial poll are not reported as 延迟 and a dead server is noticed.
			state.LastSuccessAt = time.Now()
		}
	}
	return &accountClient{
		app:        app,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
		state:      state,
	}
}

func (c *accountClient) run(ctx context.Context) {
	defer close(c.doneCh)
	delay := accountPollInterval
	for {
		if err := c.wait(ctx, delay); err != nil {
			return
		}
		if c.app == nil || c.app.isClosing() {
			return
		}
		credentials := c.app.accountCredentials()
		if credentials.DeviceToken == "" {
			state := "未登录"
			if credentials.SessionToken != "" {
				state = "账号已登录"
			}
			previous := c.snapshot()
			if previous.State == "需要重新登录" {
				state = previous.State
			}
			c.setState(accountStatusSnapshot{
				State:    state,
				Detail:   accountStatusDetail(credentials),
				Backoff:  accountPollInterval,
				LastSeq:  credentials.LastSeq,
				DeviceID: credentials.DeviceID,
				LoggedIn: credentials.SessionToken != "" || credentials.UserID != "",
			})
			delay = accountPollInterval
			continue
		}
		err := c.pollOnce(ctx)
		if err == nil {
			delay = accountPollInterval
			continue
		}
		if errors.Is(err, context.Canceled) || c.app.isClosing() {
			return
		}
		if isAccountUnauthorized(err) {
			delay = accountMaxBackoff
			continue
		}
		delay = nextAccountErrorDelay(delay)
		credentials = c.app.accountCredentials()
		c.setState(accountStatusSnapshot{
			State:    "云同步重试中",
			Detail:   err.Error(),
			Backoff:  delay,
			LastSeq:  credentials.LastSeq,
			DeviceID: credentials.DeviceID,
			LoggedIn: credentials.SessionToken != "" || credentials.UserID != "",
		})
	}
}

func (c *accountClient) wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.stopCh:
		return context.Canceled
	case <-timer.C:
		return nil
	}
}

func (c *accountClient) stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() { close(c.stopCh) })
	<-c.doneCh
}

func (c *accountClient) snapshot() accountStatusSnapshot {
	if c == nil {
		return accountStatusSnapshot{State: "未登录", Backoff: accountPollInterval}
	}
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.state
}

func (c *accountClient) setState(state accountStatusSnapshot) {
	if state.Backoff <= 0 {
		state.Backoff = accountPollInterval
	}
	now := time.Now()
	c.stateMu.Lock()
	previous := c.state
	switch state.State {
	case "已连接":
		state.LastSuccessAt = now
	case "云同步重试中":
		state.LastSuccessAt = previous.LastSuccessAt
		state.FailingSince = previous.FailingSince
		if state.FailingSince.IsZero() {
			state.FailingSince = now
		}
	default:
		// Not polling (logged out, no device) or an auth failure, which DESIGN.md
		// judges separately from "continuously unreachable".
		state.LastSuccessAt = previous.LastSuccessAt
	}
	c.state = state
	c.stateMu.Unlock()
}

func (c *accountClient) devicesSnapshot() []accountDevice {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return append([]accountDevice(nil), c.devices...)
}

func (c *accountClient) setDevices(devices []accountDevice) {
	c.stateMu.Lock()
	c.devices = append([]accountDevice(nil), devices...)
	c.stateMu.Unlock()
}

func accountStatusDetail(credentials AccountCredentials) string {
	if credentials.UserID != "" {
		if credentials.Username != "" {
			return credentials.Username
		}
		return credentials.Email
	}
	return ""
}

func (a *App) accountCredentials() AccountCredentials {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.Account
}

func (a *App) accountClient() *accountClient {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.account
}

func (a *App) accountAPIURL() string {
	credentials := a.accountCredentials()
	if strings.TrimSpace(credentials.APIURL) == "" {
		return defaultAccountAPIURL
	}
	return strings.TrimSpace(credentials.APIURL)
}

func (a *App) setAccountCredentials(credentials AccountCredentials) error {
	return a.updateConfig(func(cfg *Config) { cfg.Account = credentials })
}

func (a *App) setAccountLastSeq(deviceID, deviceToken string, seq int64) error {
	if seq <= 0 {
		return errors.New("account message sequence is invalid")
	}
	credentialsChanged := false
	err := a.updateConfig(func(cfg *Config) {
		if cfg.Account.DeviceID != deviceID || cfg.Account.DeviceToken != deviceToken {
			credentialsChanged = true
			return
		}
		if seq > cfg.Account.LastSeq {
			cfg.Account.LastSeq = seq
		}
	})
	if err != nil {
		return err
	}
	if credentialsChanged {
		return errors.New("account credentials changed while saving cursor")
	}
	return nil
}

func (c *accountClient) login(ctx context.Context, identifier, password string) error {
	return c.authenticate(ctx, "/api/v1/auth/login", map[string]string{
		"identifier": strings.TrimSpace(identifier),
		"password":   password,
	})
}

func (c *accountClient) register(ctx context.Context, username, email, password string) error {
	return c.authenticate(ctx, "/api/v1/auth/register", map[string]string{
		"username": strings.TrimSpace(username),
		"email":    strings.TrimSpace(email),
		"password": password,
	})
}

func (c *accountClient) authenticate(ctx context.Context, path string, payload map[string]string) error {
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	if strings.TrimSpace(payload["password"]) == "" {
		return errors.New("密码不能为空")
	}
	var response accountAuthResponse
	if err := c.requestJSON(ctx, http.MethodPost, path, payload, "", &response); err != nil {
		c.setState(accountStatusSnapshot{State: "登录失败", Detail: accountErrorDetail(err), Backoff: accountPollInterval})
		return err
	}
	if response.SessionToken == "" || response.User.ID == "" {
		return errors.New("账号 API 登录响应缺少 user 或 session_token")
	}
	old := c.app.accountCredentials()
	credentials := AccountCredentials{
		APIURL:       c.app.accountAPIURL(),
		UserID:       response.User.ID,
		Username:     response.User.Username,
		Email:        response.User.Email,
		SessionToken: response.SessionToken,
	}
	if old.UserID == credentials.UserID {
		credentials.DeviceID = old.DeviceID
		credentials.DeviceToken = old.DeviceToken
		credentials.LastSeq = old.LastSeq
	}
	if err := c.app.setAccountCredentials(credentials); err != nil {
		return fmt.Errorf("保存账号登录状态失败: %w", err)
	}
	if err := c.ensureDevice(ctx); err != nil {
		if !isAccountUnauthorized(err) {
			c.setState(accountStatusSnapshot{State: "账号已登录", Detail: err.Error(), Backoff: accountPollInterval, LastSeq: credentials.LastSeq, LoggedIn: true})
		}
		return err
	}
	if err := c.refreshDevicesWithoutLock(ctx); err != nil {
		if isAccountUnauthorized(err) {
			c.markUnauthorized()
		} else {
			// Device registration already succeeded. A transient device-list
			// error should not turn a successful login into a failed login.
			current := c.app.accountCredentials()
			c.setState(accountStatusSnapshot{State: "已连接", Detail: "设备列表暂不可用", Backoff: accountPollInterval, LastSeq: current.LastSeq, DeviceID: current.DeviceID, LoggedIn: true})
		}
	}
	return nil
}

func (c *accountClient) ensureDevice(ctx context.Context) error {
	credentials := c.app.accountCredentials()
	if credentials.DeviceID != "" && credentials.DeviceToken != "" {
		c.setState(accountStatusSnapshot{State: "已连接", Detail: accountStatusDetail(credentials), Backoff: accountPollInterval, LastSeq: credentials.LastSeq, DeviceID: credentials.DeviceID, LoggedIn: true})
		return nil
	}
	if credentials.SessionToken == "" {
		return errors.New("请先登录 MsgDock 账号")
	}
	name, _ := os.Hostname()
	if strings.TrimSpace(name) == "" {
		name = "Windows"
	}
	payload := struct {
		ID   string `json:"id,omitempty"`
		Name string `json:"name"`
		Type string `json:"type"`
	}{ID: credentials.DeviceID, Name: name, Type: "windows"}
	var response accountDeviceResponse
	if err := c.requestJSON(ctx, http.MethodPost, "/api/v1/devices", payload, credentials.SessionToken, &response); err != nil {
		if isAccountUnauthorized(err) {
			c.markUnauthorized()
		}
		return err
	}
	if response.Device.ID == "" || response.DeviceToken == "" {
		return errors.New("设备注册响应缺少 device 或 device_token")
	}
	credentials.DeviceID = response.Device.ID
	credentials.DeviceToken = response.DeviceToken
	if err := c.app.setAccountCredentials(credentials); err != nil {
		return fmt.Errorf("保存设备令牌失败: %w", err)
	}
	c.setState(accountStatusSnapshot{State: "已连接", Detail: accountStatusDetail(credentials), Backoff: accountPollInterval, LastSeq: credentials.LastSeq, DeviceID: credentials.DeviceID, LoggedIn: true})
	return nil
}

func (c *accountClient) logout(ctx context.Context) error {
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	credentials := c.app.accountCredentials()
	var requestErr error
	if credentials.SessionToken != "" {
		requestErr = c.requestJSON(ctx, http.MethodPost, "/api/v1/auth/logout", nil, credentials.SessionToken, nil)
	}
	clean := AccountCredentials{APIURL: c.app.accountAPIURL()}
	if err := c.app.setAccountCredentials(clean); err != nil {
		return fmt.Errorf("保存退出状态失败: %w", err)
	}
	c.setDevices(nil)
	c.setState(accountStatusSnapshot{State: "未登录", Backoff: accountPollInterval})
	return requestErr
}

func (c *accountClient) refreshDevices(ctx context.Context) error {
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	credentials := c.app.accountCredentials()
	if credentials.SessionToken == "" {
		return errors.New("请先登录 MsgDock 账号")
	}
	var response struct {
		Devices []accountDevice `json:"devices"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, "/api/v1/devices", nil, credentials.SessionToken, &response); err != nil {
		if isAccountUnauthorized(err) {
			c.markUnauthorized()
		}
		return err
	}
	c.setDevices(response.Devices)
	return nil
}

func (c *accountClient) removeDevice(ctx context.Context, id string) error {
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("设备 ID 不能为空")
	}
	credentials := c.app.accountCredentials()
	if credentials.SessionToken == "" {
		return errors.New("请先登录 MsgDock 账号")
	}
	path := "/api/v1/devices/" + url.PathEscape(id)
	if err := c.requestJSON(ctx, http.MethodDelete, path, nil, credentials.SessionToken, nil); err != nil {
		if isAccountUnauthorized(err) {
			c.markUnauthorized()
		}
		return err
	}
	if id == credentials.DeviceID {
		credentials.DeviceID = ""
		credentials.DeviceToken = ""
		if err := c.app.setAccountCredentials(credentials); err != nil {
			return err
		}
		c.setState(accountStatusSnapshot{State: "账号已登录", Detail: "本机设备已移除", Backoff: accountPollInterval, LastSeq: credentials.LastSeq, LoggedIn: true})
	}
	return c.refreshDevicesWithoutLock(ctx)
}

func (c *accountClient) refreshDevicesWithoutLock(ctx context.Context) error {
	credentials := c.app.accountCredentials()
	if credentials.SessionToken == "" {
		return nil
	}
	var response struct {
		Devices []accountDevice `json:"devices"`
	}
	if err := c.requestJSON(ctx, http.MethodGet, "/api/v1/devices", nil, credentials.SessionToken, &response); err != nil {
		return err
	}
	c.setDevices(response.Devices)
	return nil
}

func (c *accountClient) pollOnce(ctx context.Context) error {
	c.operationMu.Lock()
	defer c.operationMu.Unlock()
	credentials := c.app.accountCredentials()
	if credentials.DeviceToken == "" {
		return nil
	}
	query := url.Values{}
	query.Set("after", fmt.Sprint(credentials.LastSeq))
	query.Set("limit", fmt.Sprint(accountMessageLimit))
	var response accountMessagesResponse
	if err := c.requestJSON(ctx, http.MethodGet, "/api/v1/messages?"+query.Encode(), nil, credentials.DeviceToken, &response); err != nil {
		if isAccountUnauthorized(err) {
			c.markUnauthorized()
		}
		return err
	}
	lastSeq := credentials.LastSeq
	for _, message := range response.Messages {
		if message.Seq > lastSeq {
			c.app.beginSending()
			defer c.app.endSending()
			break
		}
	}
	for _, message := range response.Messages {
		if message.Seq <= lastSeq {
			continue
		}
		sms, err := message.toSMS()
		if err != nil {
			return err
		}
		if err := c.app.processAccountMessage(sms); err != nil {
			return fmt.Errorf("处理云端短信 seq=%d: %w", message.Seq, err)
		}
		if err := c.app.setAccountLastSeq(credentials.DeviceID, credentials.DeviceToken, message.Seq); err != nil {
			return fmt.Errorf("保存云端游标 seq=%d: %w", message.Seq, err)
		}
		lastSeq = message.Seq
		// History is durable and supplies cross-path de-duplication. Removing
		// the completed pending item after the cursor commit keeps the pending
		// file small while preserving crash recovery before that commit.
		pending, found, err := c.app.findPendingNotification(sms.ID)
		if err != nil {
			return fmt.Errorf("读取云端待通知: %w", err)
		}
		if found && pending.Source == "account" {
			if err := c.app.removePendingNotification(sms.ID); err != nil {
				return fmt.Errorf("清理云端待通知: %w", err)
			}
		}
	}
	c.setState(accountStatusSnapshot{State: "已连接", Detail: accountStatusDetail(c.app.accountCredentials()), Backoff: accountPollInterval, LastSeq: lastSeq, DeviceID: credentials.DeviceID, LoggedIn: true})
	return nil
}

func (message accountMessage) toSMS() (SMS, error) {
	if message.Seq <= 0 || strings.TrimSpace(message.ClientMessageID) == "" {
		return SMS{}, errors.New("云端短信缺少有效 seq 或 client_message_id")
	}
	receivedAt := message.ReceivedAt
	if receivedAt <= 0 {
		receivedAt = message.CreatedAt
	}
	if receivedAt <= 0 {
		receivedAt = time.Now().UnixMilli()
	}
	device := message.SourceDevice.Name
	if device == "" {
		device = message.SourceDevice.Type
	}
	sender := message.Sender
	if sender == "" {
		sender = "短信"
	}
	return SMS{ID: message.ClientMessageID, From: sender, Text: message.Body, ReceivedAt: receivedAt, Device: device}, nil
}

func (c *accountClient) markUnauthorized() {
	credentials := c.app.accountCredentials()
	credentials.DeviceToken = ""
	_ = c.app.setAccountCredentials(credentials)
	c.setState(accountStatusSnapshot{State: "需要重新登录", Detail: "账号或设备令牌已失效", Backoff: accountMaxBackoff, LastSeq: credentials.LastSeq, LoggedIn: credentials.SessionToken != "" || credentials.UserID != ""})
}

func isAccountUnauthorized(err error) bool {
	var apiErr *accountHTTPError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized
}

func accountErrorDetail(err error) string {
	var apiErr *accountHTTPError
	if errors.As(err, &apiErr) && apiErr.Message != "" {
		return apiErr.Message
	}
	return err.Error()
}

func (c *accountClient) requestJSON(ctx context.Context, method, path string, payload any, bearer string, result any) error {
	base, err := parseAccountAPIURL(c.app.accountAPIURL())
	if err != nil {
		return err
	}
	if !strings.HasPrefix(path, "/api/v1/") {
		return errors.New("account API path must start with /api/v1/")
	}
	endpoint := *base
	endpoint.Path = strings.TrimSuffix(base.Path, "/") + path
	endpoint.RawQuery = ""
	if queryIndex := strings.IndexByte(endpoint.Path, '?'); queryIndex >= 0 {
		endpoint.RawQuery = endpoint.Path[queryIndex+1:]
		endpoint.Path = endpoint.Path[:queryIndex]
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode account API request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-MsgDock-Client", "native")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	client := c.httpClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if readErr != nil {
		return readErr
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return decodeAccountHTTPError(response.StatusCode, data)
	}
	if result == nil || response.StatusCode == http.StatusNoContent || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, result); err != nil {
		return fmt.Errorf("decode account API response: %w", err)
	}
	return nil
}

func decodeAccountHTTPError(status int, data []byte) error {
	message := ""
	var payload struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &payload) == nil {
		message = strings.TrimSpace(payload.Error)
		if message == "" {
			message = strings.TrimSpace(payload.Message)
		}
	}
	if len(message) > 256 {
		message = message[:256]
	}
	return &accountHTTPError{Status: status, Message: message}
}

func parseAccountAPIURL(raw string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("MsgDock API 必须是无查询参数的 HTTPS 地址")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, errors.New("MsgDock API 地址不能包含路径")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	return parsed, nil
}
