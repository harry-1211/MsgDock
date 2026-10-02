//go:build windows

package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"git.sr.ht/~jackmordaunt/go-toast/v2/wintoast"
	"golang.org/x/sys/windows/registry"
)

const (
	notificationAppID         = "Xgy LAN SMS"
	notificationActivatorGUID = "{4A147B2E-9207-4D81-995C-DF072E85151E}"
)

var errNotificationWorkerStopped = errors.New("notification worker is stopped")

type notificationRequest struct {
	sms    SMS
	probe  bool // read the notification setting instead of showing a card
	result chan error
}

type notificationWorker struct {
	queue   chan notificationRequest
	stop    chan struct{}
	done    chan struct{}
	once    sync.Once
	lifeMu  sync.RWMutex
	stopped bool
	push    func(SMS) error
	probe   func() error // nil means probes succeed without doing anything
	setup   func() error // runs once on the worker thread before the first request
	cleanup func()
}

func newNotificationWorker(push func(SMS) error) *notificationWorker {
	if push == nil {
		push = func(SMS) error { return errors.New("notification presenter is not configured") }
	}
	return &notificationWorker{
		queue: make(chan notificationRequest, 32),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
		push:  push,
	}
}

func (a *App) initializeNotifications() error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return err
	}
	activationCommand := fmt.Sprintf("\"%s\"", executable)
	data := wintoast.AppData{
		AppID:         notificationAppID,
		GUID:          notificationActivatorGUID,
		ActivationExe: activationCommand,
	}
	if err := wintoast.SetAppData(data); err != nil {
		return err
	}
	if err := refreshActivationExecutable(activationCommand); err != nil {
		return err
	}
	wintoast.SetActivationCallback(func(_ string, arguments string, _ []wintoast.UserData) {
		a.mu.RLock()
		ui := a.ui
		a.mu.RUnlock()
		if ui != nil {
			ui.handleToastAction(arguments)
		}
	})
	sender := &nativeToastSender{onSetting: func(disabled bool) { a.notifyDisabled.Store(disabled) }}
	presenter := &smsNotificationPresenter{push: sender.push, now: time.Now, preview: a.notificationPreview, copyCode: func(sms SMS, code string) {
		a.mu.RLock()
		ui := a.ui
		a.mu.RUnlock()
		if ui != nil {
			ui.enqueueVerificationCode(sms, code)
		}
	}}
	worker := newNotificationWorker(presenter.deliver)
	// Register the activation class object now rather than on the first push,
	// so clicking a card left by a previous run reaches this process instead of
	// making COM launch a second EXE that only brings the window forward.
	worker.setup = sender.initialize
	worker.probe = sender.probeSetting
	worker.cleanup = sender.close
	a.mu.Lock()
	a.notify = worker
	a.mu.Unlock()
	go worker.run()
	return nil
}

func (worker *notificationWorker) run() {
	// Walk owns a single-threaded COM apartment on the GUI thread. WinRT toast
	// notifications require their own multithreaded apartment, so every call is
	// serialized on this dedicated, permanently locked OS thread.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(worker.done)
	defer func() {
		if worker.cleanup != nil {
			worker.cleanup()
		}
	}()
	if worker.setup != nil {
		if err := worker.setup(); err != nil {
			// push retries the initialization, so a late failure is not fatal.
			log.Printf("prepare Windows notifications failed: %v", err)
		}
	}
	for {
		select {
		case request := <-worker.queue:
			switch {
			case !request.probe:
				request.result <- worker.push(request.sms)
			case worker.probe != nil:
				request.result <- worker.probe()
			default:
				request.result <- nil
			}
		case <-worker.stop:
			for {
				select {
				case request := <-worker.queue:
					request.result <- errNotificationWorkerStopped
				default:
					return
				}
			}
		}
	}
}

func (worker *notificationWorker) request(sms SMS) error {
	return worker.submit(notificationRequest{sms: sms})
}

// probeSetting asks the worker thread to re-read whether Windows shows this
// app's notifications, without presenting a card.
func (worker *notificationWorker) probeSetting() error {
	return worker.submit(notificationRequest{probe: true})
}

func (worker *notificationWorker) submit(request notificationRequest) error {
	// Keep the worker alive until this request has received its result. Without
	// this lifecycle read lock, a request could win the queue send at the same
	// instant shutdown drains the channel and exits, then wait forever.
	worker.lifeMu.RLock()
	defer worker.lifeMu.RUnlock()
	if worker.stopped {
		return errNotificationWorkerStopped
	}
	request.result = make(chan error, 1)
	worker.queue <- request
	return <-request.result
}

