//go:build windows

package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/windows"
)

const (
	httpPort      = 58123
	discoveryPort = 58124
	appName       = "MsgDock"
	appVersion    = "0.7.7"
	// shutdownGrace bounds the cleanup after the user chooses 退出.
	shutdownGrace = 10 * time.Second
	// instanceHandoverAttempts × instanceHandoverPause is how long a second
	// launch waits for the running instance to either answer the show-window
	// event or release the mutex (it may still be shutting down).
	instanceHandoverAttempts = 12
	instanceHandoverPause    = 250 * time.Millisecond
	// defaultRelayURL is kept in one place so the desktop client and its
	// installer can be changed without hunting through the cloud code.
	defaultRelayURL = "https://xgy-sms-relay.xgy2021sh.workers.dev"
)

var secureRandomRead = rand.Read

type Config struct {
	PairCode            string             `json:"pairCode"`
	RelayURL            string             `json:"relayUrl"`
	Cloud               CloudCredentials   `json:"cloud"`
	Account             AccountCredentials `json:"account"`
	TrayFallbackEnabled bool               `json:"trayFallbackEnabled"`
	// NotificationPreview is "full", "sender" or "minimal"; see
	// parseNotificationPreview. Missing (configs from older versions) means full.
	NotificationPreview string `json:"notificationPreview,omitempty"`
	// TrayHintShown records that the one-time "still running in the tray"
	// balloon has been shown after the user first closed the window.
	TrayHintShown       bool `json:"trayHintShown,omitempty"`
	trayFallbackPresent bool `json:"-"`
}

// UnmarshalJSON keeps old config files compatible while recording whether the
// new setting was present so newApp can persist the enabled-by-default value.
func (c *Config) UnmarshalJSON(data []byte) error {
	type configFields struct {
		PairCode            string             `json:"pairCode"`
		RelayURL            string             `json:"relayUrl"`
		Cloud               CloudCredentials   `json:"cloud"`
		Account             AccountCredentials `json:"account"`
		TrayFallbackEnabled bool               `json:"trayFallbackEnabled"`
		NotificationPreview string             `json:"notificationPreview"`
		TrayHintShown       bool               `json:"trayHintShown"`
	}
	var fields configFields
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.PairCode = fields.PairCode
	c.RelayURL = fields.RelayURL
	c.Cloud = fields.Cloud
	c.Account = fields.Account
	c.TrayFallbackEnabled = fields.TrayFallbackEnabled
	c.NotificationPreview = fields.NotificationPreview
	c.TrayHintShown = fields.TrayHintShown
	_, c.trayFallbackPresent = raw["trayFallbackEnabled"]
	if !c.trayFallbackPresent {
		c.TrayFallbackEnabled = true
	}
	return nil
}

type SMS struct {
	ID         string `json:"id,omitempty"`
	From       string `json:"from"`
	Text       string `json:"text"`
	ReceivedAt int64  `json:"receivedAt"`
	SIM        int    `json:"sim"`
	Device     string `json:"device"`
}

type App struct {
	cfg              Config
	dir              string
	mu               sync.RWMutex
	configMu         sync.Mutex
	historyMu        sync.Mutex
	pendingMu        sync.Mutex
	pendingProcessMu sync.Mutex
	pending          []pendingNotification
	pendingLoaded    bool
	closing          atomic.Bool
	notifyFailed     atomic.Bool  // latest native Toast attempt failed; shown in the status header
	notifyDisabled   atomic.Bool  // Windows reports notifications for this app as turned off
	sending          atomic.Int32 // fetches that carried new messages, and their ACKs, currently in flight
	// shutdownCtx is cancelled by beginClosing so user-initiated requests
	// (login, pairing) stop blocking the exit instead of running to their
	// 15 s HTTP timeout. Nil means a test-constructed App; see shutdownContext.
	shutdownCtx    context.Context
	cancelShutdown context.CancelFunc
	logFile        *os.File
	recent         []SMS
	seenIDs        map[string]struct{}
	ui             *desktopUI
	server         *http.Server
	notify         *notificationWorker
	cloud          *cloudClient
	account        *accountClient
}

