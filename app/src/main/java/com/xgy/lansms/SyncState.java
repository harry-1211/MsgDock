package com.xgy.lansms;

import java.util.ArrayList;
import java.util.Collections;
import java.util.List;

/**
 * The five-state sync model shared with the Windows client (DESIGN.md "同步状态模型").
 * Pure Java: it only reads facts that MainActivity gathers and never touches sync
 * behaviour, so every rule and the priority order are covered by JVM tests. The
 * state names, colours, order and thresholds must stay identical to the Windows
 * implementation.
 */
final class SyncState {
    /** Ordered as in DESIGN.md; the first matching state wins. */
    enum Kind {
        BROKEN("同步中断", "#F87171"),
        ATTENTION("需要处理", "#94A3B8"),
        DELAYED("同步延迟", "#FBBF24"),
        SYNCING("正在同步", "#38BDF8"),
        OK("同步正常", "#34D399");

        final String title;
        final String colorHex;

        Kind(String title, String colorHex) {
            this.title = title;
            this.colorHex = colorHex;
        }
    }

    enum Action { RELOGIN, GRANT_SMS, ENABLE_NOTIFICATIONS, BATTERY, ADD_ROUTE, ENABLE_RECEIVE, DEAD_LETTERS }

    /** One needs-action row: a sentence, a button, and for battery an extra guide link. */
    static final class Item {
        final Action action;
        final String text;
        final String button;
        final boolean guideLink;

        Item(Action action, String text, String button, boolean guideLink) {
            this.action = action;
            this.text = text;
            this.button = button;
            this.guideLink = guideLink;
        }
    }

    static final class Input {
        long now;
        boolean online = true;
        boolean authRequired;
        /** Time of the first uncleared account/cloud server failure; 0 when healthy. */
        long failingSinceAt;

        boolean canReceiveSms = true;
        boolean smsPermission;
        boolean notificationsEnabled = true;
        boolean batteryExempt = true;
        boolean loggedIn;
        boolean accountReceive;
        /** The user turned the local receiver service on (LAN, account and cloud receive). */
        boolean receiverEnabled;
        int lanTargets;
        int cloudSenderLinks;
        int cloudReceiverLinks;
        int deadLetters;

        /** Negative counts mean the queue file could not be read; they count as empty here. */
        int accountPending;
        int legacyPending;
        int inFlight;

        /** Current receiver poll interval; 0 when this device does not poll at all. */
        long pollIntervalMs;
        long lastSuccessAt;

        /** LAN receiver whose latest forward failed; empty when LAN is fine. */
        String lanProblem = "";
        long lastForwardedAt;
        String lastForwardedTo = "";
        long lastReceivedAt;
        String lastReceivedFrom = "";
    }

    static final class Result {
        final Kind kind;
        final String reason;
        final String recent;
        final List<Item> items;
        /**
         * The item the reason line is about, when there is one: its button belongs in
         * the status card, and the needs-action list shows only the other items so the
         * same sentence is never printed twice. Null when the reason is about something
         * else (offline, server unreachable, backlog).
         */
        final Item primary;

        Result(Kind kind, String reason, String recent, List<Item> items, Item primary) {
            this.kind = kind;
            this.reason = reason;
            this.recent = recent;
            this.items = Collections.unmodifiableList(items);
            this.primary = primary;
        }

        String title() { return kind.title; }
        String colorHex() { return kind.colorHex; }

        /** The needs-action rows that still have to be listed under the status card. */
        List<Item> remainingItems() {
            if (primary == null) return items;
            List<Item> rest = new ArrayList<>(items);
            rest.remove(primary);
            return Collections.unmodifiableList(rest);
        }
    }

    static final long MIN_THRESHOLD_MS = 30_000L;
    static final long UNREACHABLE_MS = 5 * 60_000L;

    private SyncState() {}

