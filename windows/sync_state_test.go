//go:build windows

package main

import (
	"strings"
	"testing"
	"time"
)

var syncTestNow = time.Date(2026, 10, 1, 14, 30, 0, 0, time.Local)

// healthyInput is a logged-in, polling account that succeeded a moment ago.
func healthyInput() syncInput {
	return syncInput{
		Now:             syncTestNow,
		AccountLoggedIn: true,
		AccountName:     "demo",
		Account: syncPath{
			Active:      true,
			Interval:    3 * time.Second,
			LastSuccess: syncTestNow.Add(-2 * time.Second),
		},
	}
}

func TestSyncStateNamesColorsAndTitlesMatchDesign(t *testing.T) {
	expect := []struct {
		level       syncLevel
		name, title string
		hex         string
	}{
		{syncBroken, "中断", "同步中断", "#F87171"},
		{syncAttention, "需要处理", "需要处理", "#94A3B8"},
		{syncDelayed, "延迟", "同步延迟", "#FBBF24"},
		{syncSending, "同步中", "正在同步", "#38BDF8"},
		{syncNormal, "正常", "同步正常", "#34D399"},
	}
	for index, tc := range expect {
		if int(tc.level) != index {
			t.Fatalf("%s must be evaluated at position %d, got %d", tc.name, index, tc.level)
		}
		if tc.level.Name() != tc.name || tc.level.Title() != tc.title || tc.level.Hex() != tc.hex {
			t.Fatalf("%v: name=%q title=%q hex=%q", tc.level, tc.level.Name(), tc.level.Title(), tc.level.Hex())
		}
	}
	r, g, b := syncBroken.RGB()
	if r != 0xF8 || g != 0x71 || b != 0x71 {
		t.Fatalf("broken RGB = %02X%02X%02X", r, g, b)
	}
}

func TestSyncStateNormalWhenRemotePathIsHealthy(t *testing.T) {
	s := evaluateSyncState(healthyInput())
	if s.Level != syncNormal || s.Title != "同步正常" || len(s.Actions) != 0 {
		t.Fatalf("status=%+v", s)
	}
	if !strings.Contains(s.Reason, "局域网、账号 demo") {
		t.Fatalf("reason=%q", s.Reason)
	}
}

func TestSyncStateThresholdIsTwiceIntervalWithThirtySecondFloor(t *testing.T) {
	if syncThreshold(3*time.Second) != 30*time.Second {
		t.Fatalf("3 s poll threshold = %v", syncThreshold(3*time.Second))
	}
	if syncThreshold(48*time.Second) != 96*time.Second {
		t.Fatalf("48 s backoff threshold = %v", syncThreshold(48*time.Second))
	}
	in := healthyInput()
	in.Account.LastSuccess = syncTestNow.Add(-29 * time.Second)
	if s := evaluateSyncState(in); s.Level != syncNormal {
		t.Fatalf("29 s without success at 3 s poll must stay normal, got %v", s.Level)
	}
	in.Account.LastSuccess = syncTestNow.Add(-31 * time.Second)
	if s := evaluateSyncState(in); s.Level != syncDelayed {
		t.Fatalf("31 s without success at 3 s poll must be delayed, got %v", s.Level)
	}
	// While backing off, the threshold follows the current interval.
	in.Account.Interval = 48 * time.Second
	in.Account.LastSuccess = syncTestNow.Add(-90 * time.Second)
	if s := evaluateSyncState(in); s.Level != syncNormal {
		t.Fatalf("90 s at 48 s backoff must stay normal, got %v", s.Level)
	}
	in.Account.LastSuccess = syncTestNow.Add(-97 * time.Second)
	in.Account.Failing = true
	in.Account.Detail = "dial tcp: i/o timeout"
	s := evaluateSyncState(in)
	if s.Level != syncDelayed || !strings.Contains(s.Reason, "dial tcp: i/o timeout") || !strings.Contains(s.Reason, "1 分钟") {
		t.Fatalf("97 s at 48 s backoff: %+v", s)
	}
}