func main() {
	runtime.LockOSThread()
	startInTray := hasTrayArgument(os.Args[1:])

	app, err := newApp()
	if err != nil {
		showFatalError("初始化失败", err)
		return
	}
	defer app.closeLog()
	if err := refreshCurrentAutoStartRegistration(); err != nil {
		log.Printf("refresh Windows auto-start executable failed: %v", err)
	}

	instance, alreadyRunning, err := acquireSingleInstance()
	if err != nil {
		log.Printf("single instance check failed: %v", err)
	}
	if alreadyRunning {
		// The mutex may belong to a healthy instance (ask it to show its window),
		// to one that is still shutting down (wait for the mutex) or to a version
		// too old to listen (explain). Auto-start (--tray) never pops a window.
		releaseSingleInstance(instance)
		instance = 0
		startup, _ := instanceHandover(func() bool {
			return signalRunningInstance(!startInTray)
		}, func() bool {
			handle, running, err := acquireSingleInstance()
			if err == nil && !running {
				instance = handle
				return true
			}
			releaseSingleInstance(handle)
			return false
		}, instanceHandoverAttempts, func() { time.Sleep(instanceHandoverPause) })
		if !startup {
			if !startInTray {
				showAlreadyRunning()
			}
			return
		}
	}
	if instance != 0 {
		defer releaseSingleInstance(instance)
	}
	// Create the show-window event as early as possible so an impatient second
	// double-click during startup finds it instead of the "old version" dialog.
	showEvent, err := createShowInstanceEvent()
	if err != nil {
		log.Printf("create show-window event failed: %v", err)
		showEvent = 0
	} else {
		defer windows.CloseHandle(showEvent)
	}

	if err := app.startServer(); err != nil {
		showFatalError("接收服务启动失败", err)
		return
	}

	ui, err := newDesktopUI(app)
	if err != nil {
		app.stopServer()
		showFatalError("界面启动失败", err)
		return
	}
	app.mu.Lock()
	app.ui = ui
	app.mu.Unlock()
	notificationsReady := true
	if err := app.initializeNotifications(); err != nil {
		notificationsReady = false
		log.Printf("initialize Windows notifications failed: %v", err)
	} else {
		// Report muted notifications before the first SMS; the window repeats
		// the probe every settingProbeInterval.
		go app.probeNotificationSetting()
	}

	cloud := newCloudClient(app)
	app.mu.Lock()
	app.cloud = cloud
	app.mu.Unlock()
	cloudCtx, cancelCloud := context.WithCancel(context.Background())
	go cloud.run(cloudCtx)
	account := newAccountClient(app)
	app.mu.Lock()
	app.account = account
	app.mu.Unlock()
	accountCtx, cancelAccount := context.WithCancel(context.Background())
	go account.run(accountCtx)

	announceCtx, cancelAnnounce := context.WithCancel(context.Background())
	go app.announceLoop(announceCtx)
	if notificationsReady && ui.window != nil {
		ui.window.Synchronize(func() {
			go func() {
				if err := cloud.replayPending(cloudCtx); err != nil && !app.isClosing() {
					log.Printf("replay pending notifications failed: %v", err)
				}
			}()
		})
	}

	if !startInTray {
		ui.showStatus()
	}
	ui.run(showEvent)

	// A hung shutdown would keep holding the single-instance mutex with no tray
	// icon, so the next launch says "already running" although nothing is
	// visible. History is persisted before any ACK, so forcing the exit after a
	// grace period loses nothing.
	go func() {
		time.Sleep(shutdownGrace)
		log.Printf("shutdown exceeded %s; forcing exit", shutdownGrace)
		os.Exit(0)
	}()

	// Stop new inputs first (beginClosing also cancels user-initiated account
	// and pairing requests). Cloud cancellation then lets any in-flight poll
	// finish before the notification worker and native window are disposed.
	app.beginClosing()
	cancelAnnounce()
	app.stopServer()
	cancelCloud()
	cloud.stop()
	cancelAccount()
	account.stop()
	app.stopNotifications()
	app.mu.Lock()
	app.ui = nil
	app.mu.Unlock()
	ui.dispose()
}

