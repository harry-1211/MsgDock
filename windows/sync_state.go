//go:build windows

package main

import (
	"fmt"
	"strings"
	"time"
)

// syncLevel is the five-state model shared with Android (DESIGN.md
// "同步状态模型"). The order is the evaluation order: the first level whose
// condition holds wins, so lower values are more urgent.
type syncLevel int

const (
	syncBroken    syncLevel = iota // 中断：无网络 / 授权失效 / 服务端连续不可达 > 5 分钟
	syncAttention                  // 需要处理：待处理列表不为空
	syncDelayed                    // 延迟：有积压且没有在发送，或最近一次成功超过阈值
	syncSending                    // 同步中：有正在发送的条目
	syncNormal                     // 正常
)

// Name is the short state word used in the tray tooltip; Title is the status
// bar headline. Both strings are fixed by DESIGN.md.
func (level syncLevel) Name() string {
	switch level {
	case syncBroken:
		return "中断"
	case syncAttention:
		return "需要处理"
	case syncDelayed:
		return "延迟"
	case syncSending:
		return "同步中"
	default:
		return "正常"
	}
}

func (level syncLevel) Title() string {
	switch level {
	case syncBroken:
		return "同步中断"
	case syncAttention:
		return "需要处理"
	case syncDelayed:
		return "同步延迟"
	case syncSending:
		return "正在同步"
	default:
		return "同步正常"
	}
}

// Hex is the shared palette; it is kept as text so the rule can be tested
// without walk and compared with Android's colors verbatim.
func (level syncLevel) Hex() string {
	switch level {
	case syncBroken:
		return "#F87171"
	case syncAttention:
		return "#94A3B8"
	case syncDelayed:
		return "#FBBF24"
	case syncSending:
		return "#38BDF8"
	default:
		return "#34D399"
	}
}

// RGB splits Hex into channels for image drawing and walk.RGB.
func (level syncLevel) RGB() (r, g, b uint8) {
	var value uint32
	_, _ = fmt.Sscanf(level.Hex(), "#%06x", &value)
	return uint8(value >> 16), uint8(value >> 8), uint8(value)
}

const (
	// syncUnreachableLimit is how long a server may fail continuously before
	// the state is 中断 rather than 延迟.
	syncUnreachableLimit = 5 * time.Minute
	// syncMinimumThreshold absorbs one slow request so the 3 s account poll does
	// not flip 延迟 on and off.
	syncMinimumThreshold = 30 * time.Second
	trayToolTipLimit     = 100
)

// syncThreshold is twice the current poll interval with a 30 s floor.
func syncThreshold(interval time.Duration) time.Duration {
	threshold := 2 * interval
	if threshold < syncMinimumThreshold {
		return syncMinimumThreshold
	}
	return threshold
}

type syncActionKind int

const (
	syncActionRelogin syncActionKind = iota
	syncActionNotifications
	syncActionConnectRemote
)

// syncAction is one row of the needs-action list: a sentence and a button.
type syncAction struct {
	Kind   syncActionKind
	Text   string
	Button string
}

// syncPath is one polled remote path (account sync or the legacy paired
// relay). LAN delivery has no poller and therefore no threshold.
type syncPath struct {
	Active       bool          // credentials exist and this path polls
	Failing      bool          // the latest poll failed
	Detail       string        // latest error text, for the reason line
	Interval     time.Duration // current poll interval including backoff
	LastSuccess  time.Time     // last 2xx; seeded with the start time
	FailingSince time.Time     // first failure of the current streak
}

// syncInput is a plain snapshot so the rules stay testable without a window,
// a tray icon or network clients. Nothing here changes sync behaviour.
type syncInput struct {
	Now             time.Time
	Offline         bool
	AuthFailed      bool // account says 需要重新登录
	AccountLoggedIn bool // a session exists, even without a device token
	AccountName     string
	Account         syncPath
	Cloud           syncPath
	CloudPaired     bool
	Backlog         int  // pending entries still waiting for an ACK
	Sending         bool // an ACK or a fetch that carried new messages is in flight
	NotifyFailed    bool // the latest native Toast failed
	NotifyDisabled  bool // Windows accepted the Toast but reports notifications as off for this app
	LastSMSAt       int64
	LastSMSDevice   string
}

