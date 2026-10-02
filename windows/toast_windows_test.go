//go:build windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestToastTitleCarriesCodeAndAttributionShowsDeviceAndTime(t *testing.T) {
	at := time.Date(2026, 10, 1, 14, 5, 0, 0, time.Local)
	sms := SMS{From: "10086", Text: "您的验证码为 482913，5 分钟内有效", Device: "手机", ReceivedAt: at.UnixMilli()}
	copied := buildSMSNotificationXMLWithCopy(sms, true)
	if !strings.Contains(copied, "<text>10086 · 验证码 482913</text>") {
		t.Fatalf("copied toast title: %s", copied)
	}
	if !strings.Contains(copied, `<text placement="attribution">验证码已复制到剪贴板 · 来自 手机 · 14:05</text>`) {
		t.Fatalf("copied toast attribution: %s", copied)
	}
	plain := buildSMSNotificationXML(sms)
	if strings.Contains(plain, "已复制") || !strings.Contains(plain, `<text placement="attribution">来自 手机 · 14:05</text>`) {
		t.Fatalf("manual toast must not claim a copy: %s", plain)
	}
	order := buildSMSNotificationXML(SMS{From: "Shop", Text: "订单 123456 已发货"})
	if strings.Contains(order, "验证码 123456") || strings.Contains(order, "attribution") {
		t.Fatalf("order number promoted to code or empty attribution emitted: %s", order)
	}
}

func TestToastTagIsPerSenderShortAndStable(t *testing.T) {
	a, b := smsToastTagFor("10086"), smsToastTagFor("95555")
	if a == b || a != smsToastTagFor(" 10086 ") || len(a) != 12 {
		t.Fatalf("tags: %q %q", a, b)
	}
	for _, r := range a {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("tag %q is not lowercase hex", a)
		}
	}
	if smsToastTagFor("") == "" {
		t.Fatal("empty sender still needs a tag")
	}
}