func newApp() (*App, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "XgyLanSms")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	lf, err := os.OpenFile(filepath.Join(dir, "receiver.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err == nil {
		log.SetOutput(lf)
	}
	closeLogOnError := func() {
		if lf != nil {
			_ = lf.Close()
			lf = nil
		}
	}

	configPath := filepath.Join(dir, "config.json")
	cfg, recoveredConfig, err := loadConfigFiles(configPath)
	if err != nil {
		closeLogOnError()
		return nil, fmt.Errorf("load config: %w", err)
	}
	configDirty := recoveredConfig
	if !cfg.trayFallbackPresent {
		cfg.TrayFallbackEnabled = true
		cfg.trayFallbackPresent = true
		configDirty = true
	}
	if !validSixDigits(cfg.PairCode) {
		pairCode, err := sixDigits()
		if err != nil {
			closeLogOnError()
			return nil, fmt.Errorf("generate pair code: %w", err)
		}
		cfg.PairCode = pairCode
		configDirty = true
	}
	if strings.TrimSpace(cfg.RelayURL) == "" {
		cfg.RelayURL = defaultRelayURL
		configDirty = true
	}
	if strings.TrimSpace(cfg.Account.APIURL) == "" {
		cfg.Account.APIURL = defaultAccountAPIURL
		configDirty = true
	}
	if configDirty {
		if err := saveConfigFile(configPath, cfg); err != nil {
			closeLogOnError()
			return nil, fmt.Errorf("save config: %w", err)
		}
	}
	app := &App{cfg: cfg, dir: dir, seenIDs: make(map[string]struct{}), logFile: lf}
	app.shutdownCtx, app.cancelShutdown = context.WithCancel(context.Background())
	if err := app.loadHistory(); err != nil {
		log.Printf("load SMS history failed: %v", err)
	}
	if err := app.loadPendingNotifications(); err != nil {
		app.closeLog()
		return nil, fmt.Errorf("load pending notifications: %w", err)
	}
	return app, nil
}

func sixDigits() (string, error) {
	b := make([]byte, 4)
	if _, err := secureRandomRead(b); err != nil {
		return "", err
	}
	n := (int(b[0])<<24 | int(b[1])<<16 | int(b[2])<<8 | int(b[3])) & 0x7fffffff
	return fmt.Sprintf("%06d", 100000+n%900000), nil
}

func (a *App) pairCode() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.PairCode
}

func (a *App) relayURL() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if strings.TrimSpace(a.cfg.RelayURL) == "" {
		return defaultRelayURL
	}
	return a.cfg.RelayURL
}

func (a *App) trayFallbackEnabled() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.TrayFallbackEnabled || !a.cfg.trayFallbackPresent
}

func (a *App) setTrayFallbackEnabled(enabled bool) error {
	return a.updateConfig(func(cfg *Config) {
		cfg.TrayFallbackEnabled = enabled
		cfg.trayFallbackPresent = true
	})
}

func (a *App) trayHintShown() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.TrayHintShown
}

func (a *App) markTrayHintShown() error {
	return a.updateConfig(func(cfg *Config) { cfg.TrayHintShown = true })
}

// shutdownContext is the context for user-initiated network calls. It is
// cancelled by beginClosing; Apps built directly in tests never close.
func (a *App) shutdownContext() context.Context {
	if a.shutdownCtx == nil {
		return context.Background()
	}
	return a.shutdownCtx
}

func (a *App) cloudClient() *cloudClient {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cloud
}

// latestSMS returns the newest history entry without copying the whole list.
func (a *App) latestSMS() (SMS, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if len(a.recent) == 0 {
		return SMS{}, false
	}
	return a.recent[0], true
}

func (a *App) notificationPreview() notificationPreview {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return parseNotificationPreview(a.cfg.NotificationPreview)
}

func (a *App) setNotificationPreview(preview notificationPreview) error {
	return a.updateConfig(func(cfg *Config) {
		cfg.NotificationPreview = string(parseNotificationPreview(string(preview)))
	})
}

// beginSending/endSending are passive bookkeeping for the 同步中 state: they
// bracket the processing and ACK of a poll that returned new messages. Empty
// polls never call them, so the state does not flash every 3 seconds.
func (a *App) beginSending() { a.sending.Add(1) }