func TestSyncStateInactivePathHasNoThreshold(t *testing.T) {
	in := healthyInput()
	in.Account.Active = false
	in.Account.LastSuccess = syncTestNow.Add(-time.Hour)
	if s := evaluateSyncState(in); s.Level != syncNormal {
		t.Fatalf("send-only / idle path must not time out, got %v", s.Level)
	}
	// An unpaired legacy relay is ignored even with stale fields.
	in.Cloud = syncPath{Active: false, LastSuccess: syncTestNow.Add(-time.Hour), FailingSince: syncTestNow.Add(-time.Hour)}
	if s := evaluateSyncState(in); s.Level != syncNormal {
		t.Fatalf("unpaired cloud must be ignored, got %v", s.Level)
	}
}

func TestSyncStateBrokenRules(t *testing.T) {
	in := healthyInput()
	in.Offline = true
	if s := evaluateSyncState(in); s.Level != syncBroken || !strings.Contains(s.Reason, "网络") {
		t.Fatalf("offline: %+v", s)
	}

	in = healthyInput()
	in.AuthFailed = true
	s := evaluateSyncState(in)
	if s.Level != syncBroken || len(s.Actions) == 0 || s.Actions[0].Kind != syncActionRelogin || s.Actions[0].Button != "重新登录" {
		t.Fatalf("auth failure: %+v", s)
	}

	in = healthyInput()
	in.Account.Failing = true
	in.Account.Detail = "HTTP 503"
	in.Account.LastSuccess = syncTestNow.Add(-6 * time.Minute)
	in.Account.FailingSince = syncTestNow.Add(-4*time.Minute - 59*time.Second)
	if s := evaluateSyncState(in); s.Level != syncDelayed {
		t.Fatalf("failing for under 5 minutes must be delayed, got %v", s.Level)
	}
	in.Account.FailingSince = syncTestNow.Add(-5*time.Minute - time.Second)
	s = evaluateSyncState(in)
	if s.Level != syncBroken || !strings.Contains(s.Reason, "5 分钟") || !strings.Contains(s.Reason, "HTTP 503") {
		t.Fatalf("unreachable > 5 min: %+v", s)
	}

	in = healthyInput()
	in.CloudPaired = true
	in.Cloud = syncPath{Active: true, Failing: true, Detail: "HTTP 502", Interval: 10 * time.Second, LastSuccess: syncTestNow.Add(-time.Hour), FailingSince: syncTestNow.Add(-10 * time.Minute)}
	if s := evaluateSyncState(in); s.Level != syncBroken || !strings.Contains(s.Reason, "配对云端") {
		t.Fatalf("paired cloud unreachable: %+v", s)
	}
}

func TestSyncStateAttentionWhenActionsExist(t *testing.T) {
	in := healthyInput()
	in.NotifyFailed = true
	s := evaluateSyncState(in)
	if s.Level != syncAttention || s.Title != "需要处理" || len(s.Actions) != 1 || s.Actions[0].Kind != syncActionNotifications {
		t.Fatalf("toast failure: %+v", s)
	}
	if !strings.Contains(s.Reason, "1 项") {
		t.Fatalf("reason=%q", s.Reason)
	}

	lanOnly := syncInput{Now: syncTestNow}
	s = evaluateSyncState(lanOnly)
	if s.Level != syncAttention || len(s.Actions) != 1 || s.Actions[0].Kind != syncActionConnectRemote {
		t.Fatalf("LAN only: %+v", s)
	}
	if s.Actions[0].Text != "登录账号或配对云端，离开局域网也能收到" {
		t.Fatalf("connect text=%q", s.Actions[0].Text)
	}
	// Either remote path removes the suggestion.
	paired := syncInput{Now: syncTestNow, CloudPaired: true, Cloud: syncPath{Active: true, Interval: 10 * time.Second, LastSuccess: syncTestNow}}
	if s := evaluateSyncState(paired); s.Level != syncNormal || !strings.Contains(s.Reason, "云配对") {
		t.Fatalf("paired cloud only: %+v", s)
	}
}

