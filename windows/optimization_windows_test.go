//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func installTestNotificationWorker(t *testing.T, app *App, push func(SMS) error) {
	t.Helper()
	worker := newNotificationWorker(push)
	app.mu.Lock()
	app.notify = worker
	app.mu.Unlock()
	go worker.run()
	t.Cleanup(func() {
		app.mu.Lock()
		if app.notify == worker {
			app.notify = nil
		}
		app.mu.Unlock()
		worker.shutdown()
	})
}

func TestHasTrayArgumentOnlyMatchesExactFlag(t *testing.T) {
	if !hasTrayArgument([]string{"--tray"}) {
		t.Fatal("--tray was not recognized")
	}
	if hasTrayArgument([]string{"--tray=false", "tray"}) {
		t.Fatal("non-exact tray argument was recognized")
	}
}

func TestSixDigitsPropagatesRandomReadError(t *testing.T) {
	want := errors.New("random source unavailable")
	original := secureRandomRead
	secureRandomRead = func([]byte) (int, error) { return 0, want }
	t.Cleanup(func() { secureRandomRead = original })
	if _, err := sixDigits(); !errors.Is(err, want) {
		t.Fatalf("sixDigits error = %v, want %v", err, want)
	}
}

func TestConfigUpdateKeepsMemoryWhenSaveFails(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	old := Config{RelayURL: defaultRelayURL, Cloud: CloudCredentials{RelayURL: defaultRelayURL, Token: "old"}}
	app := &App{dir: blocked, cfg: old}
	if err := app.setRelayURL("https://new.example"); err == nil {
		t.Fatal("setRelayURL unexpectedly succeeded")
	}
	if got := app.cfg; got != old {
		t.Fatalf("failed config update changed memory: %#v, want %#v", got, old)
	}
}

func TestParseRelayURLMatchesAndroidRestrictions(t *testing.T) {
	valid := []string{
		"https://example.test",
		"https://example.test/",
		"HTTPS://example.test",
	}
	for _, value := range valid {
		if parsed, err := parseRelayURL(value); err != nil || parsed == nil {
			t.Fatalf("parseRelayURL(%q) = %v, want valid", value, err)
		}
	}
	invalid := []string{
		"http://example.test",
		"https://user:pass@example.test",
		"https://example.test?token=secret",
		"https://example.test?",
		"https://example.test#fragment",
		"https://example.test/api",
	}
	for _, value := range invalid {
		if _, err := parseRelayURL(value); err == nil {
			t.Errorf("parseRelayURL(%q) unexpectedly accepted", value)
		}
	}
}

func TestCloudPollDelayPolicy(t *testing.T) {
	idle := []struct {
		current time.Duration
		want    time.Duration
	}{
		{cloudPollInterval, cloudIdleInterval},
		{cloudIdleInterval, cloudIdleMax},
		{cloudIdleMax, cloudIdleMax},
	}
	for _, test := range idle {
		if got := nextIdlePollDelay(test.current); got != test.want {
			t.Errorf("nextIdlePollDelay(%s) = %s, want %s", test.current, got, test.want)
		}
	}
	if got := nextErrorPollDelay(cloudPollInterval); got != 10*time.Second {
		t.Fatalf("first error delay = %s, want 10s", got)
	}
	if got := nextErrorPollDelay(3 * time.Minute); got != cloudMaxBackoff {
		t.Fatalf("capped error delay = %s, want %s", got, cloudMaxBackoff)
	}
	if got := nextErrorPollDelay(cloudMaxBackoff); got != cloudMaxBackoff {
		t.Fatalf("max error delay = %s, want %s", got, cloudMaxBackoff)
	}
}

func TestCloudStatusStartsWithActualPollInterval(t *testing.T) {
	client := newCloudClient(&App{cfg: Config{Cloud: CloudCredentials{}}})
	if got := client.snapshot().Backoff; got != cloudPollInterval {
		t.Fatalf("initial cloud interval = %s, want %s", got, cloudPollInterval)
	}
}