func (a *App) endSending() { a.sending.Add(-1) }

func (a *App) isSending() bool { return a.sending.Load() > 0 }

func (a *App) cloudCredentials() CloudCredentials {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.Cloud
}

func (a *App) updateCloudConfig(update func(*CloudCredentials)) error {
	return a.updateConfig(func(cfg *Config) {
		update(&cfg.Cloud)
	})
}

func (a *App) setRelayURL(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		value = defaultRelayURL
	}
	if _, err := parseRelayURL(value); err != nil {
		return err
	}
	return a.updateConfig(func(cfg *Config) {
		cfg.RelayURL = value
		cfg.Cloud.RelayURL = value
	})
}

// updateConfig serializes configuration changes and commits the candidate to
// memory only after its durable atomic replacement succeeds. This keeps a
// failed write from making the running process disagree with config.json.
func (a *App) updateConfig(update func(*Config)) error {
	a.configMu.Lock()
	defer a.configMu.Unlock()
	a.mu.RLock()
	candidate := a.cfg
	a.mu.RUnlock()
	update(&candidate)
	if err := a.saveConfig(candidate); err != nil {
		return err
	}
	a.mu.Lock()
	a.cfg = candidate
	a.mu.Unlock()
	return nil
}

func (a *App) saveConfig(cfg Config) error {
	return saveConfigFile(filepath.Join(a.dir, "config.json"), cfg)
}

func (a *App) recentSnapshot() []SMS {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return append([]SMS(nil), a.recent...)
}

func (a *App) startServer() error {
	addr := fmt.Sprintf("0.0.0.0:%d", httpPort)
	listener, err := net.Listen("tcp4", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	a.server = &http.Server{
		Addr:              addr,
		Handler:           a.handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
	go func() {
		log.Printf("listening on %s", addr)
		if err := a.server.Serve(listener); err != nil && err != http.ErrServerClosed {
			log.Printf("HTTP server stopped: %v", err)
		}
	}()
	return nil
}

func (a *App) stopServer() {
	if a.server == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.server.Shutdown(ctx); err != nil {
		log.Printf("HTTP server shutdown failed: %v", err)
	}
}

func (a *App) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.status)
	mux.HandleFunc("/sms", a.sms)
	return mux
}