    /** 2x the current poll interval, at least 30 s; 0 means "do not judge staleness". */
    static long thresholdMs(long pollIntervalMs) {
        if (pollIntervalMs <= 0) return 0L;
        return Math.max(MIN_THRESHOLD_MS, pollIntervalMs * 2L);
    }

    static boolean forwarding(Input in) {
        return in.canReceiveSms && in.smsPermission && hasForwardTarget(in);
    }

    static boolean hasForwardTarget(Input in) {
        return in.loggedIn || in.lanTargets > 0 || in.cloudSenderLinks > 0;
    }

    /**
     * A forward target only counts on a device that can receive SMS at all; a tablet
     * that merely logged in is doing nothing until it also turns receiving on.
     */
    static boolean hasAnyRoute(Input in) {
        return (in.canReceiveSms && hasForwardTarget(in)) || in.receiverEnabled || in.cloudReceiverLinks > 0;
    }

    static int backlog(Input in) {
        return Math.max(0, in.accountPending) + Math.max(0, in.legacyPending);
    }

    static boolean unreachable(Input in) {
        return in.failingSinceAt > 0 && in.now - in.failingSinceAt > UNREACHABLE_MS;
    }

    static boolean stale(Input in) {
        long threshold = thresholdMs(in.pollIntervalMs);
        return threshold > 0 && in.lastSuccessAt > 0 && in.now - in.lastSuccessAt > threshold;
    }

    static List<Item> items(Input in) {
        List<Item> items = new ArrayList<>();
        if (in.authRequired) {
            items.add(new Item(Action.RELOGIN, "账号授权已失效，重新登录后会自动补传。", "重新登录", false));
        }
        if (in.canReceiveSms && hasForwardTarget(in) && !in.smsPermission) {
            items.add(new Item(Action.GRANT_SMS, "没有短信权限，本机的新短信不会转发。", "允许短信权限", false));
        }
        if (in.receiverEnabled && !in.notificationsEnabled) {
            items.add(new Item(Action.ENABLE_NOTIFICATIONS, "通知已关闭，收到的短信只会存进收件箱，不会提醒。", "开启通知", false));
        }
        if ((forwarding(in) || in.receiverEnabled) && !in.batteryExempt) {
            items.add(new Item(Action.BATTERY, "电池优化未豁免，锁屏后短信可能延迟。", "关闭电池优化", true));
        }
        if (!hasAnyRoute(in)) {
            if (in.canReceiveSms) {
                items.add(new Item(Action.ADD_ROUTE, "还没有任何转发目标或接收来源。", "登录账号或添加接收端", false));
            } else if (in.loggedIn) {
                items.add(new Item(Action.ENABLE_RECEIVE, "本机没有短信功能，还没有开启接收。", "开启接收", false));
            } else {
                items.add(new Item(Action.ADD_ROUTE, "本机没有短信功能，登录账号后可以接收其他手机的短信。", "登录账号", false));
            }
        }
        if (in.deadLetters > 0) {
            items.add(new Item(Action.DEAD_LETTERS, in.deadLetters + " 条短信多次发送失败", "查看", false));
        }
        return items;
    }

    static Result evaluate(Input in) {
        List<Item> items = items(in);
        int backlog = backlog(in);
        Kind kind;
        String reason;
        Item primary = null;
        if (!in.online) {
            kind = Kind.BROKEN;
            reason = backlog > 0 ? "没有网络，" + backlog + " 条短信会在联网后自动补发。" : "没有网络，联网后会自动恢复。";
        } else if (in.authRequired) {
            kind = Kind.BROKEN;
            reason = "账号授权已失效，账号同步已暂停。";
            primary = items.get(0); // always the relogin row
        } else if (unreachable(in)) {
            kind = Kind.BROKEN;
            reason = "服务端连续 " + Math.max(1L, (in.now - in.failingSinceAt) / 60_000L) + " 分钟无法连接，仍在自动重试。";
        } else if (!items.isEmpty()) {
            kind = Kind.ATTENTION;
            primary = items.get(0);
            reason = primary.text;
        } else if (backlog > 0 && in.inFlight == 0) {
            kind = Kind.DELAYED;
            reason = backlog + " 条短信等待发送，稍后自动重试。";
        } else if (stale(in)) {
            kind = Kind.DELAYED;
            reason = "最近一次成功同步是 " + ago(in.lastSuccessAt, in.now) + "，比预期久。";
        } else if (in.inFlight > 0) {
            kind = Kind.SYNCING;
            reason = backlog > 0 ? "正在发送 " + backlog + " 条短信。" : "正在发送短信。";
        } else {
            kind = Kind.OK;
            reason = okReason(in);
        }
        return new Result(kind, reason, recentLine(in), items, primary);
    }

