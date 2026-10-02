//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	cloudPollInterval = 5 * time.Second
	cloudIdleInterval = 10 * time.Second
	cloudIdleMax      = 15 * time.Second
	cloudMaxBackoff   = 5 * time.Minute
)

// CloudCredentials are deliberately stored in the per-user config file with
// mode 0600. The private key is needed after a restart to derive the same
// room key; it is never sent to the relay.
type CloudCredentials struct {
	RelayURL      string `json:"relayUrl,omitempty"`
	SessionID     string `json:"sessionId,omitempty"`
	PairCode      string `json:"pairCode,omitempty"`
	PairExpiresAt int64  `json:"pairExpiresAt,omitempty"`
	LastPairCode  string `json:"lastPairCode,omitempty"`
	PairedAt      int64  `json:"pairedAt,omitempty"`
	RoomID        string `json:"roomId,omitempty"`
	DeviceID      string `json:"deviceId,omitempty"`
	Token         string `json:"token,omitempty"`
	PeerDeviceID  string `json:"peerDeviceId,omitempty"`
	PeerPublicKey string `json:"peerPublicKey,omitempty"`
	PrivateKey    string `json:"privateKey,omitempty"`
}

type cloudPairStartRequest struct {
	DeviceName string `json:"deviceName"`
	DeviceType string `json:"deviceType"`
	PublicKey  string `json:"publicKey"`
}

type cloudPairStartResponse struct {
	SessionID string `json:"sessionId"`
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expiresAt"`
}

type cloudPairResult struct {
	RoomID        string `json:"roomId"`
	DeviceID      string `json:"deviceId"`
	Token         string `json:"token"`
	PeerDeviceID  string `json:"peerDeviceId"`
	PeerPublicKey string `json:"peerPublicKey"`
}

type cloudEnvelope struct {
	RoomID         string `json:"roomId"`
	SenderDeviceID string `json:"senderDeviceId"`
	TargetDeviceID string `json:"targetDeviceId"`
	ID             string `json:"id"`
	CreatedAt      int64  `json:"createdAt"`
	Nonce          string `json:"nonce"`
	Ciphertext     string `json:"ciphertext"`
}

type cloudMessageResponse struct {
	Messages []cloudEnvelope `json:"messages"`
}

type cloudStatusSnapshot struct {
	State         string
	Detail        string
	PairCode      string
	PairExpiresAt int64
	LastPairCode  string
	PairedAt      int64
	LastPoll      time.Time // last successful message poll; seeded with the start time when paired
	FailingSince  time.Time // first failure of the current streak; zero when the last poll succeeded
	Backoff       time.Duration
	Paired        bool
}

type cloudClient struct {
	app        *App
	httpClient *http.Client
	stopCh     chan struct{}
	doneCh     chan struct{}
	stopOnce   sync.Once
	stateMu    sync.RWMutex
	state      cloudStatusSnapshot
	pairMu     sync.Mutex
}

func newCloudClient(app *App) *cloudClient {
	creds := app.cloudCredentials()
	state := cloudPairingState(creds)
	var lastPoll time.Time
	if cloudCredentialsReady(creds) {
		lastPoll = time.Now()
	}
	return &cloudClient{
		app:        app,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		stopCh:     make(chan struct{}),
		doneCh:     make(chan struct{}),
		state: cloudStatusSnapshot{
			State: state, PairCode: creds.PairCode, PairExpiresAt: creds.PairExpiresAt,
			LastPairCode: creds.LastPairCode, PairedAt: creds.PairedAt,
			LastPoll: lastPoll,
			Backoff:  cloudPollInterval,
			Paired:   cloudCredentialsReady(creds),
		},
	}
}

func (c *cloudClient) run(ctx context.Context) {
	defer close(c.doneCh)
	delay := cloudPollInterval
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-c.stopCh:
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}

		// Retry durable notification work even when this client is not cloud
		// paired. This lets a transient Toast failure recover without requiring
		// an application restart or a second LAN delivery.
		if err := c.replayPending(ctx); err != nil && !c.app.isClosing() {
			log.Printf("replay pending notifications failed: %v", err)
		}
		messageCount, err := c.pollOnceWithResult(ctx)
		if err != nil {
			delay = nextErrorPollDelay(delay)
			c.setState("连接失败", err.Error(), "", delay)
			continue
		}
		if messageCount > 0 {
			delay = cloudPollInterval
		} else {
			delay = nextIdlePollDelay(delay)
		}
		c.setPollInterval(delay)
	}
}