func (a *App) sms(w http.ResponseWriter, r *http.Request) {
	if a.isClosing() {
		http.Error(w, "receiver is shutting down", http.StatusServiceUnavailable)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("X-Xgy-Key") != a.pairCode() {
		http.Error(w, "bad key", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var s SMS
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if s.From == "" {
		s.From = "短信"
	}
	if s.ReceivedAt <= 0 {
		s.ReceivedAt = time.Now().UnixMilli()
	}
	if s.ID == "" {
		s.ID = stableLocalSMSID(s)
	}
	if err := a.processLANMessage(s); err != nil {
		log.Printf("process LAN SMS failed: %v", err)
		http.Error(w, "notification delivery failed", http.StatusInternalServerError)
		return
	}

	log.Printf("SMS received from=%q device=%q bytes=%d", s.From, s.Device, len(s.Text))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok"))
}

func (a *App) deliverSMS(s SMS) {
	if a.isClosing() {
		return
	}
	a.mu.RLock()
	ui := a.ui
	a.mu.RUnlock()
	if ui != nil {
		ui.onSMS(s)
	}
}

func stableLocalSMSID(s SMS) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "xgy-lan-sms\n%s\n%s\n%d\n%d\n%s", s.From, s.Text, s.ReceivedAt, s.SIM, s.Device)
	return "lan-" + hex.EncodeToString(hash.Sum(nil))
}

func (a *App) isClosing() bool {
	return a.closing.Load()
}

func (a *App) beginClosing() {
	a.closing.Store(true)
	if a.cancelShutdown != nil {
		a.cancelShutdown()
	}
}

func (a *App) closeLog() {
	if a.logFile != nil {
		_ = a.logFile.Close()
		a.logFile = nil
	}
}

func (a *App) status(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	cloudState := "disabled"
	cloudPaired := false
	if cloud := a.cloudClient(); cloud != nil {
		snapshot := cloud.snapshot()
		cloudState = snapshot.State
		cloudPaired = snapshot.Paired
	}
	health := struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
		LAN     struct {
			Listening bool `json:"listening"`
			Port      int  `json:"port"`
		} `json:"lan"`
		Cloud struct {
			State  string `json:"state"`
			Paired bool   `json:"paired"`
		} `json:"cloud"`
		Account struct {
			State    string `json:"state"`
			LoggedIn bool   `json:"logged_in"`
		} `json:"account"`
	}{OK: true, Version: appVersion}
	health.LAN.Listening = true
	health.LAN.Port = httpPort
	health.Cloud.State = cloudState
	health.Cloud.Paired = cloudPaired
	if account := a.accountClient(); account != nil {
		snapshot := account.snapshot()
		health.Account.State = snapshot.State
		health.Account.LoggedIn = snapshot.LoggedIn
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(health)
}

func (a *App) announceLoop(ctx context.Context) {
	ticker := time.NewTicker(2200 * time.Millisecond)
	defer ticker.Stop()
	for {
		host, _ := os.Hostname()
		msg := fmt.Sprintf("XGY_SMS_V1|%s|%s|%d", strings.ReplaceAll(host, "|", " "), localIPv4(), httpPort)
		if err := sendLanBroadcast([]byte(msg), discoveryPort); err != nil {
			log.Printf("LAN announce failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func localIPv4() string {
	type candidate struct {
		ip    string
		score int
	}
	var best candidate
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.ToLower(i.Name)
		penalty := 0
		for _, bad := range []string{"vpn", "tun", "tap", "tailscale", "zerotier", "wsl", "virtual", "vethernet", "hyper-v"} {
			if strings.Contains(name, bad) {
				penalty -= 50
			}
		}
		bonus := 0
		for _, good := range []string{"wi-fi", "wifi", "wlan", "ethernet", "以太网", "无线"} {
			if strings.Contains(name, good) {
				bonus += 30
				break
			}
		}
		addrs, _ := i.Addrs()
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() {
				continue
			}
			score := bonus + penalty
			if ip4[0] == 192 && ip4[1] == 168 {
				score += 40
			} else if ip4[0] == 10 {
				score += 25
			} else if ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31 {
				score += 25
			} else {
				score += 5
			}
			if best.ip == "" || score > best.score {
				best = candidate{ip: ip4.String(), score: score}
			}
		}
	}
	if best.ip != "" {
		return best.ip
	}
	return "0.0.0.0"
}

var digitRun = regexp.MustCompile(`[0-9]{4,8}`)

func extractVerificationCode(text string) string {
	indexes := digitRun.FindAllStringIndex(text, -1)
	fallback := ""
	for _, index := range indexes {
		if index[0] > 0 && text[index[0]-1] >= '0' && text[index[0]-1] <= '9' {
			continue
		}
		if index[1] < len(text) && text[index[1]] >= '0' && text[index[1]] <= '9' {
			continue
		}
		code := text[index[0]:index[1]]
		if len(code) == 6 {
			return code
		}
		if fallback == "" {
			fallback = code
		}
	}
	return fallback
}

func formatPairCode(code string) string {
	if len(code) == 6 {
		return code[:3] + " " + code[3:]
	}
	return code
}

// instanceHandover runs while another process holds the single-instance
// mutex. Each attempt first asks that instance to show its window (signal
// returns true when it listened), then retries the mutex in case the other
// instance was only finishing its shutdown (acquire returns true once this
// process owns it). startup reports that this process should keep starting;
// signaled reports that the running instance took over. Both false after the
// last attempt means an instance too old to listen is still running.
func instanceHandover(signal func() bool, acquire func() bool, attempts int, pause func()) (startup, signaled bool) {
	for attempt := 0; attempt < attempts; attempt++ {
		if signal() {
			return false, true
		}
		if acquire() {
			return true, false
		}
		if attempt+1 < attempts && pause != nil {
			pause()
		}
	}
	return false, false
}

func hasTrayArgument(arguments []string) bool {
	for _, argument := range arguments {
		if argument == "--tray" {
			return true
		}
	}
	return false
}