type syncStatus struct {
	Level   syncLevel
	Title   string
	Reason  string
	LastSMS string // "最近一条短信 3 分钟前来自某设备" or ""
	Actions []syncAction
	Tooltip string
}

func evaluateSyncState(in syncInput) syncStatus {
	now := in.Now
	actions := syncActions(in)

	unreachable := func(path syncPath) bool {
		return path.Active && !path.FailingSince.IsZero() && now.Sub(path.FailingSince) > syncUnreachableLimit
	}
	stale := func(path syncPath) bool {
		return path.Active && !path.LastSuccess.IsZero() && now.Sub(path.LastSuccess) > syncThreshold(path.Interval)
	}

	var level syncLevel
	var reason string
	switch {
	case in.Offline:
		level, reason = syncBroken, "没有可用的网络连接，手机的短信暂时无法到达。"
	case in.AuthFailed:
		level, reason = syncBroken, "账号授权已失效，互联网短信不会到达，局域网不受影响。"
	case unreachable(in.Account):
		level, reason = syncBroken, unreachableReason("账号服务", in.Account, now)
	case in.CloudPaired && unreachable(in.Cloud):
		level, reason = syncBroken, unreachableReason("配对云端", in.Cloud, now)
	case len(actions) > 0:
		level, reason = syncAttention, fmt.Sprintf("有 %d 项需要处理，见下方。", len(actions))
	case in.Backlog > 0 && !in.Sending:
		level, reason = syncDelayed, fmt.Sprintf("%d 条短信尚未向手机确认，会自动重试。", in.Backlog)
	case stale(in.Account):
		level, reason = syncDelayed, staleReason("账号同步", in.Account, now)
	case in.CloudPaired && stale(in.Cloud):
		level, reason = syncDelayed, staleReason("配对云端", in.Cloud, now)
	case in.Sending:
		level, reason = syncSending, "正在接收新短信…"
	default:
		level, reason = syncNormal, normalReason(in)
	}

	last := lastSMSAge(in.LastSMSAt, now)
	lastLine := ""
	if last != "" {
		lastLine = "最近一条短信 " + last
		if device := strings.TrimSpace(in.LastSMSDevice); device != "" {
			lastLine += "来自 " + device
		}
	}
	tip := appName + " · " + level.Name()
	if last != "" {
		tip += " · 最近短信 " + last
	}
	return syncStatus{
		Level:   level,
		Title:   level.Title(),
		Reason:  reason,
		LastSMS: lastLine,
		Actions: actions,
		Tooltip: clampRunes(tip, trayToolTipLimit),
	}
}

// syncActions lists needs-action rows in DESIGN.md order. Windows has no SMS
// permission, battery optimisation or dead-letter queue, so those rows do not
// exist here; LAN delivery failures never become actions either.
func syncActions(in syncInput) []syncAction {
	var actions []syncAction
	if in.AuthFailed {
		actions = append(actions, syncAction{Kind: syncActionRelogin, Text: "账号授权已失效，重新登录后恢复互联网短信。", Button: "重新登录"})
	}
	switch {
	case in.NotifyFailed:
		actions = append(actions, syncAction{Kind: syncActionNotifications, Text: "Windows 通知发送失败，短信仍会保存到历史。", Button: "打开通知设置"})
	case in.NotifyDisabled:
		// DESIGN.md row 3 (关闭了通知): Windows accepts the Toast but drops it.
		actions = append(actions, syncAction{Kind: syncActionNotifications, Text: "Windows 已关闭 MsgDock 的通知，新短信不会弹出，仍会保存到历史。", Button: "开启通知"})
	}
	if !in.AccountLoggedIn && !in.CloudPaired {
		actions = append(actions, syncAction{Kind: syncActionConnectRemote, Text: "登录账号或配对云端，离开局域网也能收到", Button: "去设置"})
	}
	return actions
}