func nextIdlePollDelay(current time.Duration) time.Duration {
	switch {
	case current < cloudPollInterval:
		return cloudPollInterval
	case current < cloudIdleInterval:
		return cloudIdleInterval
	default:
		return cloudIdleMax
	}
}

func nextErrorPollDelay(current time.Duration) time.Duration {
	if current <= 0 {
		return cloudPollInterval
	}
	if current >= cloudMaxBackoff {
		return cloudMaxBackoff
	}
	next := current * 2
	if next > cloudMaxBackoff || next < current {
		return cloudMaxBackoff
	}
	return next
}

func (c *cloudClient) stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
	<-c.doneCh
}

func (c *cloudClient) snapshot() cloudStatusSnapshot {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.state
}

func (c *cloudClient) setState(state, detail, code string, backoff time.Duration) {
	creds := c.app.cloudCredentials()
	c.stateMu.Lock()
	c.state.State = state
	c.state.Detail = detail
	if code != "" {
		c.state.PairCode = code
	} else {
		c.state.PairCode = creds.PairCode
	}
	c.state.PairExpiresAt = creds.PairExpiresAt
	c.state.LastPairCode = creds.LastPairCode
	c.state.PairedAt = creds.PairedAt
	c.state.Backoff = backoff
	c.state.Paired = cloudCredentialsReady(creds)
	switch state {
	case "已连接":
		c.state.LastPoll = time.Now()
		c.state.FailingSince = time.Time{}
	case "连接失败":
		if c.state.FailingSince.IsZero() {
			c.state.FailingSince = time.Now()
		}
	default:
		c.state.FailingSince = time.Time{}
	}
	c.stateMu.Unlock()
}

func (c *cloudClient) setPollInterval(interval time.Duration) {
	c.stateMu.Lock()
	c.state.Backoff = interval
	c.stateMu.Unlock()
}

func cloudCredentialsReady(creds CloudCredentials) bool {
	return creds.RoomID != "" && creds.DeviceID != "" && creds.Token != "" && creds.PeerDeviceID != "" && creds.PeerPublicKey != "" && creds.PrivateKey != ""
}

func cloudPairingPending(creds CloudCredentials) bool {
	return creds.SessionID != "" && validSixDigits(creds.PairCode)
}

func cloudPairingState(creds CloudCredentials) string {
	switch {
	case cloudCredentialsReady(creds):
		return "已配对"
	case cloudPairingPending(creds):
		return "等待手机确认"
	default:
		return "未配对"
	}
}

func (c *cloudClient) startPairing() error {
	return c.startPairingWithMode(false)
}

func (c *cloudClient) startPairingForced() error {
	return c.startPairingWithMode(true)
}

func (c *cloudClient) startPairingWithMode(force bool) error {
	c.pairMu.Lock()
	defer c.pairMu.Unlock()
	current := c.app.cloudCredentials()
	if cloudPairingPending(current) {
		err := errors.New("cloud pairing is already pending; wait for the current code to finish")
		c.setState("等待手机确认", "当前配对码仍在等待手机确认", current.PairCode, 0)
		return err
	}
	if cloudCredentialsReady(current) && !force {
		err := errors.New("cloud pairing is already complete; reset is not available")
		c.setState("已配对", "如需更换手机，请先确认后重新云配对", "", 0)
		return err
	}

	relay := c.app.relayURL()
	if _, err := parseRelayURL(relay); err != nil {
		c.setState("配置错误", err.Error(), "", 0)
		return err
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate pairing key: %w", err)
	}
	privateEncoded := encodeBase64URL(p256PrivateBytes(privateKey))
	publicEncoded := encodeBase64URL(elliptic.Marshal(elliptic.P256(), privateKey.PublicKey.X, privateKey.PublicKey.Y))
	host, _ := c.app.hostname()
	var response cloudPairStartResponse
	status, err := c.doJSON(c.app.shutdownContext(), http.MethodPost, "/v1/pair/start", cloudPairStartRequest{
		DeviceName: host,
		DeviceType: "windows",
		PublicKey:  publicEncoded,
	}, "", &response)
	if err != nil {
		return fmt.Errorf("pair start (%d): %w", status, err)
	}
	if response.SessionID == "" || !validSixDigits(response.Code) {
		return errors.New("relay returned an invalid pairing session")
	}
	if err := c.app.updateCloudConfig(func(creds *CloudCredentials) {
		pairedAt := creds.PairedAt
		*creds = CloudCredentials{
			RelayURL:      relay,
			SessionID:     response.SessionID,
			PairCode:      response.Code,
			PairExpiresAt: response.ExpiresAt,
			LastPairCode:  response.Code,
			PairedAt:      pairedAt,
			PrivateKey:    privateEncoded,
		}
	}); err != nil {
		return err
	}
	c.setState("等待手机确认", "请在 Android 端输入此配对码", response.Code, 0)
	return nil
}