func TestPresenterTagsBySenderAndKeepsPopupThrottle(t *testing.T) {
	now := time.Unix(1800000000, 0)
	var tags []string
	popups := 0
	p := &smsNotificationPresenter{now: func() time.Time { return now }, push: func(_ string, tag string, suppress bool) error {
		tags = append(tags, tag)
		if !suppress {
			popups++
		}
		return nil
	}}
	for index, from := range []string{"10086", "95555", "10086"} {
		sms := SMS{ID: string(rune('a' + index)), From: from, Text: "hello", ReceivedAt: now.UnixMilli()}
		if err := p.deliver(sms); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	if len(tags) != 3 || tags[0] != tags[2] || tags[0] == tags[1] {
		t.Fatalf("tags=%v", tags)
	}
	if popups != 1 {
		t.Fatalf("popups=%d, want one banner per 10 s", popups)
	}
}

func TestNotificationPreviewLevels(t *testing.T) {
	sms := SMS{From: "10086", Text: "您的验证码为 482913", Device: "手机", ReceivedAt: time.Date(2026, 10, 1, 9, 30, 0, 0, time.Local).UnixMilli()}

	full := buildSMSNotificationXMLWithPreview(sms, false, previewFull)
	if !strings.Contains(full, "10086 · 验证码 482913") || !strings.Contains(full, "<text>您的验证码为 482913</text>") {
		t.Fatalf("full: %s", full)
	}

	sender := buildSMSNotificationXMLWithPreview(sms, false, previewSender)
	if !strings.Contains(sender, "<text>10086</text><text>收到新短信</text>") {
		t.Fatalf("sender: %s", sender)
	}
	if strings.Contains(sender, "482913</text>") || strings.Contains(sender, "您的验证码") {
		t.Fatalf("sender level leaked the body: %s", sender)
	}

	minimal := buildSMSNotificationXMLWithPreview(sms, true, previewMinimal)
	if !strings.Contains(minimal, "<text>收到新短信</text><text placement=\"attribution\">验证码已复制到剪贴板 · 来自 手机 · 09:30</text>") {
		t.Fatalf("minimal: %s", minimal)
	}
	if strings.Contains(minimal, "10086") || strings.Contains(minimal, "<text>您的") {
		t.Fatalf("minimal level leaked sender or body: %s", minimal)
	}
	// Copy actions keep working at every level so the OTP is reachable without
	// being displayed.
	for _, xml := range []string{full, sender, minimal} {
		if !strings.Contains(xml, `content="复制验证码"`) || !strings.Contains(xml, `content="复制全文"`) {
			t.Fatalf("copy actions missing: %s", xml)
		}
	}
}

func TestNotificationPreviewParsingDefaultsToFull(t *testing.T) {
	for raw, want := range map[string]notificationPreview{
		"": previewFull, "full": previewFull, "sender": previewSender, " minimal ": previewMinimal, "garbage": previewFull,
	} {
		if got := parseNotificationPreview(raw); got != want {
			t.Errorf("parse(%q) = %q, want %q", raw, got, want)
		}
	}
	if previewFull.Label() != "完整" || previewSender.Label() != "只显示来源" || previewMinimal.Label() != "只显示“收到新短信”" {
		t.Fatal("labels changed")
	}
}

func TestPresenterUsesPreviewHookWithoutChangingAutoCopy(t *testing.T) {
	now := time.Unix(1800000000, 0)
	preview := previewMinimal
	copies := 0
	last := ""
	p := &smsNotificationPresenter{
		now:      func() time.Time { return now },
		preview:  func() notificationPreview { return preview },
		push:     func(xml, _ string, _ bool) error { last = xml; return nil },
		copyCode: func(SMS, string) { copies++ },
	}
	if err := p.deliver(SMS{ID: "m", From: "10086", Text: "验证码 654321", ReceivedAt: now.UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	if copies != 1 || !strings.Contains(last, "<text>收到新短信</text>") || !strings.Contains(last, "验证码已复制到剪贴板") || strings.Contains(last, "654321</text>") {
		t.Fatalf("copies=%d xml=%s", copies, last)
	}
}

func TestNotificationPreviewConfigIsBackwardCompatible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"pairCode":"123456","relayUrl":"https://example.test","trayFallbackEnabled":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := loadConfigFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NotificationPreview != "" || parseNotificationPreview(cfg.NotificationPreview) != previewFull {
		t.Fatalf("old config preview = %q", cfg.NotificationPreview)
	}
	app := &App{cfg: cfg, dir: filepath.Dir(path)}
	if app.notificationPreview() != previewFull {
		t.Fatal("missing field must mean 完整")
	}
	if err := app.setNotificationPreview(previewSender); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["notificationPreview"]) != `"sender"` || string(raw["trayFallbackEnabled"]) != "true" {
		t.Fatalf("persisted config = %s", data)
	}
	reloaded, _, err := loadConfigFiles(path)
	if err != nil || parseNotificationPreview(reloaded.NotificationPreview) != previewSender {
		t.Fatalf("reload preview = %q err=%v", reloaded.NotificationPreview, err)
	}
	if err := app.setNotificationPreview("bogus"); err != nil {
		t.Fatal(err)
	}
	if app.notificationPreview() != previewFull {
		t.Fatal("unknown values must normalise to full")
	}
}

func TestStatusDotImageIsRoundOpaqueCenterTransparentCorners(t *testing.T) {
	r, g, b := syncNormal.RGB()
	for _, size := range []int{16, 24, 32} {
		img := statusDotImage(size, r, g, b)
		if img.Bounds().Dx() != size || img.Bounds().Dy() != size {
			t.Fatalf("size %d: bounds %v", size, img.Bounds())
		}
		if c := img.RGBAAt(0, 0); c.A != 0 {
			t.Fatalf("size %d: corner alpha %d", size, c.A)
		}
		center := img.RGBAAt(size/2, size/2)
		if center.A != 255 || center.R != r || center.G != g || center.B != b {
			t.Fatalf("size %d: center = %+v, want opaque %02X%02X%02X", size, center, r, g, b)
		}
		edge := img.RGBAAt(size/2, 0)
		if edge.A == 0 || edge.A == 255 {
			t.Fatalf("size %d: top edge alpha %d should be antialiased", size, edge.A)
		}
		if edge.R > edge.A || edge.G > edge.A || edge.B > edge.A {
			t.Fatalf("size %d: edge %+v is not premultiplied", size, edge)
		}
	}
}