func unreachableReason(name string, path syncPath, now time.Time) string {
	minutes := int(now.Sub(path.FailingSince) / time.Minute)
	return fmt.Sprintf("%s已连续 %d 分钟无法访问，正在自动重试：%s", name, minutes, shortError(path.Detail))
}

func staleReason(name string, path syncPath, now time.Time) string {
	since := coarseDuration(now.Sub(path.LastSuccess))
	if path.Failing {
		return fmt.Sprintf("%s上次成功在 %s前，正在自动重试：%s", name, since, shortError(path.Detail))
	}
	return fmt.Sprintf("%s上次成功在 %s前，正在等待下一次轮询。", name, since)
}

func normalReason(in syncInput) string {
	paths := []string{"局域网"}
	if in.AccountLoggedIn {
		if in.AccountName != "" {
			paths = append(paths, "账号 "+in.AccountName)
		} else {
			paths = append(paths, "账号")
		}
	}
	if in.CloudPaired {
		paths = append(paths, "云配对")
	}
	return strings.Join(paths, "、") + " 正常接收。"
}

// lastSMSAge is a coarse age such as "3 分钟前"; "" when unknown or in the future.
func lastSMSAge(receivedAt int64, now time.Time) string {
	if receivedAt <= 0 || now.IsZero() {
		return ""
	}
	age := now.Sub(time.UnixMilli(receivedAt))
	if age < -time.Minute {
		return ""
	}
	if age < time.Minute {
		return "刚刚"
	}
	return coarseDuration(age) + "前"
}

func coarseDuration(age time.Duration) string {
	switch {
	case age < time.Minute:
		return fmt.Sprintf("%d 秒", int(age/time.Second))
	case age < time.Hour:
		return fmt.Sprintf("%d 分钟", int(age/time.Minute))
	case age < 24*time.Hour:
		return fmt.Sprintf("%d 小时", int(age/time.Hour))
	default:
		return fmt.Sprintf("%d 天", int(age/(24*time.Hour)))
	}
}

// countdownText says when a pending cloud pair code expires, in the same
// Chinese units as the rest of the window rather than Go's "4m59s".
func countdownText(remaining time.Duration) string {
	if remaining <= 0 {
		return "已过期"
	}
	remaining = remaining.Round(time.Second)
	minutes := int(remaining / time.Minute)
	seconds := int((remaining % time.Minute) / time.Second)
	if minutes > 0 {
		return fmt.Sprintf("%d 分 %02d 秒后到期", minutes, seconds)
	}
	return fmt.Sprintf("%d 秒后到期", seconds)
}

// formatSMSTime is the table's time column: clock time today, date otherwise.
func formatSMSTime(receivedAt int64, now time.Time) string {
	if receivedAt <= 0 {
		return ""
	}
	at := time.UnixMilli(receivedAt)
	switch {
	case at.Year() != now.Year():
		return at.Format("2006-01-02 15:04")
	case at.YearDay() != now.YearDay():
		return at.Format("01-02 15:04")
	default:
		return at.Format("15:04")
	}
}

// singleLine collapses an SMS body for a one-line table cell.
func singleLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	return clampRunes(text, limit)
}

// pendingBacklogCount counts entries still owed to the phone: cloud entries
// awaiting ACK and anything not yet notified. Notified LAN/account entries are
// the 30-day de-duplication ledger, not backlog.
func pendingBacklogCount(list []pendingNotification) int {
	count := 0
	for _, pending := range list {
		if pending.Source == "cloud" || pending.NotifiedAt == 0 {
			count++
		}
	}
	return count
}

func shortError(detail string) string {
	detail = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(detail, "\r", " "), "\n", " "))
	if detail == "" {
		return "原因未知"
	}
	return clampRunes(detail, 80)
}

func clampRunes(value string, limit int) string {
	runes := []rune(value)
	if limit <= 0 || len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}