    /** LAN delivery failure is never a needs-action item; it only shows up here. */
    private static String okReason(Input in) {
        if (forwarding(in) && in.lanTargets > 0 && in.lanProblem != null && !in.lanProblem.isEmpty()) {
            return "上次发往“" + in.lanProblem + "”失败，电脑可能没开"
                    + (in.loggedIn || in.cloudSenderLinks > 0 ? "；账号和云端不受影响。" : "。");
        }
        List<String> parts = new ArrayList<>();
        if (forwarding(in)) parts.add("转发到" + routeSummary(in));
        if (in.receiverEnabled) parts.add("接收" + sourceSummary(in));
        if (parts.isEmpty()) return in.canReceiveSms ? "本机未转发也未接收。" : "本机没有短信功能，已停止接收。";
        return join(parts, "；") + "。";
    }

    private static String routeSummary(Input in) {
        List<String> parts = new ArrayList<>();
        if (in.loggedIn) parts.add("账号");
        if (in.lanTargets > 0) parts.add(in.lanTargets + " 台局域网电脑");
        if (in.cloudSenderLinks > 0) parts.add(in.cloudSenderLinks + " 条云端链路");
        return join(parts, "、");
    }

    private static String sourceSummary(Input in) {
        List<String> parts = new ArrayList<>();
        parts.add("局域网");
        if (in.loggedIn && in.accountReceive) parts.add("账号");
        if (in.cloudReceiverLinks > 0) parts.add("云端");
        return join(parts, "、");
    }

    /** "最近一条短信 N 分钟前转发到 X" or "…来自 X", whichever happened last. */
    static String recentLine(Input in) {
        boolean forwarded = in.lastForwardedAt > 0;
        boolean received = in.lastReceivedAt > 0;
        if (!forwarded && !received) return "还没有同步过短信。";
        if (forwarded && (!received || in.lastForwardedAt >= in.lastReceivedAt)) {
            String target = in.lastForwardedTo == null || in.lastForwardedTo.isEmpty() ? "接收端" : in.lastForwardedTo;
            return "最近一条短信 " + ago(in.lastForwardedAt, in.now) + "转发到 " + target;
        }
        String source = in.lastReceivedFrom == null || in.lastReceivedFrom.isEmpty() ? "其他设备" : in.lastReceivedFrom;
        return "最近一条短信 " + ago(in.lastReceivedAt, in.now) + "来自 " + source;
    }

    private static String join(List<String> parts, String separator) {
        StringBuilder out = new StringBuilder();
        for (int i = 0; i < parts.size(); i++) {
            if (i > 0) out.append(separator);
            out.append(parts.get(i));
        }
        return out.toString();
    }

    /** Coarse, locale-free relative time; "刚刚" under a minute, empty when unknown. */
    static String ago(long at, long now) {
        if (at <= 0 || now <= 0 || at > now + 60_000L) return "";
        long seconds = Math.max(0L, (now - at) / 1000L);
        if (seconds < 60) return "刚刚";
        long minutes = seconds / 60;
        if (minutes < 60) return minutes + " 分钟前";
        long hours = minutes / 60;
        if (hours < 24) return hours + " 小时前";
        return (hours / 24) + " 天前";
    }
}
