package com.xgy.lansms;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import org.junit.Test;

/**
 * The five-state model from DESIGN.md "同步状态模型". Wording, colours, order and
 * thresholds here must match the Windows evaluateSyncState tests.
 */
public class SyncStateTest {
    private static final long NOW = 1_800_000_000_000L;
    private static final long MIN = 60_000L;

    /** A healthy phone that forwards to its account: no needs-action items. */
    private static SyncState.Input sender() {
        SyncState.Input in = new SyncState.Input();
        in.now = NOW;
        in.smsPermission = true;
        in.loggedIn = true;
        in.lastSuccessAt = NOW - 10_000L;
        return in;
    }

    /** A tablet that only receives from its account, polling every 3 s. */
    private static SyncState.Input receiver() {
        SyncState.Input in = new SyncState.Input();
        in.now = NOW;
        in.canReceiveSms = false;
        in.loggedIn = true;
        in.accountReceive = true;
        in.receiverEnabled = true;
        in.pollIntervalMs = 3000L;
        in.lastSuccessAt = NOW - 2000L;
        return in;
    }

    private static SyncState.Action first(SyncState.Result r) {
        return r.items.get(0).action;
    }

    @Test public void statesCarryDesignColorsAndTitles() {
        assertEquals("#F87171", SyncState.Kind.BROKEN.colorHex);
        assertEquals("#94A3B8", SyncState.Kind.ATTENTION.colorHex);
        assertEquals("#FBBF24", SyncState.Kind.DELAYED.colorHex);
        assertEquals("#38BDF8", SyncState.Kind.SYNCING.colorHex);
        assertEquals("#34D399", SyncState.Kind.OK.colorHex);
        assertEquals("同步中断", SyncState.Kind.BROKEN.title);
        assertEquals("需要处理", SyncState.Kind.ATTENTION.title);
        assertEquals("同步延迟", SyncState.Kind.DELAYED.title);
        assertEquals("正在同步", SyncState.Kind.SYNCING.title);
        assertEquals("同步正常", SyncState.Kind.OK.title);
        // Evaluation order is the enum order.
        SyncState.Kind[] order = SyncState.Kind.values();
        assertEquals(SyncState.Kind.BROKEN, order[0]);
        assertEquals(SyncState.Kind.OK, order[4]);
    }

    @Test public void healthySenderIsOk() {
        SyncState.Result r = SyncState.evaluate(sender());
        assertEquals(SyncState.Kind.OK, r.kind);
        assertEquals("同步正常", r.title());
        assertTrue(r.items.isEmpty());
        assertEquals("转发到账号。", r.reason);
    }

    @Test public void offlineIsBroken() {
        SyncState.Input in = sender();
        in.online = false;
        in.accountPending = 2;
        SyncState.Result r = SyncState.evaluate(in);
        assertEquals(SyncState.Kind.BROKEN, r.kind);
        assertTrue(r.reason.contains("2 条短信会在联网后自动补发"));
    }

    @Test public void authFailureIsBrokenAndFirstItemIsRelogin() {
        SyncState.Input in = sender();
        in.authRequired = true;
        in.batteryExempt = false;
        SyncState.Result r = SyncState.evaluate(in);
        assertEquals(SyncState.Kind.BROKEN, r.kind);
        assertEquals(SyncState.Action.RELOGIN, first(r));
        assertEquals("重新登录", r.items.get(0).button);
    }

    @Test public void serverUnreachableOverFiveMinutesIsBroken() {
        SyncState.Input in = sender();
        in.failingSinceAt = NOW - 5 * MIN - 1000L;
        assertEquals(SyncState.Kind.BROKEN, SyncState.evaluate(in).kind);
        in.failingSinceAt = NOW - 5 * MIN;
        assertEquals(SyncState.Kind.OK, SyncState.evaluate(in).kind);
        in.failingSinceAt = 0L; // any success clears the streak
        assertEquals(SyncState.Kind.OK, SyncState.evaluate(in).kind);
    }

    @Test public void brokenOutranksNeedsAction() {
        SyncState.Input in = sender();
        in.online = false;
        in.smsPermission = false;
        SyncState.Result r = SyncState.evaluate(in);
        assertEquals(SyncState.Kind.BROKEN, r.kind);
        assertEquals(SyncState.Action.GRANT_SMS, first(r)); // list still shows the fix
    }