func TestParseRelayURLDoesNotMutateURLQueryDefaults(t *testing.T) {
	parsed, err := parseRelayURL(" https://example.test/ ")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		t.Fatalf("parsed relay URL = %#v", parsed)
	}
	if _, err := url.Parse(parsed.String()); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationWorkerReturnsPushError(t *testing.T) {
	want := errors.New("toast unavailable")
	worker := newNotificationWorker(func(SMS) error { return want })
	go worker.run()
	t.Cleanup(worker.shutdown)
	if err := worker.request(SMS{ID: "worker-error"}); !errors.Is(err, want) {
		t.Fatalf("worker request error = %v, want %v", err, want)
	}
}

func TestNotificationWorkerShutdownWaitsForInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	worker := newNotificationWorker(func(SMS) error {
		close(started)
		<-release
		return nil
	})
	go worker.run()
	requestDone := make(chan error, 1)
	go func() { requestDone <- worker.request(SMS{ID: "in-flight"}) }()
	<-started
	shutdownDone := make(chan struct{})
	go func() {
		worker.shutdown()
		close(shutdownDone)
	}()
	close(release)
	if err := <-requestDone; err != nil {
		t.Fatalf("in-flight request failed during shutdown: %v", err)
	}
	<-shutdownDone
	if err := worker.request(SMS{ID: "after-stop"}); !errors.Is(err, errNotificationWorkerStopped) {
		t.Fatalf("request after shutdown = %v, want %v", err, errNotificationWorkerStopped)
	}
}

func TestPendingLANNotificationReplaysAfterRestart(t *testing.T) {
	dir := t.TempDir()
	sms := SMS{ID: "lan-replay", From: "10086", Text: "验证码 123456", ReceivedAt: 1787400001000}
	first := &App{cfg: Config{PairCode: "123456"}, dir: dir, seenIDs: make(map[string]struct{})}
	installTestNotificationWorker(t, first, func(SMS) error { return errors.New("toast failed") })
	if err := first.processLANMessage(sms); err == nil {
		t.Fatal("failed LAN notification unexpectedly succeeded")
	}
	pending, err := first.pendingNotificationsSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].NotifiedAt != 0 {
		t.Fatalf("pending after toast failure = %#v", pending)
	}
	if !first.hasHistoryID(sms.ID) {
		t.Fatal("history was not persisted before notification failure")
	}

	second := &App{cfg: Config{PairCode: "123456"}, dir: dir, seenIDs: make(map[string]struct{})}
	if err := second.loadHistory(); err != nil {
		t.Fatal(err)
	}
	if err := second.loadPendingNotifications(); err != nil {
		t.Fatal(err)
	}
	pushes := 0
	installTestNotificationWorker(t, second, func(SMS) error {
		pushes++
		return nil
	})
	if err := second.processLANMessage(sms); err != nil {
		t.Fatal(err)
	}
	if pushes != 1 {
		t.Fatalf("replayed push count = %d, want 1", pushes)
	}
	pending, err = second.pendingNotificationsSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].NotifiedAt == 0 {
		t.Fatalf("LAN notification ledger = %#v", pending)
	}
}

func TestCloudPendingDoesNotAckWhenToastFails(t *testing.T) {
	ackCount := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ack" {
			http.NotFound(w, r)
			return
		}
		ackCount++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	dir := t.TempDir()
	app := &App{cfg: Config{RelayURL: server.URL}, dir: dir, seenIDs: make(map[string]struct{})}
	pending := pendingNotification{
		ID: "cloud-pending", SMS: SMS{ID: "cloud-pending", From: "10086", Text: "验证码 123456", ReceivedAt: 1787400001000}, Source: "cloud",
		CloudAck: &pendingCloudAck{RelayURL: server.URL, RoomID: "room", DeviceID: "win", Token: "token"},
	}
	if err := app.addPendingNotification(pending); err != nil {
		t.Fatal(err)
	}
	installTestNotificationWorker(t, app, func(SMS) error { return errors.New("toast failed") })
	client := newCloudClient(app)
	client.httpClient = server.Client()
	if err := client.replayPending(context.Background()); err == nil {
		t.Fatal("toast failure unexpectedly replayed successfully")
	}
	if ackCount != 0 {
		t.Fatalf("ACK count after toast failure = %d, want 0", ackCount)
	}
	stored, err := app.pendingNotificationsSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].NotifiedAt != 0 {
		t.Fatalf("pending after toast failure = %#v", stored)
	}
}

