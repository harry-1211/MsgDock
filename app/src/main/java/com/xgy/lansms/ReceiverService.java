package com.xgy.lansms;

import android.app.Service;
import android.content.Intent;
import android.os.IBinder;
import org.json.JSONObject;
import java.io.ByteArrayOutputStream;
import java.io.EOFException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.*;
import java.nio.charset.StandardCharsets;
import java.util.*;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

public class ReceiverService extends Service {
    public static final String ACTION_STOP = "com.xgy.lansms.action.STOP_RECEIVER";
    public static final String PREF_RECEIVER_ENABLED = "receiver_enabled";
    public static final String PREF_RESTART_MIGRATION_V044 = "receiver_restart_migration_v044";
    public static final int HTTP_PORT = 58123;
    public static final int DISCOVERY_PORT = 58124;
    private static final Object START_LOCK = new Object();
    private static final Object MIGRATION_LOCK = new Object();
    private static volatile boolean processRunning;
    private static volatile boolean startRequested;
    private static volatile String runtimeStatus = "未启动";
    private volatile boolean running;
    private ServerSocket server;
    // Shared by the account and cloud loops: any setting, network, screen-on or
    // charger event bumps the version so both loops re-evaluate immediately.
    private final Object accountWake = new Object();
    private long accountWakeVersion;
    private final android.content.SharedPreferences.OnSharedPreferenceChangeListener accountSettings = (prefs, key) -> {
        if (key == null || "receive_enabled".equals(key) || "user_id".equals(key)
                || "device_token".equals(key) || "session_token".equals(key)
                || "auth_required".equals(key)) wakeAccountReceiver();
    };
    private final android.content.SharedPreferences.OnSharedPreferenceChangeListener cloudSettings = (prefs, key) -> {
        if (key == null || "cloud_links".equals(key)) wakeAccountReceiver();
    };
    private final android.content.BroadcastReceiver powerEvents = new android.content.BroadcastReceiver() {
        @Override public void onReceive(android.content.Context context, Intent intent) {
            // The user is looking or the phone is charging: poll now and return to fast pacing.
            wakeAccountReceiver();
        }
    };
    private boolean powerEventsRegistered;
    private android.net.ConnectivityManager.NetworkCallback accountNetworkCallback;
    private android.net.Network lastNetwork;
    private boolean lastNetworkValidated;
    private String lastServiceText = "";
    private long lastSavedCloudDelay = -1L;
    private final ExecutorService pool = Executors.newCachedThreadPool();

    @Override public void onCreate() {
        super.onCreate();
        processRunning = true;
        startRequested = false;
        String code = TargetStore.ensurePairCode(this);
        startForegroundCompat(code);
        running = true;
        AccountStore.prefs(this).registerOnSharedPreferenceChangeListener(accountSettings);
        TargetStore.prefs(this).registerOnSharedPreferenceChangeListener(cloudSettings);
        runtimeStatus = "服务已启动，正在开启局域网接收…";
        registerAccountNetworkCallback();
        registerPowerEvents();
        pool.execute(this::httpLoop);
        pool.execute(this::announceLoop);
        pool.execute(this::cloudLoop);
        pool.execute(this::accountLoop);
    }

    @Override public int onStartCommand(Intent intent, int flags, int startId) {
        String action = intent == null ? null : intent.getAction();
        if (ReceiverLifecyclePolicy.isExplicitStop(action)) {
            // The stop button persists this first as well; keep this path durable for
            // callers that send the explicit service command directly.
            TargetStore.prefs(this).edit().putBoolean(PREF_RECEIVER_ENABLED, false).commit();
            BootReceiver.cancelReceiverRestart(this);
            stopSelfResult(startId);
            return START_NOT_STICKY;
        }
        // A null intent is Android's START_STICKY restart signal.  Do not change the
        // durable preference in that case, otherwise a process restart disables itself.
        if (intent != null) {
            TargetStore.prefs(this).edit().putBoolean(PREF_RECEIVER_ENABLED, true).commit();
        }
        return START_STICKY;
    }