    @Test public void needsActionOutranksDelayAndSyncing() {
        SyncState.Input in = sender();
        in.smsPermission = false;
        in.accountPending = 3;
        in.inFlight = 1;
        SyncState.Result r = SyncState.evaluate(in);
        assertEquals(SyncState.Kind.ATTENTION, r.kind);
        assertEquals(r.items.get(0).text, r.reason);
    }

    @Test public void backlogWithoutSendingIsDelayed() {
        SyncState.Input in = sender();
        in.accountPending = 2;
        in.legacyPending = 1;
        SyncState.Result r = SyncState.evaluate(in);
        assertEquals(SyncState.Kind.DELAYED, r.kind);
        assertEquals("3 条短信等待发送，稍后自动重试。", r.reason);
    }

    @Test public void backlogWhileSendingIsSyncing() {
        SyncState.Input in = sender();
        in.accountPending = 2;
        in.inFlight = 1;
        SyncState.Result r = SyncState.evaluate(in);
        assertEquals(SyncState.Kind.SYNCING, r.kind);
        assertEquals("正在发送 2 条短信。", r.reason);
    }

    @Test public void inFlightWithoutBacklogIsSyncing() {
        SyncState.Input in = sender();
        in.inFlight = 1;
        assertEquals(SyncState.Kind.SYNCING, SyncState.evaluate(in).kind);
    }

    @Test public void unreadableQueueCountsAsEmptyBacklog() {
        SyncState.Input in = sender();
        in.accountPending = -1;
        assertEquals(0, SyncState.backlog(in));
        assertEquals(SyncState.Kind.OK, SyncState.evaluate(in).kind);
    }

    @Test public void thresholdIsTwicePollIntervalWithThirtySecondFloor() {
        assertEquals(0L, SyncState.thresholdMs(0L));
        assertEquals(30_000L, SyncState.thresholdMs(3000L));
        assertEquals(30_000L, SyncState.thresholdMs(15_000L));
        assertEquals(120_000L, SyncState.thresholdMs(60_000L));
    }

    @Test public void staleReceiverIsDelayed() {
        SyncState.Input in = receiver();
        in.lastSuccessAt = NOW - 31_000L;
        SyncState.Result r = SyncState.evaluate(in);
        assertEquals(SyncState.Kind.DELAYED, r.kind);
        assertTrue(r.reason.contains("比预期久"));
        in.lastSuccessAt = NOW - 29_000L;
        assertEquals(SyncState.Kind.OK, SyncState.evaluate(in).kind);
    }

    @Test public void senderOnlyHasNoStalenessCheck() {
        SyncState.Input in = sender();
        in.pollIntervalMs = 0L;
        in.lastSuccessAt = NOW - 3 * 86_400_000L;
        assertFalse(SyncState.stale(in));
        assertEquals(SyncState.Kind.OK, SyncState.evaluate(in).kind);
    }

    @Test public void unknownLastSuccessIsNotStale() {
        SyncState.Input in = receiver();
        in.lastSuccessAt = 0L;
        assertFalse(SyncState.stale(in));
    }

    @Test public void needsActionItemsFollowDesignOrder() {
        SyncState.Input in = new SyncState.Input();
        in.now = NOW;
        in.authRequired = true;
        in.loggedIn = true;
        in.smsPermission = false;
        in.receiverEnabled = true;
        in.notificationsEnabled = false;
        in.batteryExempt = false;
        in.deadLetters = 2;
        SyncState.Result r = SyncState.evaluate(in);
        assertEquals(5, r.items.size());
        assertEquals(SyncState.Action.RELOGIN, r.items.get(0).action);
        assertEquals(SyncState.Action.GRANT_SMS, r.items.get(1).action);
        assertEquals(SyncState.Action.ENABLE_NOTIFICATIONS, r.items.get(2).action);
        assertEquals(SyncState.Action.BATTERY, r.items.get(3).action);
        assertEquals(SyncState.Action.DEAD_LETTERS, r.items.get(4).action);
        assertEquals("2 条短信多次发送失败", r.items.get(4).text);
        assertTrue(r.items.get(3).guideLink);
        assertFalse(r.items.get(0).guideLink);
    }