func TestCloudNotifiedPendingRetriesAckWithoutToast(t *testing.T) {
	ackCount := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ack" {
			http.NotFound(w, r)
			return
		}
		ackCount++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	dir := t.TempDir()
	app := &App{cfg: Config{RelayURL: server.URL}, dir: dir, seenIDs: make(map[string]struct{})}
	pending := pendingNotification{
		ID: "cloud-notified", SMS: SMS{ID: "cloud-notified", From: "10086", Text: "验证码 123456", ReceivedAt: 1787400001000}, Source: "cloud",
		CloudAck: &pendingCloudAck{RelayURL: server.URL, RoomID: "room", DeviceID: "win", Token: "token"},
	}
	if err := app.addPendingNotification(pending); err != nil {
		t.Fatal(err)
	}
	if _, err := app.markPendingNotified(pending.ID, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	pushes := 0
	installTestNotificationWorker(t, app, func(SMS) error {
		pushes++
		return nil
	})
	client := newCloudClient(app)
	client.httpClient = server.Client()
	if err := client.replayPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pushes != 0 {
		t.Fatalf("replayed toast count = %d, want 0", pushes)
	}
	if ackCount != 1 {
		t.Fatalf("ACK count = %d, want 1", ackCount)
	}
	stored, err := app.pendingNotificationsSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatalf("pending after successful ACK = %#v", stored)
	}
}

func TestPendingNotificationJSONRoundTrip(t *testing.T) {
	want := pendingNotification{ID: "json-id", SMS: SMS{ID: "json-id", Text: "text"}, Source: "lan", NotifiedAt: 42}
	data, err := json.Marshal(pendingNotificationFile{Notifications: []pendingNotification{want}})
	if err != nil {
		t.Fatal(err)
	}
	var got pendingNotificationFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Notifications) != 1 || got.Notifications[0] != want {
		t.Fatalf("pending JSON round trip = %#v, want %#v", got, want)
	}
}

func TestExpiredPairingClearFailureKeepsCredentialsAndReportsError(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer server.Close()
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	old := CloudCredentials{RelayURL: server.URL, SessionID: "session", PairCode: "123456", PairExpiresAt: 1787400000000, PrivateKey: "AQ"}
	app := &App{cfg: Config{RelayURL: server.URL, Cloud: old}, dir: blocked}
	client := newCloudClient(app)
	client.httpClient = server.Client()
	if err := client.pollOnce(context.Background()); err == nil {
		t.Fatal("expired pairing cleanup unexpectedly succeeded")
	}
	if got := app.cloudCredentials(); got != old {
		t.Fatalf("credentials changed after failed cleanup: %#v, want %#v", got, old)
	}
	if snapshot := client.snapshot(); snapshot.State != "清理失败" {
		t.Fatalf("cleanup failure state = %#v", snapshot)
	}
}

func TestPendingNotificationRetentionPrunesOldNotifiedRecords(t *testing.T) {
	now := time.UnixMilli(1787400000000)
	old := pendingNotification{ID: "old", SMS: SMS{ID: "old"}, Source: "lan", NotifiedAt: now.Add(-pendingNotificationRetention - time.Second).UnixMilli()}
	current := pendingNotification{ID: "current", SMS: SMS{ID: "current"}, Source: "lan", NotifiedAt: now.Add(-time.Second).UnixMilli()}
	kept := prunePendingNotifications([]pendingNotification{old, current}, now)
	if len(kept) != 1 || kept[0].ID != current.ID {
		t.Fatalf("pruned pending records = %#v", kept)
	}
}

func TestReplayPendingKeepsNotifiedLANLedgerWithoutRepushing(t *testing.T) {
	dir := t.TempDir()
	app := &App{cfg: Config{RelayURL: defaultRelayURL}, dir: dir, seenIDs: make(map[string]struct{})}
	pending := pendingNotification{
		ID: "lan-ledger", SMS: SMS{ID: "lan-ledger", From: "10086", Text: "验证码 123456", ReceivedAt: 1787400001000},
		Source: "lan", NotifiedAt: time.Now().UnixMilli(),
	}
	if err := app.addPendingNotification(pending); err != nil {
		t.Fatal(err)
	}
	pushes := 0
	installTestNotificationWorker(t, app, func(SMS) error {
		pushes++
		return nil
	})
	client := newCloudClient(app)
	if err := client.replayPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pushes != 0 {
		t.Fatalf("LAN ledger replay push count = %d, want 0", pushes)
	}
	stored, err := app.pendingNotificationsSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].ID != pending.ID || stored[0].NotifiedAt == 0 {
		t.Fatalf("LAN ledger after replay = %#v", stored)
	}
}