    @Override public void onDestroy() {
        synchronized (START_LOCK) {
            running = false;
            processRunning = false;
            startRequested = false;
            runtimeStatus = "未启动";
        }
        unregisterAccountNetworkCallback();
        unregisterPowerEvents();
        SyncClock.clearPollDelays();
        AccountStore.prefs(this).unregisterOnSharedPreferenceChangeListener(accountSettings);
        TargetStore.prefs(this).unregisterOnSharedPreferenceChangeListener(cloudSettings);
        wakeAccountReceiver();
        try { if (server != null) server.close(); } catch (Exception ignored) {}
        pool.shutdownNow();
        super.onDestroy();
    }

    @Override public void onTaskRemoved(Intent rootIntent) {
        // Removing the activity task must not turn off an explicitly enabled receiver.
        // Leave the preference untouched and arrange a best-effort process restart.
        if (TargetStore.prefs(this).getBoolean(PREF_RECEIVER_ENABLED, false)) {
            BootReceiver.scheduleReceiverRestart(this);
        }
        super.onTaskRemoved(rootIntent);
    }

    @Override public IBinder onBind(Intent intent) { return null; }

    /** Starts the foreground receiver once when the durable setting is enabled. */
    public static boolean ensureStarted(android.content.Context context) {
        android.content.Context app = context.getApplicationContext();
        if (!TargetStore.prefs(app).getBoolean(PREF_RECEIVER_ENABLED, false)) return false;
        synchronized (START_LOCK) {
            if (processRunning || startRequested) return true;
            startRequested = true;
            runtimeStatus = "正在启动接收…";
            Intent intent = new Intent(app, ReceiverService.class);
            try {
                if (android.os.Build.VERSION.SDK_INT >= 26) app.startForegroundService(intent);
                else app.startService(intent);
                return true;
            } catch (RuntimeException e) {
                startRequested = false;
                runtimeStatus = "启动失败，请重试或检查后台限制";
                android.util.Log.e("XgyLanSms", "Unable to start receiver service", e);
                return false;
            }
        }
    }

    public static boolean isRunningOrStarting() {
        return processRunning || startRequested;
    }

    /** Live service/socket state; the saved enabled preference is only user intent. */
    public static String statusText() {
        return runtimeStatus;
    }

    /** One-time migration for installs that already had a cloud receiver link. */
    public static boolean migrateReceiverEnabled(android.content.Context context) {
        android.content.Context app = context.getApplicationContext();
        synchronized (MIGRATION_LOCK) {
            android.content.SharedPreferences prefs = TargetStore.prefs(app);
            if (prefs.getBoolean(PREF_RESTART_MIGRATION_V044, false)) return false;
            if (CloudConfigStore.receiverLinks(app).isEmpty()) {
                prefs.edit().putBoolean(PREF_RESTART_MIGRATION_V044, true).commit();
                return false;
            }
            if (!prefs.edit().putBoolean(PREF_RECEIVER_ENABLED, true).commit()) return false;
            if (!ensureStarted(app)) return false;
            prefs.edit().putBoolean(PREF_RESTART_MIGRATION_V044, true).commit();
            return true;
        }
    }

    @android.annotation.TargetApi(34)
    private void startForegroundCompat(String code) {
        if (android.os.Build.VERSION.SDK_INT >= 34) {
            startForeground(58123, Notifications.serviceNotification(this, code),
                    android.content.pm.ServiceInfo.FOREGROUND_SERVICE_TYPE_REMOTE_MESSAGING);
        } else {
            startForeground(58123, Notifications.serviceNotification(this, code));
        }
    }

    private void httpLoop() {
        try {
            server = new ServerSocket();
            server.setReuseAddress(true);
            server.bind(new InetSocketAddress(HTTP_PORT));
            synchronized (START_LOCK) {
                if (running) runtimeStatus = "✓ 运行中（LAN 已就绪）";
            }
            while (running) {
                Socket s = server.accept();
                pool.execute(() -> handle(s));
            }
        } catch (Exception e) {
            synchronized (START_LOCK) {
                if (running) {
                    runtimeStatus = "LAN 监听失败；账号/云接收按各自设置继续";
                    android.util.Log.e("XgyLanSms", "HTTP server stopped", e);
                }
            }
        }
    }

