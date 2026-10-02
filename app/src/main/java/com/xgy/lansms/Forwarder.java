package com.xgy.lansms;

import android.content.Context;
import org.json.JSONObject;
import java.io.OutputStream;
import java.net.HttpURLConnection;
import java.net.URL;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;
import java.util.UUID;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.TimeoutException;

public final class Forwarder {
    private static final ExecutorService EXEC = Executors.newCachedThreadPool();
    private Forwarder() {}

    public static void forward(Context c, String from, String text, long receivedAt, int sim) {
        forward(c, UUID.randomUUID().toString(), from, text, receivedAt, sim);
    }

    /** UI/test entry point: LAN I/O, encryption, and outbox fsync stay off the main thread. */
    public static void forwardAsync(Context c, String id, String from, String text, long receivedAt, int sim) {
        Context app = c.getApplicationContext();
        EXEC.execute(() -> forward(app, id, from, text, receivedAt, sim));
    }

    /** One id is shared by LAN and cloud so the receiver can de-duplicate both paths. */
    public static void forward(Context c, String id, String from, String text, long receivedAt, int sim) {
        dispatch(c, id, from, text, receivedAt, sim, null);
    }

    /**
     * SMS broadcast entry point. Android briefly exempts the SMS receiver from Doze
     * network limits, but a cached process may be frozen as soon as the broadcast
     * finishes. So the LAN posts and the first cloud uploads run now, in parallel, and
     * the caller keeps the broadcast open for at most {@code budgetMs}. Anything still
     * unsent stays in the durable outboxes for JobScheduler; LAN is best effort as before.
     */
    public static void forwardAndWait(Context c, String id, String from, String text, long receivedAt, int sim,
                                      long budgetMs) {
        List<Future<?>> work = new ArrayList<>();
        dispatch(c, id, from, text, receivedAt, sim, work);
        long deadline = System.nanoTime() + TimeUnit.MILLISECONDS.toNanos(budgetMs);
        for (Future<?> f : work) {
            long left = deadline - System.nanoTime();
            if (left <= 0) break;
            try { f.get(left, TimeUnit.NANOSECONDS); }
            catch (TimeoutException e) { break; }
            catch (InterruptedException e) { Thread.currentThread().interrupt(); break; }
            catch (Exception ignored) { /* each path logs and persists its own failure */ }
        }
    }

    /** {@code waitList} null keeps the original fire-and-forget behaviour. */
    private static void dispatch(Context c, String id, String from, String text, long receivedAt, int sim,
                                 List<Future<?>> waitList) {
        Context app = c.getApplicationContext();
        String messageId = id == null || id.isEmpty() ? UUID.randomUUID().toString() : id;
        String device = android.os.Build.MODEL;

        // The account path is independent from legacy /v1 pairing. Persist it
        // before starting any network work so a process kill cannot lose a
        // message that was meant for the MsgDock history.
        boolean accountQueued = false;
        if (AccountStore.hasAccount(app)) {
            try {
                AccountOutboxStore.enqueue(app, messageId, from, text, receivedAt);
                accountQueued = true;
            } catch (Exception e) {
                android.util.Log.w("MsgDock", "账号 outbox 写入失败；继续 LAN/旧云端转发", e);
            }
        }

        List<TargetStore.Target> targets = TargetStore.load(app);
        for (TargetStore.Target t : targets) {
            track(waitList, EXEC.submit(() -> send(app, t, messageId, from, text, receivedAt, sim, device)));
        }
        if (accountQueued) {
            // Start the current account path before touching the legacy relay;
            // the two network paths must not serialize one another.
            try {
                CloudSyncJobService.schedule(app);
            } catch (RuntimeException e) {
                android.util.Log.w("MsgDock", "账号 JobScheduler 安排失败；进程内重试仍继续", e);
            }
            // flushOutbox re-arms the in-process retry timer itself when it finishes.
            if (waitList == null) AccountApi.scheduleOutboxFlush(app);
            track(waitList, EXEC.submit(() -> AccountApi.flushOutbox(app)));
        }
        // Keep the old E2EE path intact. It has its own durable encrypted outbox.
        // A malformed legacy link must not prevent the independent account queue
        // from being flushed.
        try {
            if (waitList == null) {
                CloudRelay.enqueueSms(app, messageId, from, text, receivedAt, sim, device);
            } else if (CloudRelay.enqueueSmsWithoutFlush(app, messageId, from, text, receivedAt, sim, device)) {
                track(waitList, CloudRelay.executor().submit(() -> CloudRelay.flushOutbox(app)));
            }
        } catch (RuntimeException e) {
            android.util.Log.w("XgyLanSms", "旧云端入队失败；账号云端仍继续", e);
        }
    }

    private static void track(List<Future<?>> waitList, Future<?> future) {
        if (waitList != null) waitList.add(future);
    }

    public static void send(Context c, TargetStore.Target t, String from, String text, long receivedAt, int sim, String device) {
        send(c, t, UUID.randomUUID().toString(), from, text, receivedAt, sim, device);
    }

    public static void send(Context c, TargetStore.Target t, String id, String from, String text, long receivedAt, int sim, String device) {
        HttpURLConnection conn = null;
        SyncClock.beginRequest(); // status bookkeeping only
        try {
            JSONObject body = new JSONObject();
            body.put("id", id == null || id.isEmpty() ? UUID.randomUUID().toString() : id);
            body.put("from", from == null ? "Unknown" : from);
            body.put("text", text == null ? "" : text);
            body.put("receivedAt", receivedAt);
            body.put("sim", sim);
            body.put("device", device == null ? "Android" : device);
            byte[] data = body.toString().getBytes(StandardCharsets.UTF_8);

            URL url = new URL("http", t.host, t.port, "/sms");
            conn = (HttpURLConnection) LanNet.open(c, url);
            conn.setRequestMethod("POST");
            conn.setConnectTimeout(3500);
            conn.setReadTimeout(3500);
            conn.setDoOutput(true);
            conn.setRequestProperty("Content-Type", "application/json; charset=utf-8");
            conn.setRequestProperty("X-Xgy-Key", t.code);
            conn.setFixedLengthStreamingMode(data.length);
            try (OutputStream os = conn.getOutputStream()) { os.write(data); }
            int status = conn.getResponseCode();
            if (status < 200 || status >= 300) {
                android.util.Log.w("XgyLanSms", "Receiver returned HTTP " + status + " for " + t.label());
            }
            SyncClock.markLan(c, status >= 200 && status < 300, t.name);
        } catch (Exception e) {
            SyncClock.markLan(c, false, t.name);
            android.util.Log.w("XgyLanSms", "Forward failed: " + t.label(), e);
        } finally {
            SyncClock.endRequest();
            if (conn != null) conn.disconnect();
        }
    }
}