func (c *cloudClient) pollOnce(ctx context.Context) error {
	_, err := c.pollOnceWithResult(ctx)
	return err
}

func (c *cloudClient) pollOnceWithResult(ctx context.Context) (int, error) {
	creds := c.app.cloudCredentials()
	switch {
	case cloudCredentialsReady(creds):
		return c.pollMessagesWithCount(ctx, creds)
	case cloudPairingPending(creds):
		return 0, c.pollPairStatus(ctx, creds)
	default:
		c.setState("未配对", "点击“开始云配对”以连接手机", "", 0)
		return 0, nil
	}
}

func (c *cloudClient) pollPairStatus(ctx context.Context, creds CloudCredentials) error {
	query := url.Values{}
	query.Set("sessionId", creds.SessionID)
	query.Set("code", creds.PairCode)
	var result cloudPairResult
	status, err := c.doJSON(ctx, http.MethodGet, "/v1/pair/status?"+query.Encode(), nil, "", &result)
	if status == http.StatusAccepted {
		c.setState("等待手机确认", "手机尚未完成配对", creds.PairCode, 0)
		return nil
	}
	if status == http.StatusNotFound || status == http.StatusGone {
		if clearErr := c.app.updateCloudConfig(func(next *CloudCredentials) {
			next.SessionID = ""
			next.PairCode = ""
			next.PairExpiresAt = 0
			next.PrivateKey = ""
		}); clearErr != nil {
			c.setState("清理失败", "配对已过期，但本地凭据清理失败："+clearErr.Error(), creds.PairCode, 0)
			return fmt.Errorf("clear expired pairing: %w", clearErr)
		}
		c.setState("配对已过期", "请重新开始云配对", "", 0)
		return nil
	}
	if err != nil {
		return fmt.Errorf("pair status (%d): %w", status, err)
	}
	confirmWarning, err := c.finishPairing(ctx, creds, result)
	if err != nil {
		return err
	}
	detail := "云中继已连接"
	if confirmWarning {
		detail = "云中继已连接 · 配对确认失败，凭据已保存"
	}
	c.setState("已连接", detail, "", 0)
	return nil
}

func (c *cloudClient) finishPairing(ctx context.Context, creds CloudCredentials, result cloudPairResult) (bool, error) {
	if result.RoomID == "" || result.DeviceID == "" || result.Token == "" || result.PeerDeviceID == "" || result.PeerPublicKey == "" {
		return false, errors.New("relay returned incomplete pairing credentials")
	}
	privateKey, err := decodeP256PrivateKey(creds.PrivateKey)
	if err != nil {
		return false, fmt.Errorf("decode pairing key: %w", err)
	}
	peer, err := decodeP256PublicKey(result.PeerPublicKey)
	if err != nil {
		return false, fmt.Errorf("decode peer key: %w", err)
	}
	if _, err := deriveRoomKey(privateKey, peer, result.RoomID); err != nil {
		return false, fmt.Errorf("derive room key: %w", err)
	}
	if err := c.app.updateCloudConfig(func(next *CloudCredentials) {
		next.RoomID = result.RoomID
		next.DeviceID = result.DeviceID
		next.Token = result.Token
		next.PeerDeviceID = result.PeerDeviceID
		next.PeerPublicKey = result.PeerPublicKey
		next.SessionID = ""
		next.PairCode = ""
		next.PairExpiresAt = 0
		if next.LastPairCode == "" {
			next.LastPairCode = creds.PairCode
		}
		next.PairedAt = time.Now().UnixMilli()
	}); err != nil {
		return false, fmt.Errorf("save cloud credentials: %w", err)
	}

	// The relay keeps the temporary pairing session until this confirmation.
	// Credentials are intentionally already durable at this point: a failed
	// confirm must not discard a usable room/token or interrupt message polling.
	if err := c.confirmPairing(ctx, creds.SessionID, result.Token); err != nil {
		log.Printf("cloud pairing confirm failed; credentials retained: %v", err)
		return true, nil
	}
	return false, nil
}