    private void handle(Socket s) {
        try (Socket socket = s) {
            socket.setSoTimeout(5000);
            InputStream in = socket.getInputStream();
            OutputStream out = socket.getOutputStream();
            byte[] headerBytes = readHeaders(in, 32 * 1024);
            if (headerBytes == null) return;
            String header = new String(headerBytes, StandardCharsets.ISO_8859_1);
            String[] lines = header.split("\\r?\\n");
            if (lines.length == 0) return;
            String[] first = lines[0].split(" ");
            String method = first.length > 0 ? first[0] : "";
            String path = first.length > 1 ? first[1] : "";
            Map<String,String> h = new HashMap<>();
            for (int i = 1; i < lines.length; i++) {
                int p = lines[i].indexOf(':');
                if (p > 0) h.put(lines[i].substring(0,p).trim().toLowerCase(Locale.ROOT), lines[i].substring(p+1).trim());
            }
            if ("GET".equals(method) && "/".equals(path)) {
                String code = TargetStore.ensurePairCode(this);
                String html = "<html><meta charset=utf-8><body style='font-family:sans-serif'><h2>MsgDock</h2><p>Android Pad 接收端正在运行</p><p>端口: 58123</p><p>局域网配对码: <b style='font-size:28px'>" + code + "</b></p></body></html>";
                respond(out, 200, "text/html; charset=utf-8", html.getBytes(StandardCharsets.UTF_8));
                return;
            }
            if (!"POST".equals(method) || !"/sms".equals(path)) { respondText(out, 404, "not found"); return; }
            String key = h.getOrDefault("x-xgy-key", "");
            if (!key.equals(TargetStore.ensurePairCode(this))) { respondText(out, 403, "bad key"); return; }
            int len = Integer.parseInt(h.getOrDefault("content-length", "0"));
            if (len < 0 || len > 1024 * 1024) { respondText(out, 413, "too large"); return; }
            byte[] body = readFully(in, len);
            JSONObject j = new JSONObject(new String(body, StandardCharsets.UTF_8));
            if (CloudInboxStore.acceptLan(this, j)) {
                SyncClock.markReceived(this, j.optString("device", "Phone"));
                String id = j.optString("id", "");
                if (id.isEmpty()) {
                    Notifications.showSms(this, j.optString("from", "短信"), j.optString("text", ""), j.optString("device", "Phone"));
                } else if (CloudInboxStore.claim(id)) {
                    try {
                        if (!CloudInboxStore.isSeen(this, id)) {
                            JSONObject sms = CloudInboxStore.pending(this, id);
                            if (sms == null) sms = j;
                            if (CloudInboxStore.isDelivered(this, id)) {
                                CloudInboxStore.markSeen(this, id, sms.optString("source", "lan"));
                            } else if (Notifications.showSms(this, sms.optString("from", "短信"), sms.optString("text", ""), sms.optString("device", "Phone"))) {
                                if (!CloudInboxStore.markDelivered(this, id)
                                        || !CloudInboxStore.markSeen(this, id, sms.optString("source", "lan"))) {
                                    android.util.Log.w("XgyLanSms", "LAN notification succeeded but durable delivery ledger update failed");
                                }
                            }
                        }
                    } finally {
                        CloudInboxStore.release(id);
                    }
                }
            }
            respondText(out, 200, "ok");
        } catch (Exception e) {
            android.util.Log.w("XgyLanSms", "client error", e);
        }
    }

    private static byte[] readHeaders(InputStream in, int max) throws Exception {
        ByteArrayOutputStream b = new ByteArrayOutputStream();
        int state = 0;
        while (b.size() < max) {
            int x = in.read();
            if (x < 0) return null;
            b.write(x);
            if ((state == 0 || state == 2) && x == '\r') state++;
            else if ((state == 1 || state == 3) && x == '\n') state++;
            else state = x == '\r' ? 1 : 0;
            if (state == 4) return b.toByteArray();
        }
        return null;
    }

