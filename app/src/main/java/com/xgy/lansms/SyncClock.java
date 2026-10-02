package com.xgy.lansms;

import android.content.Context;
import android.content.SharedPreferences;

import java.util.concurrent.atomic.AtomicInteger;

/**
 * Passive bookkeeping for the home-screen status: when any sync path last worked,
 * since when the server has been failing, how many uploads are in flight, and the
 * last forwarded/received SMS. Everything here is written at existing success and
 * failure points and never changes timing or retry behaviour.
 *
 * The precise values live in static fields; the prefs copy of the last success is
 * throttled to one write per minute so a 3-second poll loop never turns into a
 * 3-second disk write, and only exists so the status survives a process restart.
 */
final class SyncClock {
    static final String KEY_LAST_OK = "last_sync_ok_at";
    static final String KEY_LAN_OK = "lan_last_ok_at";
    static final String KEY_LAN_FAIL = "lan_last_fail_at";
    static final String KEY_LAN_FAIL_TARGET = "lan_last_fail_target";
    static final String KEY_FORWARDED_AT = "last_forwarded_at";
    static final String KEY_FORWARDED_TO = "last_forwarded_to";
    static final String KEY_RECEIVED_AT = "last_received_at";
    static final String KEY_RECEIVED_FROM = "last_received_from";
    static final long WRITE_INTERVAL_MS = 60_000L;
    private static volatile long lastWritten;
    private static volatile long lastLanOkWritten;
    private static volatile long lastSuccessAt;
    private static volatile long failingSince;
    private static final AtomicInteger IN_FLIGHT = new AtomicInteger();
    private static volatile long accountPollDelay;
    private static volatile long cloudPollDelay;
    private static volatile long forwardedAt;
    private static volatile String forwardedTo = "";
    private static volatile long receivedAt;
    private static volatile String receivedFrom = "";

    private SyncClock() {}

    /** Any account upload/poll, legacy cloud send/ACK or LAN 2xx: clears the failure streak. */
    static void markSuccess(Context context) {
        long now = System.currentTimeMillis();
        lastSuccessAt = now;
        failingSince = 0L;
        if (!shouldWrite(lastWritten, now)) return;
        lastWritten = now;
        TargetStore.prefs(context.getApplicationContext()).edit().putLong(KEY_LAST_OK, now).apply();
    }

    /** A network or server failure on the account/cloud path (never auth, never LAN). */
    static void markFailure() {
        if (failingSince <= 0L) failingSince = System.currentTimeMillis();
    }

    static long failingSince() { return failingSince; }

    /** Wrap each upload or LAN delivery request; empty polls are not counted. */
    static void beginRequest() { IN_FLIGHT.incrementAndGet(); }

    static void endRequest() {
        // Never go negative even if a caller pairs begin/end incorrectly.
        if (IN_FLIGHT.decrementAndGet() < 0) IN_FLIGHT.set(0);
    }

    static int inFlight() { return Math.max(0, IN_FLIGHT.get()); }

    /** Written by the receiver loops where they compute their delay; 0 = not polling. */
    static void setAccountPollDelay(long delayMs) { accountPollDelay = Math.max(0L, delayMs); }

    static void setCloudPollDelay(long delayMs) { cloudPollDelay = Math.max(0L, delayMs); }

    static void clearPollDelays() {
        accountPollDelay = 0L;
        cloudPollDelay = 0L;
    }

    /** The shortest active poll interval, or 0 when this device only sends. */
    static long pollIntervalMs() {
        return pollInterval(accountPollDelay, cloudPollDelay);
    }

    static long pollInterval(long account, long cloud) {
        if (account > 0 && cloud > 0) return Math.min(account, cloud);
        return Math.max(account, cloud);
    }

    static void markForwarded(Context context, String target) {
        long now = System.currentTimeMillis();
        String name = target == null ? "" : target;
        forwardedAt = now;
        forwardedTo = name;
        TargetStore.prefs(context.getApplicationContext()).edit()
                .putLong(KEY_FORWARDED_AT, now).putString(KEY_FORWARDED_TO, name).apply();
    }

    static void markReceived(Context context, String source) {
        long now = System.currentTimeMillis();
        String name = source == null ? "" : source;
        receivedAt = now;
        receivedFrom = name;
        TargetStore.prefs(context.getApplicationContext()).edit()
                .putLong(KEY_RECEIVED_AT, now).putString(KEY_RECEIVED_FROM, name).apply();
    }

    static long lastForwardedAt(Context context) {
        return forwardedAt > 0 ? forwardedAt : prefs(context).getLong(KEY_FORWARDED_AT, 0L);
    }

    static String lastForwardedTo(Context context) {
        return forwardedAt > 0 ? forwardedTo : prefs(context).getString(KEY_FORWARDED_TO, "");
    }

    static long lastReceivedAt(Context context) {
        return receivedAt > 0 ? receivedAt : prefs(context).getLong(KEY_RECEIVED_AT, 0L);
    }

    static String lastReceivedFrom(Context context) {
        return receivedAt > 0 ? receivedFrom : prefs(context).getString(KEY_RECEIVED_FROM, "");
    }

    static void markLan(Context context, boolean ok, String target) {
        Context app = context.getApplicationContext();
        long now = System.currentTimeMillis();
        if (ok) {
            markSuccess(app);
            markForwarded(app, target);
            // Only a recovery after a failure needs an immediate write; otherwise throttle.
            boolean recovering = TargetStore.prefs(app).getLong(KEY_LAN_FAIL, 0L)
                    > TargetStore.prefs(app).getLong(KEY_LAN_OK, 0L);
            if (!recovering && !shouldWrite(lastLanOkWritten, now)) return;
            lastLanOkWritten = now;
            TargetStore.prefs(app).edit().putLong(KEY_LAN_OK, now).apply();
        } else {
            TargetStore.prefs(app).edit().putLong(KEY_LAN_FAIL, now)
                    .putString(KEY_LAN_FAIL_TARGET, target == null ? "" : target).apply();
        }
    }

    static boolean shouldWrite(long previous, long now) {
        return previous <= 0L || now - previous >= WRITE_INTERVAL_MS || now < previous;
    }

    static long lastSuccess(Context context) {
        return Math.max(lastSuccessAt, Math.max(lastWritten, prefs(context).getLong(KEY_LAST_OK, 0L)));
    }

    /** Name of the LAN receiver whose latest attempt failed, or empty when LAN is fine. */
    static String lanProblem(Context context) {
        SharedPreferences p = prefs(context);
        long fail = p.getLong(KEY_LAN_FAIL, 0L);
        if (fail <= 0L || fail <= p.getLong(KEY_LAN_OK, 0L)) return "";
        String target = p.getString(KEY_LAN_FAIL_TARGET, "");
        return target.isEmpty() ? "局域网接收端" : target;
    }

    private static SharedPreferences prefs(Context context) {
        return TargetStore.prefs(context.getApplicationContext());
    }
}
