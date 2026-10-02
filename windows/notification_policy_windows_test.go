//go:build windows

package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAutomaticVerificationCodeDoesNotCopyOrdersOrPhones(t *testing.T) {
	for _, tc := range []struct{ text, want string }{
		{"订单 123456", ""}, {"请联系 13800138000", ""},
		{"验证码 13800138000", ""}, {"验证码 123456789", ""},
		{"订单 123456，验证码 908172", "908172"}, {"动态码：9021", "9021"},
		{"Your security code: 12345678", "12345678"}, {"123456 is your verification code", "123456"},
	} {
		if got := automaticVerificationCode(tc.text); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestNotificationBurstReplacesCardWithoutPopupStorm(t *testing.T) {
	now := time.Unix(1800000000, 0)
	popups, updates, copies := 0, 0, 0
	lastXML := ""
	p := &smsNotificationPresenter{now: func() time.Time { return now },
		push: func(xml, _ string, suppress bool) error {
			updates++
			if !suppress {
				popups++
			}
			lastXML = xml
			return nil
		},
		copyCode: func(SMS, string) { copies++ },
	}
	for i := 0; i < 20; i++ {
		sms := SMS{ID: fmt.Sprint(i), From: "Test", Text: fmt.Sprintf("验证码 %06d", 123400+i), ReceivedAt: now.UnixMilli()}
		if err := p.deliver(sms); err != nil {
			t.Fatal(err)
		}
		now = now.Add(100 * time.Millisecond)
	}
	if updates != 20 || popups != 1 || copies != 20 || !strings.Contains(lastXML, "123419") {
		t.Fatalf("updates=%d popups=%d copies=%d", updates, popups, copies)
	}
	now = now.Add(11 * time.Second)
	if err := p.deliver(SMS{ID: "later", Text: "plain SMS", ReceivedAt: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if popups != 2 || copies != 20 {
		t.Fatalf("later popups=%d copies=%d", popups, copies)
	}
}

func TestOldFutureOutOfOrderAndRetryMessagesDoNotStealClipboard(t *testing.T) {
	now := time.Unix(1800000000, 0)
	updates, copies := 0, 0
	p := &smsNotificationPresenter{now: func() time.Time { return now }, push: func(string, string, bool) error { updates++; return nil }, copyCode: func(SMS, string) { copies++ }}
	for _, at := range []int64{0, now.Add(-3 * time.Minute).UnixMilli(), now.Add(time.Hour).UnixMilli()} {
		if err := p.deliver(SMS{ID: "old", Text: "验证码 123456", ReceivedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	if updates != 0 || copies != 0 {
		t.Fatal("stale/future message was presented")
	}
	sms := SMS{ID: "fresh", Text: "验证码 123456", ReceivedAt: now.UnixMilli()}
	_ = p.deliver(sms)
	_ = p.deliver(sms) // Retried completion must not copy again.
	_ = p.deliver(SMS{ID: "older", Text: "验证码 111111", ReceivedAt: now.Add(-time.Second).UnixMilli()})
	if updates != 2 || copies != 1 {
		t.Fatalf("updates=%d copies=%d", updates, copies)
	}
}

func TestPresenterFailureRemainsRetryable(t *testing.T) {
	now := time.Now()
	fail := true
	copies := 0
	p := &smsNotificationPresenter{now: func() time.Time { return now }, push: func(string, string, bool) error {
		if fail {
			return errors.New("failed")
		}
		return nil
	}, copyCode: func(SMS, string) { copies++ }}
	sms := SMS{ID: "retry", Text: "验证码 123456", ReceivedAt: now.UnixMilli()}
	if p.deliver(sms) == nil || copies != 0 || !p.lastPopup.IsZero() {
		t.Fatal("failed presentation acknowledged")
	}
	fail = false
	if err := p.deliver(sms); err != nil || copies != 1 {
		t.Fatalf("retry error=%v copies=%d", err, copies)
	}
}

func TestQuietRecoveryKeepsDurableHistoryAndCrossPathDedup(t *testing.T) {
	app := &App{dir: t.TempDir(), seenIDs: make(map[string]struct{})}
	p := &smsNotificationPresenter{now: time.Now, push: func(string, string, bool) error { t.Fatal("old history should be silent"); return nil }}
	installTestNotificationWorker(t, app, p.deliver)
	sms := SMS{ID: "recovery", Text: "验证码 123456", ReceivedAt: time.Now().Add(-time.Hour).UnixMilli()}
	if err := app.processAccountMessage(sms); err != nil {
		t.Fatal(err)
	}
	if err := app.processLANMessage(sms); err != nil {
		t.Fatal(err)
	}
	if !app.hasHistoryID(sms.ID) {
		t.Fatal("history missing")
	}
	pending, err := app.pendingNotificationsSnapshot()
	if err != nil || len(pending) != 1 || pending[0].NotifiedAt == 0 {
		t.Fatalf("pending=%v error=%v", pending, err)
	}
}