    /** Strict API-26-compatible body read; truncated HTTP requests are rejected. */
    private static byte[] readFully(InputStream in, int length) throws Exception {
        byte[] body = new byte[length];
        int offset = 0;
        while (offset < length) {
            int count = in.read(body, offset, length - offset);
            if (count < 0) throw new EOFException("truncated request body");
            if (count == 0) {
                int one = in.read();
                if (one < 0) throw new EOFException("truncated request body");
                body[offset++] = (byte) one;
            } else {
                offset += count;
            }
        }
        return body;
    }

    private static void respondText(OutputStream out, int status, String text) throws Exception {
        respond(out, status, "text/plain; charset=utf-8", text.getBytes(StandardCharsets.UTF_8));
    }
    private static void respond(OutputStream out, int status, String ct, byte[] body) throws Exception {
        String reason = status == 200 ? "OK" : status == 403 ? "Forbidden" : status == 404 ? "Not Found" : "Error";
        String h = "HTTP/1.1 " + status + " " + reason + "\r\nContent-Type: " + ct + "\r\nContent-Length: " + body.length + "\r\nConnection: close\r\n\r\n";
        out.write(h.getBytes(StandardCharsets.ISO_8859_1)); out.write(body); out.flush();
    }

    private void announceLoop() {
        while (running) {
            if (LanNet.wifi(this) == null) {
                // No Wi-Fi means nobody on the LAN can hear the broadcast. Wait for a
                // network event (bounded, in case Wi-Fi joins without becoming default).
                long version;
                synchronized (accountWake) { version = accountWakeVersion; }
                if (!waitForWake(version, 30_000L)) return;
                continue;
            }
            try (DatagramSocket ds = new DatagramSocket()) {
                LanNet.bindWifi(this, ds);
                ds.setBroadcast(true);
                String name = android.os.Build.MANUFACTURER + " " + android.os.Build.MODEL;
                String ip = LanNet.wifiIpv4(this);
                String msg = "XGY_SMS_V1|" + name.replace("|", " ") + "|" + ip + "|" + HTTP_PORT;
                byte[] data = msg.getBytes(StandardCharsets.UTF_8);
                ds.send(new DatagramPacket(data, data.length, InetAddress.getByName("255.255.255.255"), DISCOVERY_PORT));
            } catch (Exception ignored) {}
            try { Thread.sleep(PowerPolicy.announceDelay(PowerPolicy.read(this))); }
            catch (InterruptedException e) { return; }
        }
    }

    private void cloudLoop() {
        long delay = 5000L;
        int emptyRounds = 0;
        while (running) {
            long version;
            synchronized (accountWake) { version = accountWakeVersion; }
            try {
                int replayed = replayPendingNotifications();
                CloudRelay.PollResult result = CloudRelay.pollReceivers(this);
                if (result.linkCount == 0) {
                    // No cloud receiver means there is nothing to poll: wait for a
                    // link/setting/network/screen event instead of waking on a timer.
                    delay = 0L;
                    emptyRounds = 0;
                } else if (result.newMessages > 0 || replayed > 0) {
                    delay = 5000L;
                    emptyRounds = 0;
                } else if (!result.success) {
                    delay = Math.min(5L * 60L * 1000L, Math.max(5000L, delay) * 2L);
                } else {
                    emptyRounds++;
                    delay = Math.max(idleDelay(delay),
                            PowerPolicy.idlePollDelay(5000L, emptyRounds, PowerPolicy.read(this)));
                }
            } catch (Exception e) {
                delay = Math.min(5L * 60L * 1000L, Math.max(5000L, delay) * 2L);
                android.util.Log.w("XgyLanSms", "Cloud receive poll failed", e);
            }
            refreshServiceNotification();
            SyncClock.setCloudPollDelay(delay); // read-only mirror for the status card
            if (delay != lastSavedCloudDelay) {
                TargetStore.prefs(this).edit().putLong("cloud_receive_next_delay_ms", delay).apply();
                lastSavedCloudDelay = delay;
            }
            if (!waitForWake(version, delay)) return;
        }
    }