    @Test public void smsPermissionOnlyAskedWithTelephonyAndTarget() {
        SyncState.Input in = new SyncState.Input();
        in.now = NOW;
        in.lanTargets = 1;
        assertEquals(SyncState.Action.GRANT_SMS, first(SyncState.evaluate(in)));
        in.canReceiveSms = false; // a LAN target is useless without SMS: no permission row, but no route either
        SyncState.Result tablet = SyncState.evaluate(in);
        assertEquals(1, tablet.items.size());
        assertEquals(SyncState.Action.ADD_ROUTE, first(tablet));
        in.canReceiveSms = true;
        in.lanTargets = 0;
        assertEquals(SyncState.Action.ADD_ROUTE, first(SyncState.evaluate(in)));
    }

    @Test public void notificationsOnlyMatterWhenReceiving() {
        SyncState.Input in = sender();
        in.notificationsEnabled = false;
        assertTrue(SyncState.evaluate(in).items.isEmpty());
        in.receiverEnabled = true;
        assertEquals(SyncState.Action.ENABLE_NOTIFICATIONS, first(SyncState.evaluate(in)));
    }

    @Test public void batteryOnlyMattersWhenForwardingOrReceiving() {
        SyncState.Input in = new SyncState.Input();
        in.now = NOW;
        in.batteryExempt = false;
        SyncState.Result r = SyncState.evaluate(in);
        assertEquals(1, r.items.size());
        assertEquals(SyncState.Action.ADD_ROUTE, first(r));
        in.receiverEnabled = true;
        r = SyncState.evaluate(in);
        assertEquals(SyncState.Action.BATTERY, first(r));
        assertEquals("关闭电池优化", r.items.get(0).button);
    }

    @Test public void noRouteAtAllAsksToLoginOrAddReceiver() {
        SyncState.Input in = new SyncState.Input();
        in.now = NOW;
        in.smsPermission = true;
        SyncState.Result r = SyncState.evaluate(in);
        assertEquals(SyncState.Kind.ATTENTION, r.kind);
        assertEquals("登录账号或添加接收端", r.items.get(0).button);
        in.cloudReceiverLinks = 1; // a receive source counts as a route
        assertTrue(SyncState.evaluate(in).items.isEmpty());
    }

    @Test public void primaryItemIsTheOneTheReasonDescribes() {
        // Needs-action: the first item is the reason, so the card owns its button and
        // the list only carries the rest.
        SyncState.Input in = sender();
        in.smsPermission = false;
        in.deadLetters = 2;
        SyncState.Result r = SyncState.evaluate(in);
        assertEquals(SyncState.Kind.ATTENTION, r.kind);
        assertEquals(SyncState.Action.GRANT_SMS, r.primary.action);
        assertEquals(r.primary.text, r.reason);
        assertEquals(1, r.remainingItems().size());
        assertEquals(SyncState.Action.DEAD_LETTERS, r.remainingItems().get(0).action);
        // A single item leaves nothing for the list at all.
        in.deadLetters = 0;
        assertTrue(SyncState.evaluate(in).remainingItems().isEmpty());
        // Broken by auth: the reason is about the account, so relogin moves into the card.
        in.authRequired = true;
        r = SyncState.evaluate(in);
        assertEquals(SyncState.Kind.BROKEN, r.kind);
        assertEquals(SyncState.Action.RELOGIN, r.primary.action);
        assertEquals(1, r.remainingItems().size());
        assertEquals(SyncState.Action.GRANT_SMS, r.remainingItems().get(0).action);
        // Offline: the reason is the network, nothing moves into the card.
        in.online = false;
        r = SyncState.evaluate(in);
        assertEquals(null, r.primary);
        assertEquals(r.items, r.remainingItems());
    }

    @Test public void deviceWithoutTelephonyNeedsReceivingNotJustLogin() {
        SyncState.Input in = new SyncState.Input();
        in.now = NOW;
        in.canReceiveSms = false;
        SyncState.Result r = SyncState.evaluate(in);
        assertEquals(SyncState.Kind.ATTENTION, r.kind);
        assertEquals(SyncState.Action.ADD_ROUTE, first(r));
        assertEquals("登录账号", r.items.get(0).button);
        // Logged in but not receiving: still nothing is happening, offer the one switch.
        in.loggedIn = true;
        r = SyncState.evaluate(in);
        assertEquals(SyncState.Kind.ATTENTION, r.kind);
        assertEquals(SyncState.Action.ENABLE_RECEIVE, first(r));
        assertEquals("开启接收", r.items.get(0).button);
        // Receiving on: a normal receiver.
        in.accountReceive = true;
        in.receiverEnabled = true;
        in.pollIntervalMs = 3000L;
        in.lastSuccessAt = NOW - 2000L;
        r = SyncState.evaluate(in);
        assertEquals(SyncState.Kind.OK, r.kind);
        assertEquals("接收局域网、账号。", r.reason);
        // A phone that logged in is a forwarder even before receiving is on.
        in.canReceiveSms = true;
        in.receiverEnabled = false;
        in.smsPermission = true;
        assertTrue(SyncState.evaluate(in).items.isEmpty());
    }