// probeNotificationSetting refreshes the 关闭了通知 row outside message
// traffic: at startup, so a muted app is reported before the first SMS, and
// periodically, so the row clears once the user turns notifications back on.
func (a *App) probeNotificationSetting() {
	if a.isClosing() {
		return
	}
	a.mu.RLock()
	worker := a.notify
	a.mu.RUnlock()
	if worker == nil {
		return
	}
	if err := worker.probeSetting(); err != nil && !errors.Is(err, errNotificationWorkerStopped) {
		log.Printf("read Windows notification setting failed: %v", err)
	}
}

func (worker *notificationWorker) shutdown() {
	worker.once.Do(func() {
		worker.lifeMu.Lock()
		worker.stopped = true
		close(worker.stop)
		worker.lifeMu.Unlock()
	})
	<-worker.done
}

func (a *App) stopNotifications() {
	a.mu.Lock()
	worker := a.notify
	a.notify = nil
	a.mu.Unlock()
	if worker != nil {
		worker.shutdown()
	}
}

func refreshActivationExecutable(command string) error {
	path := filepath.Join("SOFTWARE", "Classes", "CLSID", notificationActivatorGUID, "LocalServer32")
	key, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()
	return key.SetStringValue("", command)
}

func (a *App) showSMSNotification(sms SMS) error {
	if a.isClosing() {
		return errors.New("receiver is shutting down")
	}
	a.mu.RLock()
	worker := a.notify
	ui := a.ui
	trayFallbackEnabled := a.cfg.TrayFallbackEnabled || !a.cfg.trayFallbackPresent
	a.mu.RUnlock()
	var nativeErr error
	if worker != nil {
		nativeErr = worker.request(sms)
	} else {
		nativeErr = errors.New("native Windows notification is not initialized")
		log.Printf("native Windows notification is not initialized")
	}
	a.notifyFailed.Store(nativeErr != nil)
	if nativeErr != nil {
		logNotificationError(nativeErr)
	}
	if nativeErr != nil && trayFallbackEnabled && ui != nil {
		title := truncateNotificationText(appName+" · "+sms.From, 60)
		body := truncateNotificationText(sms.Text, 240)
		ui.enqueueTrayFallback(sms.ID, title, body)
	}
	return nativeErr
}

func truncateNotificationText(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	text := []rune(value)
	if len(text) <= maxRunes {
		return value
	}
	return string(text[:maxRunes-1]) + "…"
}

// Completion/ACK still requires synchronous native API success. Burst updates
// from one sender replace that sender's card; different senders stack. Only
// one banner pops per notificationPopupInterval regardless of sender.
type smsNotificationPresenter struct {
	push           func(xml, tag string, suppress bool) error
	copyCode       func(SMS, string)
	preview        func() notificationPreview // nil means previewFull
	now            func() time.Time
	lastPopup      time.Time
	latestReceived int64
	lastCopiedID   string
}

const notificationPopupInterval = 10 * time.Second

func freshNotification(sms SMS, now time.Time) bool {
	age := now.Sub(time.UnixMilli(sms.ReceivedAt))
	return sms.ReceivedAt > 0 && age >= -30*time.Second && age <= 2*time.Minute
}

func (p *smsNotificationPresenter) deliver(sms SMS) error {
	now := p.now()
	// Backfilled history stays in the inbox; old OTPs never steal the clipboard.
	if !freshNotification(sms, now) || sms.ReceivedAt < p.latestReceived {
		return nil
	}
	suppress := !p.lastPopup.IsZero() && now.Sub(p.lastPopup) < notificationPopupInterval
	code := automaticVerificationCode(sms.Text)
	willCopy := code != "" && sms.ID != "" && sms.ID != p.lastCopiedID && p.copyCode != nil
	preview := previewFull
	if p.preview != nil {
		preview = p.preview()
	}
	if err := p.push(buildSMSNotificationXMLWithPreview(sms, willCopy, preview), smsToastTagFor(sms.From), suppress); err != nil {
		return err
	}
	if !suppress {
		p.lastPopup = now
	}
	p.latestReceived = sms.ReceivedAt
	if willCopy {
		p.copyCode(sms, code)
		p.lastCopiedID = sms.ID
	}
	return nil
}

// Manual copy retains the legacy extractor. Automatic writes require an OTP
// keyword adjacent to a complete digit run, not an arbitrary order/phone number.
var automaticCodePattern = regexp.MustCompile(`(?i)(?:验证码|校验码|动态码|验证代码|verification code|security code|one.time (?:password|code)|otp)[^0-9\r\n]{0,20}([0-9]{4,8})(?:[^0-9]|$)`)
var leadingCodePattern = regexp.MustCompile(`(?i)(?:^|[^0-9])([0-9]{4,8})[^0-9\r\n]{0,20}(?:验证码|校验码|动态码|verification code|security code|one.time (?:password|code)|otp)`)