func (c *cloudClient) confirmPairing(ctx context.Context, sessionID, token string) error {
	payload := struct {
		SessionID string `json:"sessionId"`
	}{SessionID: sessionID}
	status, err := c.doJSON(ctx, http.MethodPost, "/v1/pair/confirm", payload, token, nil)
	if err != nil {
		return fmt.Errorf("pair confirm (%d): %w", status, err)
	}
	return nil
}

func (c *cloudClient) pollMessages(ctx context.Context, creds CloudCredentials) error {
	_, err := c.pollMessagesWithCount(ctx, creds)
	return err
}

func (c *cloudClient) pollMessagesWithCount(ctx context.Context, creds CloudCredentials) (int, error) {
	query := url.Values{}
	query.Set("roomId", creds.RoomID)
	query.Set("deviceId", creds.DeviceID)
	query.Set("limit", "20")
	var raw json.RawMessage
	status, err := c.doJSON(ctx, http.MethodGet, "/v1/messages?"+query.Encode(), nil, creds.Token, &raw)
	if err != nil {
		return 0, fmt.Errorf("messages (%d): %w", status, err)
	}
	messages, err := decodeEnvelopeList(raw)
	if err != nil {
		return 0, err
	}
	if len(messages) > 0 {
		c.app.beginSending()
		defer c.app.endSending()
	}
	ackIDs := make([]string, 0, len(messages))
	var firstErr error
	var key []byte
	keyReady := false
	c.app.pendingProcessMu.Lock()
	if c.app.isClosing() {
		c.app.pendingProcessMu.Unlock()
		return 0, errors.New("receiver is shutting down")
	}
	for _, envelope := range messages {
		if envelope.ID == "" {
			if firstErr == nil {
				firstErr = errors.New("relay returned a message without id")
			}
			continue
		}
		if envelope.RoomID != "" && envelope.RoomID != creds.RoomID {
			if firstErr == nil {
				firstErr = errors.New("relay returned a message for another room")
			}
			continue
		}
		if envelope.TargetDeviceID != "" && envelope.TargetDeviceID != creds.DeviceID {
			if firstErr == nil {
				firstErr = errors.New("relay returned a message for another device")
			}
			continue
		}
		pending, found, pendingErr := c.app.findPendingNotification(envelope.ID)
		if pendingErr != nil {
			if firstErr == nil {
				firstErr = pendingErr
			}
			continue
		}
		if !found {
			if !keyReady {
				key, err = c.roomKey(creds)
				if err != nil {
					if firstErr == nil {
						firstErr = err
					}
					continue
				}
				keyReady = true
			}
			sms, decryptErr := decryptCloudEnvelope(envelope, key)
			if decryptErr != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("decrypt message %s: %w", envelope.ID, decryptErr)
				}
				continue
			}
			sms.ID = envelope.ID
			pending = pendingNotification{
				ID: envelope.ID, SMS: sms, Source: "cloud",
				CloudAck: &pendingCloudAck{RelayURL: c.app.relayURL(), RoomID: creds.RoomID, DeviceID: creds.DeviceID, Token: creds.Token},
			}
			if err := c.app.addPendingNotification(pending); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("persist pending cloud message %s: %w", envelope.ID, err)
				}
				continue
			}
		}
		if pending.SMS.ID == "" {
			pending.SMS.ID = pending.ID
		}
		if err := c.app.completePendingNotificationLocked(pending); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("complete cloud message %s: %w", envelope.ID, err)
			}
			continue
		}
		ackIDs = append(ackIDs, envelope.ID)
	}
	c.app.pendingProcessMu.Unlock()
	if c.app.isClosing() {
		return 0, errors.New("receiver is shutting down")
	}
	if len(ackIDs) > 0 {
		if err := c.ackMessages(ctx, creds, ackIDs); err != nil {
			return 0, err
		}
		for _, id := range ackIDs {
			if err := c.app.removePendingNotification(id); err != nil {
				return 0, fmt.Errorf("clear pending cloud message %s: %w", id, err)
			}
		}
	}
	if firstErr != nil {
		return 0, firstErr
	}
	c.setState("已连接", fmt.Sprintf("云中继正常 · %d 条新消息", len(ackIDs)), "", 0)
	return len(ackIDs), nil
}