    @Test public void lanFailureIsOnlyMentionedInReason() {
        SyncState.Input in = sender();
        in.lanTargets = 1;
        in.lanProblem = "Desk-PC";
        SyncState.Result r = SyncState.evaluate(in);
        assertEquals(SyncState.Kind.OK, r.kind);
        assertTrue(r.items.isEmpty());
        assertTrue(r.reason.contains("Desk-PC"));
        assertTrue(r.reason.contains("账号和云端不受影响"));
        in.lanTargets = 0;
        assertFalse(SyncState.evaluate(in).reason.contains("Desk-PC"));
    }

    @Test public void recentLinePrefersTheLaterEvent() {
        SyncState.Input in = sender();
        assertEquals("还没有同步过短信。", SyncState.recentLine(in));
        in.lastForwardedAt = NOW - 5 * MIN;
        in.lastForwardedTo = "Desk-PC";
        assertEquals("最近一条短信 5 分钟前转发到 Desk-PC", SyncState.recentLine(in));
        in.lastReceivedAt = NOW - 2 * MIN;
        in.lastReceivedFrom = "Phone-A";
        assertEquals("最近一条短信 2 分钟前来自 Phone-A", SyncState.recentLine(in));
        in.lastReceivedFrom = "";
        assertEquals("最近一条短信 2 分钟前来自 其他设备", SyncState.recentLine(in));
    }

    @Test public void receiverReasonListsSources() {
        SyncState.Input in = receiver();
        in.cloudReceiverLinks = 1;
        assertEquals("接收局域网、账号、云端。", SyncState.evaluate(in).reason);
    }

    @Test public void relativeTime() {
        assertEquals("", SyncState.ago(0, NOW));
        assertEquals("刚刚", SyncState.ago(NOW - 10_000L, NOW));
        assertEquals("5 分钟前", SyncState.ago(NOW - 5 * MIN, NOW));
        assertEquals("2 小时前", SyncState.ago(NOW - 2 * 3_600_000L, NOW));
        assertEquals("3 天前", SyncState.ago(NOW - 3 * 86_400_000L, NOW));
        assertEquals("", SyncState.ago(NOW + 10 * MIN, NOW));
    }

    @Test public void syncClockThrottlesWrites() {
        assertTrue(SyncClock.shouldWrite(0L, NOW));
        assertFalse(SyncClock.shouldWrite(NOW - 30_000L, NOW));
        assertTrue(SyncClock.shouldWrite(NOW - 60_000L, NOW));
        assertTrue(SyncClock.shouldWrite(NOW + 5_000L, NOW)); // clock moved backwards
    }

    @Test public void syncClockPollIntervalIsTheShortestActiveLoop() {
        assertEquals(0L, SyncClock.pollInterval(0L, 0L));
        assertEquals(3000L, SyncClock.pollInterval(3000L, 0L));
        assertEquals(5000L, SyncClock.pollInterval(0L, 5000L));
        assertEquals(3000L, SyncClock.pollInterval(3000L, 15_000L));
    }

    @Test public void syncClockInFlightCounterNeverGoesNegative() {
        SyncClock.beginRequest();
        SyncClock.beginRequest();
        assertEquals(2, SyncClock.inFlight());
        SyncClock.endRequest();
        SyncClock.endRequest();
        SyncClock.endRequest();
        assertEquals(0, SyncClock.inFlight());
    }

    @Test public void syncClockFailureStreakStartsOnceAndIsReadable() {
        // markFailure is in-memory only; markSuccess needs a Context and is not exercised here.
        long before = System.currentTimeMillis();
        SyncClock.markFailure();
        long first = SyncClock.failingSince();
        assertTrue(first >= before);
        SyncClock.markFailure();
        assertEquals(first, SyncClock.failingSince());
    }
}