func automaticVerificationCode(text string) string {
	for _, pattern := range []*regexp.Regexp{automaticCodePattern, leadingCodePattern} {
		if match := pattern.FindStringSubmatch(text); match != nil {
			return match[1]
		}
	}
	return ""
}

// notificationPreview is the privacy level of the Toast card. It changes only
// what is drawn; copy actions and OTP auto-copy behave the same at every level.
type notificationPreview string

const (
	previewFull    notificationPreview = "full"    // 完整：来源、验证码、正文
	previewSender  notificationPreview = "sender"  // 只显示来源
	previewMinimal notificationPreview = "minimal" // 只显示“收到新短信”
)

// parseNotificationPreview maps the persisted value; unknown or missing means
// 完整 so configs written before this setting existed keep their behaviour.
func parseNotificationPreview(value string) notificationPreview {
	switch notificationPreview(strings.TrimSpace(value)) {
	case previewSender:
		return previewSender
	case previewMinimal:
		return previewMinimal
	default:
		return previewFull
	}
}

func (preview notificationPreview) Label() string {
	switch preview {
	case previewSender:
		return "只显示来源"
	case previewMinimal:
		return "只显示“收到新短信”"
	default:
		return "完整"
	}
}

// smsToastTagFor keys the Toast by sender so the newest card from one sender
// replaces the previous one while other senders keep their own card. Windows
// limits Tag to 16 characters on older builds; 12 hex digits stay inside it.
func smsToastTagFor(sender string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(sender)))
	return hex.EncodeToString(sum[:6])
}

func buildSMSNotificationXML(sms SMS) string {
	return buildSMSNotificationXMLWithCopy(sms, false)
}

func buildSMSNotificationXMLWithCopy(sms SMS, autoCopied bool) string {
	return buildSMSNotificationXMLWithPreview(sms, autoCopied, previewFull)
}

// buildSMSNotificationXMLWithPreview puts an explicit OTP into the title so it
// can be read at a glance, and says so in the attribution line when the code
// is also being auto-copied. The attribution line otherwise carries the source
// device and the time the phone received the message.
func buildSMSNotificationXMLWithPreview(sms SMS, autoCopied bool, preview notificationPreview) string {
	code := extractVerificationCode(sms.Text)
	title, body := sms.From, sms.Text
	switch preview {
	case previewSender:
		body = "收到新短信"
	case previewMinimal:
		title, body = "收到新短信", ""
	default:
		if otp := automaticVerificationCode(sms.Text); otp != "" {
			title = sms.From + " · 验证码 " + otp
		}
	}

	var attribution []string
	if autoCopied {
		attribution = append(attribution, "验证码已复制到剪贴板")
	}
	if device := strings.TrimSpace(sms.Device); device != "" {
		attribution = append(attribution, "来自 "+device)
	}
	if sms.ReceivedAt > 0 {
		attribution = append(attribution, time.UnixMilli(sms.ReceivedAt).Format("15:04"))
	}
	attributionXML := ""
	if len(attribution) > 0 {
		attributionXML = `<text placement="attribution">` + xmlEscape(strings.Join(attribution, " · ")) + `</text>`
	}
	bodyXML := ""
	if body != "" {
		bodyXML = `<text>` + xmlEscape(body) + `</text>`
	}
	actions := ""
	if code != "" {
		actions += fmt.Sprintf(`<action activationType="foreground" content="复制验证码" arguments="copy-code:%s"/>`, encodeToastValue(code))
	}
	actions += fmt.Sprintf(`<action activationType="foreground" content="复制全文" arguments="copy-full:%s"/>`, encodeToastValue(sms.Text))

	return fmt.Sprintf(
		`<toast activationType="foreground" launch="show-status" duration="short"><visual><binding template="ToastGeneric"><text>%s</text>%s%s</binding></visual><actions>%s</actions><audio silent="true"/></toast>`,
		xmlEscape(title),
		bodyXML,
		attributionXML,
		actions,
	)
}

func encodeToastValue(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeToastValue(value string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

func xmlEscape(value string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		"\"", "&quot;",
		"'", "&apos;",
	)
	return replacer.Replace(value)
}

func logNotificationError(err error) {
	// Deliberately do not fall back to PowerShell: it would reintroduce the
	// command-window flash that this Windows client is designed to eliminate.
	log.Printf("native Windows notification failed: %v", err)
}