func (c *cloudClient) roomKey(creds CloudCredentials) ([]byte, error) {
	privateKey, err := decodeP256PrivateKey(creds.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}
	peer, err := decodeP256PublicKey(creds.PeerPublicKey)
	if err != nil {
		return nil, fmt.Errorf("decode peer public key: %w", err)
	}
	return deriveRoomKey(privateKey, peer, creds.RoomID)
}

func (c *cloudClient) ackMessages(ctx context.Context, creds CloudCredentials, ids []string) error {
	return c.ackMessagesWithRelay(ctx, creds, ids, "")
}

func (c *cloudClient) ackMessagesWithRelay(ctx context.Context, creds CloudCredentials, ids []string, relayURL string) error {
	payload := struct {
		RoomID   string   `json:"roomId"`
		DeviceID string   `json:"deviceId"`
		IDs      []string `json:"messageIds"`
	}{creds.RoomID, creds.DeviceID, ids}
	status, err := c.doJSONAt(ctx, relayURL, http.MethodPost, "/v1/ack", payload, creds.Token, nil)
	if err != nil {
		return fmt.Errorf("ack (%d): %w", status, err)
	}
	return nil
}

func (c *cloudClient) doJSON(ctx context.Context, method, path string, requestBody any, token string, responseBody any) (int, error) {
	return c.doJSONAt(ctx, c.app.relayURL(), method, path, requestBody, token, responseBody)
}

func (c *cloudClient) doJSONAt(ctx context.Context, relayURL, method, path string, requestBody any, token string, responseBody any) (int, error) {
	if strings.TrimSpace(relayURL) == "" {
		relayURL = c.app.relayURL()
	}
	base, err := parseRelayURL(relayURL)
	if err != nil {
		return 0, err
	}
	pathPart, queryPart, _ := strings.Cut(path, "?")
	base.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(pathPart, "/")
	base.RawQuery = queryPart
	var body io.Reader
	if requestBody != nil {
		encoded, encodeErr := json.Marshal(requestBody)
		if encodeErr != nil {
			return 0, encodeErr
		}
		body = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequestWithContext(ctx, method, base.String(), body)
	if err != nil {
		return 0, err
	}
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if readErr != nil {
		return resp.StatusCode, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(data))
		if message == "" {
			message = resp.Status
		}
		return resp.StatusCode, errors.New(message)
	}
	if responseBody != nil && len(data) > 0 {
		if err := json.Unmarshal(data, responseBody); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func parseRelayURL(value string) (*url.URL, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return nil, fmt.Errorf("invalid relay URL: %w", err)
	}
	if !strings.EqualFold(parsed.Scheme, "https") || parsed.Hostname() == "" {
		return nil, errors.New("relay URL must be an HTTPS URL")
	}
	if parsed.User != nil {
		return nil, errors.New("relay URL must not include user info")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, errors.New("relay URL must not include query or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return nil, errors.New("relay URL path must be empty or /")
	}
	return parsed, nil
}

func decodeEnvelopeList(raw json.RawMessage) ([]cloudEnvelope, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var list []cloudEnvelope
	if raw[0] == '[' {
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, fmt.Errorf("decode messages: %w", err)
		}
		return list, nil
	}
	var wrapped cloudMessageResponse
	if err := json.Unmarshal(raw, &wrapped); err != nil {
		return nil, fmt.Errorf("decode messages: %w", err)
	}
	return wrapped.Messages, nil
}

func decryptCloudEnvelope(envelope cloudEnvelope, key []byte) (SMS, error) {
	nonce, err := decodeBase64URL(envelope.Nonce)
	if err != nil {
		return SMS{}, fmt.Errorf("nonce: %w", err)
	}
	ciphertext, err := decodeBase64URL(envelope.Ciphertext)
	if err != nil {
		return SMS{}, fmt.Errorf("ciphertext: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return SMS{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return SMS{}, err
	}
	if len(nonce) != gcm.NonceSize() {
		return SMS{}, fmt.Errorf("nonce has length %d, want %d", len(nonce), gcm.NonceSize())
	}
	aad := []byte(fmt.Sprintf("xgy-sms-v2\n%s\n%s\n%s\n%s", envelope.ID, envelope.RoomID, envelope.SenderDeviceID, envelope.TargetDeviceID))
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return SMS{}, fmt.Errorf("AES-GCM authentication failed: %w", err)
	}
	var payload struct {
		Version    int    `json:"v"`
		From       string `json:"from"`
		Text       string `json:"text"`
		ReceivedAt int64  `json:"receivedAt"`
		SIM        int    `json:"sim"`
		Device     string `json:"device"`
	}
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		return SMS{}, fmt.Errorf("plaintext JSON: %w", err)
	}
	if payload.Version != 2 {
		return SMS{}, fmt.Errorf("unsupported plaintext version %d", payload.Version)
	}
	if payload.From == "" {
		payload.From = "短信"
	}
	if payload.ReceivedAt <= 0 {
		payload.ReceivedAt = envelope.CreatedAt
	}
	if payload.ReceivedAt <= 0 {
		payload.ReceivedAt = time.Now().UnixMilli()
	}
	return SMS{From: payload.From, Text: payload.Text, ReceivedAt: payload.ReceivedAt, SIM: payload.SIM, Device: payload.Device}, nil
}