func TestInstanceHandoverSignalsThenRetriesMutexThenGivesUp(t *testing.T) {
	pauses := 0
	pause := func() { pauses++ }

	// A listening instance takes over on the first attempt without waiting.
	startup, signaled := instanceHandover(func() bool { return true }, func() bool { t.Fatal("mutex retried after signal"); return false }, 3, pause)
	if startup || !signaled || pauses != 0 {
		t.Fatalf("listening instance: startup=%v signaled=%v pauses=%d", startup, signaled, pauses)
	}

	// An instance that is still shutting down releases the mutex later; the
	// second launch must then continue starting up instead of complaining.
	attempts := 0
	startup, signaled = instanceHandover(func() bool { return false }, func() bool { attempts++; return attempts == 3 }, 5, pause)
	if !startup || signaled || attempts != 3 || pauses != 2 {
		t.Fatalf("shutting-down instance: startup=%v signaled=%v attempts=%d pauses=%d", startup, signaled, attempts, pauses)
	}

	// An old version never listens and never leaves: give up after the budget.
	pauses = 0
	startup, signaled = instanceHandover(func() bool { return false }, func() bool { return false }, 4, pause)
	if startup || signaled || pauses != 3 {
		t.Fatalf("old instance: startup=%v signaled=%v pauses=%d", startup, signaled, pauses)
	}
}

func TestTrayHintFlagPersistsAndDefaultsToUnshown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"pairCode":"123456","relayUrl":"https://example.test","trayFallbackEnabled":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadConfigFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{cfg: cfg, dir: filepath.Dir(path)}
	if app.trayHintShown() {
		t.Fatal("old config must show the hint once")
	}
	if err := app.markTrayHintShown(); err != nil {
		t.Fatal(err)
	}
	reloaded, _, err := loadConfigFiles(path)
	if err != nil || !reloaded.TrayHintShown || !reloaded.TrayFallbackEnabled || reloaded.PairCode != "123456" {
		t.Fatalf("reloaded = %+v err=%v", reloaded, err)
	}
}

func TestEmptyInboxTextPointsToPairCodeOnlyWithoutRemotePath(t *testing.T) {
	lanOnly := emptyInboxText(false)
	if !strings.Contains(lanOnly, "局域网配对码") || !strings.Contains(lanOnly, "添加接收端") {
		t.Fatalf("LAN-only empty state = %q", lanOnly)
	}
	remote := emptyInboxText(true)
	if strings.Contains(remote, "配对码") || !strings.Contains(remote, "Windows 通知") {
		t.Fatalf("remote empty state = %q", remote)
	}
}

func TestShutdownContextCancelsUserRequestsOnClose(t *testing.T) {
	app := &App{dir: t.TempDir()}
	if app.shutdownContext().Err() != nil {
		t.Fatal("test-built App must never be cancelled")
	}
	app.shutdownCtx, app.cancelShutdown = context.WithCancel(context.Background())
	app.beginClosing()
	if !app.isClosing() || !errors.Is(app.shutdownContext().Err(), context.Canceled) {
		t.Fatal("beginClosing must cancel user-initiated requests")
	}
}

func TestNotificationWorkerProbeDoesNotPushAndSurvivesMissingHook(t *testing.T) {
	pushes, probes := 0, 0
	worker := newNotificationWorker(func(SMS) error { pushes++; return nil })
	go worker.run()
	t.Cleanup(worker.shutdown)
	if err := worker.probeSetting(); err != nil || pushes != 0 {
		t.Fatalf("probe without hook: err=%v pushes=%d", err, pushes)
	}
	worker.probe = func() error { probes++; return errors.New("setting unavailable") }
	if err := worker.probeSetting(); err == nil || probes != 1 || pushes != 0 {
		t.Fatalf("probe with hook: err=%v probes=%d pushes=%d", err, probes, pushes)
	}
	app := &App{dir: t.TempDir()}
	app.probeNotificationSetting() // no worker installed: must be a no-op
	app.mu.Lock()
	app.notify = worker
	app.mu.Unlock()
	app.probeNotificationSetting()
	if probes != 2 {
		t.Fatalf("app probe count = %d, want 2", probes)
	}
}