func TestSyncActionsFollowDesignOrder(t *testing.T) {
	in := syncInput{Now: syncTestNow, AuthFailed: true, NotifyFailed: true}
	actions := syncActions(in)
	if len(actions) != 3 {
		t.Fatalf("actions=%+v", actions)
	}
	want := []syncActionKind{syncActionRelogin, syncActionNotifications, syncActionConnectRemote}
	for index, kind := range want {
		if actions[index].Kind != kind || actions[index].Text == "" || actions[index].Button == "" {
			t.Fatalf("action %d = %+v, want kind %v", index, actions[index], kind)
		}
	}
}

func TestSyncStateBacklogAndSending(t *testing.T) {
	in := healthyInput()
	in.Backlog = 2
	s := evaluateSyncState(in)
	if s.Level != syncDelayed || !strings.Contains(s.Reason, "2 条") {
		t.Fatalf("backlog idle: %+v", s)
	}
	in.Sending = true
	if s := evaluateSyncState(in); s.Level != syncSending || s.Title != "正在同步" {
		t.Fatalf("backlog while sending: %+v", s)
	}
	in.Backlog = 0
	if s := evaluateSyncState(in); s.Level != syncSending {
		t.Fatalf("sending without backlog: %+v", s)
	}
	// A stale path outranks sending.
	in.Account.LastSuccess = syncTestNow.Add(-time.Minute)
	if s := evaluateSyncState(in); s.Level != syncDelayed {
		t.Fatalf("stale while sending: %+v", s)
	}
}

func TestSyncStatePriorityOrderIsBrokenAttentionDelayedSendingNormal(t *testing.T) {
	everything := healthyInput()
	everything.Offline = true
	everything.AuthFailed = true
	everything.NotifyFailed = true
	everything.Backlog = 3
	everything.Sending = true
	everything.Account.LastSuccess = syncTestNow.Add(-time.Hour)
	if s := evaluateSyncState(everything); s.Level != syncBroken {
		t.Fatalf("all conditions: %v", s.Level)
	}
	everything.Offline, everything.AuthFailed = false, false
	if s := evaluateSyncState(everything); s.Level != syncAttention {
		t.Fatalf("without broken: %v", s.Level)
	}
	everything.NotifyFailed = false
	if s := evaluateSyncState(everything); s.Level != syncDelayed {
		t.Fatalf("without actions: %v", s.Level)
	}
	everything.Account.LastSuccess = syncTestNow
	everything.Backlog = 0
	if s := evaluateSyncState(everything); s.Level != syncSending {
		t.Fatalf("without delay: %v", s.Level)
	}
	everything.Sending = false
	if s := evaluateSyncState(everything); s.Level != syncNormal {
		t.Fatalf("nothing left: %v", s.Level)
	}
}

func TestSyncStateTooltipAndLastSMSLine(t *testing.T) {
	in := healthyInput()
	in.LastSMSAt = syncTestNow.Add(-3 * time.Minute).UnixMilli()
	in.LastSMSDevice = "手机"
	s := evaluateSyncState(in)
	if s.Tooltip != "MsgDock · 正常 · 最近短信 3 分钟前" {
		t.Fatalf("tooltip=%q", s.Tooltip)
	}
	if s.LastSMS != "最近一条短信 3 分钟前来自 手机" {
		t.Fatalf("last=%q", s.LastSMS)
	}
	in.LastSMSAt = 0
	s = evaluateSyncState(in)
	if s.Tooltip != "MsgDock · 正常" || s.LastSMS != "" {
		t.Fatalf("no SMS: tooltip=%q last=%q", s.Tooltip, s.LastSMS)
	}
	in.Account.Failing = true
	in.Account.Detail = strings.Repeat("很长的错误", 80)
	in.Account.LastSuccess = syncTestNow.Add(-time.Hour)
	s = evaluateSyncState(in)
	if n := len([]rune(s.Tooltip)); n > trayToolTipLimit {
		t.Fatalf("tooltip has %d runes", n)
	}
	if n := len([]rune(s.Reason)); n > 140 {
		t.Fatalf("reason is %d runes: %q", n, s.Reason)
	}
}