func deriveRoomKey(privateKey *ecdsa.PrivateKey, peer *ecdsa.PublicKey, roomID string) ([]byte, error) {
	if privateKey == nil || peer == nil || peer.X == nil || peer.Y == nil || !elliptic.P256().IsOnCurve(peer.X, peer.Y) {
		return nil, errors.New("invalid ECDH key")
	}
	x, _ := elliptic.P256().ScalarMult(peer.X, peer.Y, p256PrivateBytes(privateKey))
	if x == nil {
		return nil, errors.New("ECDH produced an empty shared secret")
	}
	secret := make([]byte, 32)
	shared := x.Bytes()
	copy(secret[len(secret)-len(shared):], shared)
	saltSum := sha256.Sum256([]byte("xgy-sms-v2:" + roomID))
	return hkdfSHA256(secret, saltSum[:], []byte("xgy-sms-room-key"), 32), nil
}

func hkdfSHA256(secret, salt, info []byte, length int) []byte {
	if len(salt) == 0 {
		salt = make([]byte, sha256.Size)
	}
	extractMAC := hmac.New(sha256.New, salt)
	_, _ = extractMAC.Write(secret)
	prk := extractMAC.Sum(nil)
	result := make([]byte, 0, length)
	var previous []byte
	for counter := byte(1); len(result) < length; counter++ {
		expandMAC := hmac.New(sha256.New, prk)
		_, _ = expandMAC.Write(previous)
		_, _ = expandMAC.Write(info)
		_, _ = expandMAC.Write([]byte{counter})
		previous = expandMAC.Sum(nil)
		result = append(result, previous...)
	}
	return result[:length]
}

func p256PrivateBytes(key *ecdsa.PrivateKey) []byte {
	result := make([]byte, 32)
	if key == nil || key.D == nil {
		return result
	}
	bytes := key.D.Bytes()
	if len(bytes) > len(result) {
		bytes = bytes[len(bytes)-len(result):]
	}
	copy(result[len(result)-len(bytes):], bytes)
	return result
}

func decodeP256PrivateKey(value string) (*ecdsa.PrivateKey, error) {
	encoded, err := decodeBase64URL(value)
	if err != nil {
		return nil, err
	}
	if len(encoded) == 0 || len(encoded) > 32 {
		return nil, errors.New("private key must be 1 to 32 bytes")
	}
	if len(encoded) < 32 {
		padded := make([]byte, 32)
		copy(padded[32-len(encoded):], encoded)
		encoded = padded
	}
	curve := elliptic.P256()
	x, y := curve.ScalarBaseMult(encoded)
	if x == nil || y == nil {
		return nil, errors.New("invalid private key")
	}
	return &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: curve, X: x, Y: y}, D: new(big.Int).SetBytes(encoded)}, nil
}

func decodeP256PublicKey(value string) (*ecdsa.PublicKey, error) {
	encoded, err := decodeBase64URL(value)
	if err != nil {
		return nil, err
	}
	if len(encoded) != 65 || encoded[0] != 4 {
		return nil, errors.New("public key must be an uncompressed P-256 point")
	}
	x, y := elliptic.Unmarshal(elliptic.P256(), encoded)
	if x == nil || y == nil {
		return nil, errors.New("invalid public key")
	}
	return &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}, nil
}

func encodeBase64URL(value []byte) string {
	return base64.RawURLEncoding.EncodeToString(value)
}

func decodeBase64URL(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

func validSixDigits(value string) bool {
	if len(value) != 6 {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func (a *App) hostname() (string, error) {
	return os.Hostname()
}