    /** Re-posts the ongoing notification only when its text actually changed. */
    private void refreshServiceNotification() {
        try {
            String code = TargetStore.ensurePairCode(this);
            String text = Notifications.serviceText(this, code);
            if (text.equals(lastServiceText)) return;
            android.app.NotificationManager nm = getSystemService(android.app.NotificationManager.class);
            if (nm != null) nm.notify(58123, Notifications.serviceNotification(this, code));
            lastServiceText = text;
        } catch (Exception ignored) {}
    }

    /**
     * Waits for {@code delayMs} (0 = until an event) unless a wake event arrived after
     * {@code version} was read. Returns false when the service is stopping.
     */
    private boolean waitForWake(long version, long delayMs) {
        synchronized (accountWake) {
            if (!running) return false;
            if (version != accountWakeVersion) return true;
            try { accountWake.wait(delayMs); }
            catch (InterruptedException e) { return false; }
            return running;
        }
    }

    @android.annotation.SuppressLint("UnspecifiedRegisterReceiverFlag")
    private void registerPowerEvents() {
        android.content.IntentFilter filter = new android.content.IntentFilter();
        filter.addAction(Intent.ACTION_SCREEN_ON);
        filter.addAction(Intent.ACTION_POWER_CONNECTED);
        filter.addAction(android.os.PowerManager.ACTION_POWER_SAVE_MODE_CHANGED);
        try {
            if (android.os.Build.VERSION.SDK_INT >= 33) {
                registerReceiver(powerEvents, filter, android.content.Context.RECEIVER_NOT_EXPORTED);
            } else {
                registerReceiver(powerEvents, filter);
            }
            powerEventsRegistered = true;
        } catch (RuntimeException e) {
            android.util.Log.w("XgyLanSms", "亮屏/充电监听注册失败；轮询按原间隔继续", e);
        }
    }

    private void unregisterPowerEvents() {
        if (!powerEventsRegistered) return;
        try { unregisterReceiver(powerEvents); } catch (RuntimeException ignored) {}
        powerEventsRegistered = false;
    }

    @android.annotation.TargetApi(24)
    private void registerAccountNetworkCallback() {
        if (android.os.Build.VERSION.SDK_INT < 24) return;
        android.net.ConnectivityManager manager =
                getSystemService(android.net.ConnectivityManager.class);
        if (manager == null) return;
        accountNetworkCallback = new android.net.ConnectivityManager.NetworkCallback() {
            // Capability callbacks also fire for signal/bandwidth updates. Only a new
            // default network or one that just became validated is a reason to sync;
            // anything else would start a network job on every signal change.
            @Override public void onAvailable(android.net.Network network) {
                if (isNetworkTransition(network, null)) triggerAccountSync();
            }

            @Override public void onCapabilitiesChanged(android.net.Network network,
                    android.net.NetworkCapabilities capabilities) {
                if (!capabilities.hasCapability(android.net.NetworkCapabilities.NET_CAPABILITY_INTERNET)) return;
                boolean validated = capabilities.hasCapability(
                        android.net.NetworkCapabilities.NET_CAPABILITY_VALIDATED);
                if (isNetworkTransition(network, validated)) triggerAccountSync();
            }

            @Override public void onLost(android.net.Network network) {
                synchronized (accountWake) {
                    if (network.equals(lastNetwork)) {
                        lastNetwork = null;
                        lastNetworkValidated = false;
                    }
                }
            }
        };
        try {
            manager.registerDefaultNetworkCallback(accountNetworkCallback);
        } catch (RuntimeException e) {
            android.util.Log.w("XgyLanSms", "账号网络监听注册失败", e);
            accountNetworkCallback = null;
        }
    }

    private void unregisterAccountNetworkCallback() {
        android.net.ConnectivityManager manager =
                getSystemService(android.net.ConnectivityManager.class);
        if (manager == null || accountNetworkCallback == null) return;
        try {
            manager.unregisterNetworkCallback(accountNetworkCallback);
        } catch (RuntimeException ignored) {
        } finally {
            accountNetworkCallback = null;
        }
    }