func TestLastSMSAgeAndTableTime(t *testing.T) {
	now := syncTestNow
	cases := []struct {
		at   time.Time
		want string
	}{
		{now.Add(-10 * time.Second), "刚刚"},
		{now.Add(-3 * time.Minute), "3 分钟前"},
		{now.Add(-2 * time.Hour), "2 小时前"},
		{now.Add(-50 * time.Hour), "2 天前"},
	}
	for _, tc := range cases {
		if got := lastSMSAge(tc.at.UnixMilli(), now); got != tc.want {
			t.Errorf("age(%v) = %q, want %q", tc.at, got, tc.want)
		}
	}
	if lastSMSAge(now.Add(time.Hour).UnixMilli(), now) != "" || lastSMSAge(0, now) != "" {
		t.Fatal("future or unknown time should be hidden")
	}
	if got := formatSMSTime(now.Add(-3*time.Minute).UnixMilli(), now); got != "14:27" {
		t.Fatalf("today = %q", got)
	}
	if got := formatSMSTime(now.Add(-26*time.Hour).UnixMilli(), now); got != "09-30 12:30" {
		t.Fatalf("yesterday = %q", got)
	}
	if got := formatSMSTime(time.Date(2025, 12, 31, 8, 0, 0, 0, time.Local).UnixMilli(), now); got != "2025-12-31 08:00" {
		t.Fatalf("last year = %q", got)
	}
	if got := singleLine("第一行\r\n  第二行\t验证码 123456", 12); got != "第一行 第二行 验证码…" {
		t.Fatalf("single line = %q", got)
	}
}

func TestPendingBacklogCountsOnlyOwedEntries(t *testing.T) {
	list := []pendingNotification{
		{ID: "a", Source: "lan", NotifiedAt: 1},     // ledger only
		{ID: "b", Source: "account", NotifiedAt: 1}, // cursor commit pending, transient
		{ID: "c", Source: "cloud", NotifiedAt: 1},   // ACK still owed
		{ID: "d", Source: "lan"},                    // Toast not yet shown
	}
	if got := pendingBacklogCount(list); got != 2 {
		t.Fatalf("backlog = %d, want 2", got)
	}
	if pendingBacklogCount(nil) != 0 {
		t.Fatal("empty backlog")
	}
}

func TestSyncActionsReportDisabledNotificationsBelowFailures(t *testing.T) {
	in := healthyInput()
	in.NotifyDisabled = true
	s := evaluateSyncState(in)
	if s.Level != syncAttention || len(s.Actions) != 1 || s.Actions[0].Kind != syncActionNotifications {
		t.Fatalf("disabled notifications: %+v", s)
	}
	if !strings.Contains(s.Actions[0].Text, "已关闭") || s.Actions[0].Button != "开启通知" {
		t.Fatalf("disabled action = %+v", s.Actions[0])
	}
	// A failed push is the more specific problem and keeps the single row.
	in.NotifyFailed = true
	actions := syncActions(in)
	if len(actions) != 1 || !strings.Contains(actions[0].Text, "发送失败") {
		t.Fatalf("failed+disabled actions = %+v", actions)
	}
}

func TestCountdownTextUsesChineseUnits(t *testing.T) {
	for _, tc := range []struct {
		remaining time.Duration
		want      string
	}{
		{-time.Second, "已过期"}, {0, "已过期"},
		{59*time.Second + 400*time.Millisecond, "59 秒后到期"},
		{4*time.Minute + 59*time.Second, "4 分 59 秒后到期"},
		{10 * time.Minute, "10 分 00 秒后到期"},
	} {
		if got := countdownText(tc.remaining); got != tc.want {
			t.Errorf("countdownText(%s) = %q, want %q", tc.remaining, got, tc.want)
		}
	}
}