    /** {@code validated} is null when the callback does not report validation. */
    private boolean isNetworkTransition(android.net.Network network, Boolean validated) {
        synchronized (accountWake) {
            boolean sameNetwork = network.equals(lastNetwork);
            boolean changed = !sameNetwork || (Boolean.TRUE.equals(validated) && !lastNetworkValidated);
            lastNetwork = network;
            if (validated != null) lastNetworkValidated = validated;
            else if (!sameNetwork) lastNetworkValidated = false;
            return changed;
        }
    }

    private void triggerAccountSync() {
        wakeAccountReceiver();
        AccountApi.scheduleOutboxFlush(this);
        // Only queue a network job when something is actually waiting to upload.
        CloudSyncJobService.scheduleIfPending(this);
        android.content.Context app = getApplicationContext();
        CloudRelay.executor().execute(() -> AccountApi.flushOutbox(app));
    }

    private int replayPendingNotifications() {
        int delivered = 0;
        for (JSONObject sms : CloudInboxStore.pending(this)) {
            String id = sms.optString("id", "");
            if (id.isEmpty()) continue;
            String deliveryId = CloudInboxStore.deliveryId(sms);
            if (!CloudInboxStore.claim(deliveryId)) continue;
            try {
                synchronized (AccountStore.LOCK) {
                if (!CloudInboxStore.accountVisible(sms, AccountStore.userId(this), AccountStore.receiveEnabled(this))) continue;
                if (CloudInboxStore.isSeen(this, id)) continue;
                if (sms.optBoolean("silent", false) || CloudInboxStore.isDelivered(this, deliveryId)) {
                    if (!CloudInboxStore.markDelivered(this, id)) continue;
                    CloudInboxStore.markSeen(this, id, sms.optString("source", "unknown"));
                    continue;
                }
                boolean notified = Notifications.showSms(this, sms.optString("from", "短信"),
                        sms.optString("text", ""), sms.optString("device", "Android"));
                if (notified && CloudInboxStore.markDelivered(this, id)
                        && CloudInboxStore.markSeen(this, id, sms.optString("source", "unknown"))) delivered++;
                }
            } finally {
                CloudInboxStore.release(deliveryId);
            }
        }
        return delivered;
    }

    private void accountLoop() {
        long delay = 3000L;
        int emptyRounds = 0;
        while (running) {
            long version;
            synchronized (accountWake) { version = accountWakeVersion; }
            boolean enabled = AccountStore.receiveEnabled(this) && AccountStore.hasAccount(this)
                    && !AccountStore.authRequired(this);
            if (enabled) {
                boolean success = AccountApi.pollInbox(this);
                int replayed = replayPendingNotifications();
                if (!success) {
                    delay = Math.min(300_000L, Math.max(3000L, delay) * 2L);
                } else {
                    // 3 s while the screen is on, charging, or messages are flowing;
                    // relaxed only after a quiet stretch on battery with the screen off.
                    emptyRounds = replayed > 0 ? 0 : emptyRounds + 1;
                    delay = PowerPolicy.idlePollDelay(3000L, emptyRounds, PowerPolicy.read(this));
                }
            } else {
                delay = 3000L;
                emptyRounds = 0;
            }
            SyncClock.setAccountPollDelay(enabled ? delay : 0L); // read-only mirror for the status card
            synchronized (accountWake) {
                if (!running) return;
                // Guard against a setting/network change between the check and
                // wait. No account reception means no periodic wakeup at all.
                if (version != accountWakeVersion) continue;
                try { accountWake.wait(enabled ? delay : 0L); }
                catch (InterruptedException e) { return; }
            }
        }
    }

    private void wakeAccountReceiver() {
        synchronized (accountWake) {
            accountWakeVersion++;
            accountWake.notifyAll();
        }
    }

    private static long idleDelay(long previous) {
        if (previous <= 5000L) return 10_000L;
        return 15_000L;
    }

    public static String localIpv4() {
        return "0.0.0.0"; // kept only for binary compatibility; UI calls localIpv4(Context) below.
    }

    public static String localIpv4(android.content.Context context) {
        return LanNet.wifiIpv4(context);
    }
}
