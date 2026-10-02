package com.xgy.lansms;

import android.content.Context;
import android.net.ConnectivityManager;
import android.net.NetworkCapabilities;
import android.os.Build;
import org.json.JSONArray;
import org.json.JSONObject;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.security.KeyPair;
import java.security.PrivateKey;
import java.security.PublicKey;
import java.util.ArrayList;
import java.util.List;
import java.util.Locale;
import java.util.UUID;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.atomic.AtomicBoolean;

/** Cloudflare relay v2 client. The relay only receives encrypted envelopes. */
public final class CloudRelay {
    public static final String DEFAULT_RELAY_URL = RelayHttp.PRIMARY;
    private static final ExecutorService EXECUTOR = Executors.newCachedThreadPool();
    private static final AtomicBoolean FLUSHING = new AtomicBoolean(false);
    private static final AtomicBoolean POLLING = new AtomicBoolean(false);
    private static final AtomicBoolean RECEIVER_PAIRING = new AtomicBoolean(false);
    private static final String PENDING_SESSION = "cloud_receiver_pair_session";
    private static final String PENDING_CODE = "cloud_receiver_pair_code";
    private static final String PENDING_PRIVATE = "cloud_receiver_pair_private";
    private static final String PENDING_RELAY = "cloud_receiver_pair_relay";
    private static final String PENDING_EXPIRES = "cloud_receiver_pair_expires";
    private CloudRelay() {}

    public interface PairCallback { void completed(boolean success, String message); }
    public interface ReceiverPairCallback {
        void started(String code);
        void completed(boolean success, String message);
    }
    public static final class PollResult {
        public final int linkCount;
        public final int newMessages;
        public final boolean success;

        PollResult(int linkCount, int newMessages, boolean success) {
            this.linkCount = linkCount;
            this.newMessages = newMessages;
            this.success = success;
        }
    }
    public static ExecutorService executor() { return EXECUTOR; }

    /** Pairs this phone as a sender and appends the new link to existing links. */
    public static void pair(Context context, String relayUrl, String code, PairCallback callback) {
        Context app = context.getApplicationContext();
        EXECUTOR.execute(() -> {
            try {
                String normalized = normalizeRelayUrl(relayUrl);
                if (code == null || !code.matches("\\d{6}")) throw new IllegalArgumentException("云配对码必须是 6 位数字");
                KeyPair keyPair = CloudCrypto.generateKeyPair();
                JSONObject request = new JSONObject(); request.put("code", code);
                request.put("deviceName", Build.MANUFACTURER + " " + Build.MODEL);
                request.put("deviceType", "android"); request.put("publicKey", CloudCrypto.encodePublicKey(keyPair.getPublic()));
                JSONObject response = responseObject(requestJson("POST", normalized + "/v1/pair/finish", request, null));
                CloudConfigStore.CloudLink link = parseLink(normalized, response, CloudConfigStore.CloudLink.ROLE_SENDER,
                        CloudCrypto.encodePrivateKey(keyPair.getPrivate()));
                if (!CloudConfigStore.saveLink(app, link)) throw new IllegalStateException("无法保存云端配对信息");
                CloudOutboxStore.markSuccess(app); CloudSyncJobService.schedule(app);
                DeviceBackupManager.scheduleSync(app);
                callback(callback, true, "云端发送配对成功：" + link.deviceId);
            } catch (Exception e) { fail(app, callback, errorMessage(e)); }
        });
    }

    /** Starts Android-receiver pairing; the returned six-digit code is shown in the UI. */
    public static void startReceiverPairing(Context context, String relayUrl, ReceiverPairCallback callback) {
        Context app = context.getApplicationContext();
        if (!RECEIVER_PAIRING.compareAndSet(false, true)) {
            if (callback != null) callback.completed(false, "已有一轮云接收配对正在进行，请先等待当前配对结束");
            return;
        }
        EXECUTOR.execute(() -> {
            boolean handedOffToPoll = false;
            try {
                String normalized = normalizeRelayUrl(relayUrl);
                KeyPair keyPair = CloudCrypto.generateKeyPair();
                JSONObject request = new JSONObject(); request.put("deviceName", Build.MANUFACTURER + " " + Build.MODEL);
                request.put("deviceType", "android_receiver"); request.put("publicKey", CloudCrypto.encodePublicKey(keyPair.getPublic()));
                HttpResult result = requestJson("POST", normalized + "/v1/pair/start", request, null);
                if (result.status < 200 || result.status >= 300) throw new IllegalStateException("云接收配对启动失败 HTTP " + result.status + ": " + result.body);
                JSONObject start = new JSONObject(result.body);
                String session = required(start, "sessionId"), code = required(start, "code");
                long expires = start.optLong("expiresAt", System.currentTimeMillis() + 5 * 60_000L);
                String privateKey = CloudCrypto.encodePrivateKey(keyPair.getPrivate());
                TargetStore.prefs(app).edit().putString(PENDING_SESSION, session).putString(PENDING_CODE, code)
                        .putString(PENDING_PRIVATE, privateKey).putString(PENDING_RELAY, normalized)
                        .putLong(PENDING_EXPIRES, expires).apply();
                if (callback != null) callback.started(code);
                EXECUTOR.execute(() -> pollReceiverPair(app, normalized, session, code, privateKey, expires, callback));
                handedOffToPoll = true;
            } catch (Exception e) {
                TargetStore.prefs(app).edit().putString("cloud_last_error", errorMessage(e)).apply();
                if (callback != null) callback.completed(false, errorMessage(e));
            } finally {
                if (!handedOffToPoll) RECEIVER_PAIRING.set(false);
            }
        });
    }

    public static String pendingReceiverCode(Context context) {
        String code = TargetStore.prefs(context.getApplicationContext()).getString(PENDING_CODE, "");
        long expires = TargetStore.prefs(context.getApplicationContext()).getLong(PENDING_EXPIRES, 0L);
        return expires > System.currentTimeMillis() ? code : "";
    }

    private static void pollReceiverPair(Context app, String relay, String session, String code, String privateKey,
                                         long expires, ReceiverPairCallback callback) {
        try {
            while (System.currentTimeMillis() < expires) {
                HttpResult result = requestJson("GET", normalizeRelayUrl(relay) + "/v1/pair/status?sessionId="
                        + enc(session) + "&code=" + enc(code), null, null);
                if (result.status == 202) { Thread.sleep(2000L); continue; }
                if (result.status < 200 || result.status >= 300) throw new IllegalStateException("云接收配对状态 HTTP " + result.status + ": " + result.body);
                JSONObject response = new JSONObject(result.body);
                CloudConfigStore.CloudLink link = parseLink(normalizeRelayUrl(relay), response, CloudConfigStore.CloudLink.ROLE_RECEIVER, privateKey);
                // Save credentials before confirm so a lost confirm response is recoverable.
                if (!CloudConfigStore.saveLink(app, link)) throw new IllegalStateException("无法保存云接收凭据");
                DeviceBackupManager.scheduleSync(app);
                JSONObject confirm = new JSONObject(); confirm.put("sessionId", session);
                try {
                    HttpResult confirmed = requestJson("POST", normalizeRelayUrl(relay) + "/v1/pair/confirm", confirm, link.token);
                    if (confirmed.status < 200 || confirmed.status >= 300) {
                        String message = "云端接收凭据已保存；确认待重试/临时会话已保留（HTTP " + confirmed.status + ")";
                        TargetStore.prefs(app).edit().putString("cloud_last_error", message).apply();
                        if (callback != null) callback.completed(true, message);
                        return;
                    }
                } catch (Exception e) {
                    String message = "云端接收凭据已保存；确认待重试/临时会话已保留（" + errorMessage(e) + ")";
                    TargetStore.prefs(app).edit().putString("cloud_last_error", message).apply();
                    if (callback != null) callback.completed(true, message);
                    return;
                }
                clearPending(app);
                if (callback != null) callback.completed(true, "云端接收配对成功：" + link.deviceId);
                return;
            }
            throw new IllegalStateException("云配对码已过期");
        } catch (Exception e) {
            TargetStore.prefs(app).edit().putString("cloud_last_error", errorMessage(e)).apply();
            if (callback != null) callback.completed(false, errorMessage(e));
        } finally {
            RECEIVER_PAIRING.set(false);
        }
    }

    private static void clearPending(Context context) {
        TargetStore.prefs(context).edit().remove(PENDING_SESSION).remove(PENDING_CODE).remove(PENDING_PRIVATE)
                .remove(PENDING_RELAY).remove(PENDING_EXPIRES).apply();
    }

    /** Encrypts and durably enqueues one SMS for every sender link. */
    public static void enqueueSms(Context context, String id, String from, String text, long receivedAt, int sim, String device) {
        if (enqueueSmsWithoutFlush(context, id, from, text, receivedAt, sim, device)) {
            Context app = context.getApplicationContext();
            EXECUTOR.execute(() -> flushOutbox(app));
        }
    }

    /**
     * Same as above but leaves the first flush to the caller, so the SMS broadcast can
     * wait for it while the system still grants network access. Returns true if queued.
     */
    static boolean enqueueSmsWithoutFlush(Context context, String id, String from, String text,
                                          long receivedAt, int sim, String device) {
        Context app = context.getApplicationContext();
        String messageId = id == null || id.isEmpty() ? UUID.randomUUID().toString() : id;
        List<CloudConfigStore.CloudLink> links = CloudConfigStore.senderLinks(app);
        if (links.isEmpty()) {
            TargetStore.prefs(app).edit().putString("cloud_last_error", "未完成云端发送配对，短信仅通过 LAN 转发").apply();
            return false;
        }
        boolean queued = false;
        for (CloudConfigStore.CloudLink link : links) {
            if (!CloudConfigStore.isConfigured(link)) continue;
            try {
                PrivateKey privateKey = CloudCrypto.decodePrivateKey(link.privateKey);
                PublicKey peerKey = CloudCrypto.decodePublicKey(link.peerPublicKey);
                byte[] roomKey = CloudCrypto.deriveRoomKey(privateKey, peerKey, link.roomId);
                JSONObject plaintext = new JSONObject(); plaintext.put("v", 2); plaintext.put("from", from == null ? "Unknown" : from);
                plaintext.put("text", text == null ? "" : text); plaintext.put("receivedAt", receivedAt); plaintext.put("sim", sim);
                plaintext.put("device", device == null ? "Android" : device);
                String aad = CloudCrypto.aad(messageId, link.roomId, link.deviceId, link.peerDeviceId);
                CloudCrypto.Encrypted encrypted = CloudCrypto.encrypt(roomKey, plaintext.toString().getBytes(StandardCharsets.UTF_8), aad.getBytes(StandardCharsets.UTF_8));
                JSONObject envelope = new JSONObject(); envelope.put("roomId", link.roomId); envelope.put("senderDeviceId", link.deviceId);
                envelope.put("targetDeviceId", link.peerDeviceId); envelope.put("id", messageId); envelope.put("createdAt", System.currentTimeMillis());
                envelope.put("nonce", CloudCrypto.b64(encrypted.nonce)); envelope.put("ciphertext", CloudCrypto.b64(encrypted.ciphertext));
                CloudOutboxStore.enqueue(app, envelope); queued = true;
            } catch (Exception e) { TargetStore.prefs(app).edit().putString("cloud_last_error", errorMessage(e)).apply(); }
        }
        if (queued) CloudSyncJobService.schedule(app);
        return queued;
    }

    public static void flushOutbox(Context context) {
        Context app = context.getApplicationContext(); if (!FLUSHING.compareAndSet(false, true)) return;
        try {
            for (CloudOutboxStore.Entry entry : CloudOutboxStore.due(app, System.currentTimeMillis(), 20)) {
                try {
                    JSONObject envelope = new JSONObject(entry.envelope);
                    CloudConfigStore.CloudLink link = findSender(app, envelope.optString("roomId", ""), envelope.optString("senderDeviceId", ""), envelope.optString("targetDeviceId", ""));
                    if (link == null) { CloudOutboxStore.moveToDeadLetter(app, entry.key, "找不到对应的云端发送链路，已停止重试"); continue; }
                    HttpResult result;
                    SyncClock.beginRequest(); // status bookkeeping only
                    try {
                        result = requestJson("POST", normalizeRelayUrl(link.relayUrl) + "/v1/messages", envelope, link.token);
                    } finally {
                        SyncClock.endRequest();
                    }
                    if (result.status >= 200 && result.status < 300) {
                        CloudOutboxStore.remove(app, entry.key);
                        CloudOutboxStore.markSuccess(app);
                        SyncClock.markSuccess(app);
                        SyncClock.markForwarded(app, link.peerName.isEmpty() ? "云端设备" : link.peerName);
                    } else {
                        String message = "云端发送 HTTP " + result.status + ": " + result.body;
                        if (result.status == 401 || result.status == 403 || result.status == 410
                                || result.body.toLowerCase(Locale.ROOT).contains("created_at_out_of_range")) {
                            CloudOutboxStore.moveToDeadLetter(app, entry.key, message);
                        } else {
                            SyncClock.markFailure();
                            CloudOutboxStore.markFailure(app, entry.key, message);
                        }
                    }
                } catch (Exception e) {
                    SyncClock.markFailure();
                    CloudOutboxStore.markFailure(app, entry.key, errorMessage(e));
                }
            }
        } finally { FLUSHING.set(false); }
    }

    private static CloudConfigStore.CloudLink findSender(Context context, String room, String sender, String target) {
        for (CloudConfigStore.CloudLink link : CloudConfigStore.senderLinks(context))
            if (link.roomId.equals(room) && link.deviceId.equals(sender) && link.peerDeviceId.equals(target)) return link;
        return null;
    }

    /** Polls all receiver links and ACKs only envelopes safely persisted and notified. */
    public static PollResult pollReceivers(Context context) {
        Context app = context.getApplicationContext();
        List<CloudConfigStore.CloudLink> links = CloudConfigStore.receiverLinks(app);
        int configured = 0;
        for (CloudConfigStore.CloudLink link : links) if (CloudConfigStore.isConfigured(link)) configured++;
        if (!POLLING.compareAndSet(false, true)) return new PollResult(configured, 0, true);
        boolean success = true;
        int newMessages = 0;
        try {
            for (CloudConfigStore.CloudLink link : links) {
                if (!CloudConfigStore.isConfigured(link)) continue;
                try { newMessages += pollReceiverLink(app, link); }
                catch (Exception e) {
                    success = false;
                    SyncClock.markFailure();
                    TargetStore.prefs(app).edit().putString("cloud_receive_last_error", errorMessage(e)).apply();
                }
            }
        } finally { POLLING.set(false); }
        return new PollResult(configured, newMessages, success);
    }

    private static int pollReceiverLink(Context app, CloudConfigStore.CloudLink link) throws Exception {
        String url = normalizeRelayUrl(link.relayUrl) + "/v1/messages?roomId=" + enc(link.roomId) + "&deviceId=" + enc(link.deviceId) + "&limit=20";
        HttpResult result = requestJson("GET", url, null, link.token);
        if (result.status < 200 || result.status >= 300) throw new IllegalStateException("云端接收 HTTP " + result.status + ": " + result.body);
        JSONObject root = new JSONObject(result.body); JSONArray messages = root.optJSONArray("messages"); if (messages == null) return 0;
        List<String> ack = new ArrayList<>();
        int newMessages = 0;
        PrivateKey privateKey = CloudCrypto.decodePrivateKey(link.privateKey); PublicKey peer = CloudCrypto.decodePublicKey(link.peerPublicKey);
        byte[] roomKey = CloudCrypto.deriveRoomKey(privateKey, peer, link.roomId);
        for (int i = 0; i < messages.length(); i++) {
            JSONObject envelope = messages.optJSONObject(i); if (envelope == null) continue;
            String id = envelope.optString("id", "");
            if (id.isEmpty() || !link.roomId.equals(envelope.optString("roomId", "")) || !link.deviceId.equals(envelope.optString("targetDeviceId", ""))
                    || !link.peerDeviceId.equals(envelope.optString("senderDeviceId", ""))) continue;
            if (!CloudInboxStore.claim(id)) continue;
            try {
                if (CloudInboxStore.isSeen(app, id)) {
                    ack.add(id);
                    continue;
                }
                if (CloudInboxStore.isDelivered(app, id)) {
                    if (CloudInboxStore.markSeen(app, id, "cloud")) ack.add(id);
                    continue;
                }
                JSONObject sms = CloudInboxStore.pending(app, id);
                if (sms == null) {
                    byte[] plaintext = CloudCrypto.decrypt(roomKey, CloudCrypto.unb64(envelope.getString("nonce")), CloudCrypto.unb64(envelope.getString("ciphertext")),
                            CloudCrypto.aad(id, link.roomId, link.peerDeviceId, link.deviceId).getBytes(StandardCharsets.UTF_8));
                    sms = new JSONObject(new String(plaintext, StandardCharsets.UTF_8));
                    if (!CloudInboxStore.acceptCloud(app, id, sms)) throw new IllegalStateException("云端短信持久化失败");
                    SyncClock.markReceived(app, sms.optString("device", "Android"));
                    sms = CloudInboxStore.pending(app, id);
                }
                if (sms != null && CloudInboxStore.isDelivered(app, id)) {
                    if (CloudInboxStore.markSeen(app, id, sms.optString("source", "cloud"))) ack.add(id);
                } else if (sms != null && Notifications.showSms(app, sms.optString("from", "短信"), sms.optString("text", ""), sms.optString("device", "Android"))) {
                    if (CloudInboxStore.markDelivered(app, id)
                            && CloudInboxStore.markSeen(app, id, sms.optString("source", "cloud"))) {
                        ack.add(id);
                        newMessages++;
                    } else {
                        android.util.Log.w("XgyLanSms", "Cloud notification succeeded but durable delivery ledger update failed");
                    }
                }
            } catch (Exception e) {
                android.util.Log.w("XgyLanSms", "Cloud envelope rejected", e);
            } finally {
                CloudInboxStore.release(id);
            }
        }
        for (int start = 0; start < ack.size(); start += 100) {
            JSONArray ids = new JSONArray(); for (int i = start; i < Math.min(start + 100, ack.size()); i++) ids.put(ack.get(i));
            JSONObject body = new JSONObject(); body.put("roomId", link.roomId); body.put("deviceId", link.deviceId); body.put("messageIds", ids);
            HttpResult acknowledged = requestJson("POST", normalizeRelayUrl(link.relayUrl) + "/v1/ack", body, link.token);
            if (acknowledged.status < 200 || acknowledged.status >= 300) throw new IllegalStateException("云端 ACK HTTP " + acknowledged.status);
        }
        if (!ack.isEmpty()) {
            TargetStore.prefs(app).edit().putLong("cloud_receive_last_success", System.currentTimeMillis()).remove("cloud_receive_last_error").apply();
        }
        // The round trip (GET and any ACK) reached the relay: the link is alive even when idle.
        SyncClock.markSuccess(app);
        return newMessages;
    }

    public static String statusText(Context context) {
        Context app = context.getApplicationContext(); List<CloudConfigStore.CloudLink> links = CloudConfigStore.loadLinks(app);
        StringBuilder out = new StringBuilder("云端链路：").append(links.size()).append(" 条\n");
        for (CloudConfigStore.CloudLink link : links) out.append(link.isSender() ? "发送" : "接收").append("：")
                .append(CloudConfigStore.isConfigured(link) ? "已配对" : "配置无效").append("（").append(link.deviceId).append(" → ").append(link.peerDeviceId)
                .append(link.peerName.isEmpty() ? "" : "，" + link.peerName).append("）\n");
        int pending = CloudOutboxStore.count(app);
        int deadLetters = CloudOutboxStore.deadLetterCount(app);
        out.append("主 Relay：").append(RelayHttp.PRIMARY).append("\n备用 Relay：").append(RelayHttp.BACKUP).append("\n")
                .append("网络：").append(hasNetwork(app) ? "在线" : "离线，等待恢复").append("\n")
                .append("待发送：").append(pending < 0 ? "读取失败（原文件已保留）" : pending)
                .append("\n死信：").append(deadLetters < 0 ? "读取失败" : deadLetters);
        long nextDelay = TargetStore.prefs(app).getLong("cloud_receive_next_delay_ms", 5000L);
        if (CloudConfigStore.receiverLinks(app).isEmpty()) out.append("\n云接收轮询：不请求网络（无接收链路）");
        else out.append("\n云接收轮询：").append(nextDelay / 1000L).append(" 秒后");
        String code = pendingReceiverCode(app); if (!code.isEmpty()) out.append("\n云配对码（接收）：").append(code);
        long received = TargetStore.prefs(app).getLong("cloud_receive_last_success", 0L); if (received > 0) out.append("\n最近接收：").append(android.text.format.DateFormat.format("MM-dd HH:mm:ss", received));
        String error = TargetStore.prefs(app).getString("cloud_last_error", ""); if (!error.isEmpty()) out.append("\n最近错误：").append(error);
        String receiveError = TargetStore.prefs(app).getString("cloud_receive_last_error", "");
        if (!receiveError.isEmpty()) out.append("\n最近云接收错误：").append(receiveError);
        return out.toString();
    }

    public static boolean hasNetwork(Context context) {
        try { ConnectivityManager cm = context.getSystemService(ConnectivityManager.class); if (cm == null) return false;
            if (Build.VERSION.SDK_INT >= 23) { NetworkCapabilities caps = cm.getNetworkCapabilities(cm.getActiveNetwork()); return caps != null && caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET); }
        } catch (Exception ignored) {} return true;
    }

    public static void saveRelayUrl(Context context, String relayUrl) { CloudConfigStore.saveRelayUrl(context, normalizeRelayUrl(relayUrl)); }

    private static CloudConfigStore.CloudLink parseLink(String relay, JSONObject response, String role, String privateKey) throws Exception {
        String room = required(response, "roomId"), device = required(response, "deviceId"), token = required(response, "token");
        String peer = required(response, "peerDeviceId"), peerKey = required(response, "peerPublicKey"); CloudCrypto.decodePublicKey(peerKey);
        String peerName = response.optString("peerName", response.optString("peerDeviceName", ""));
        return new CloudConfigStore.CloudLink(relay, room, device, token, peer, peerKey, privateKey, role, peerName);
    }

    private static JSONObject responseObject(HttpResult result) throws Exception {
        if (result.status < 200 || result.status >= 300) throw new IllegalStateException("云配对失败 HTTP " + result.status + ": " + result.body);
        return new JSONObject(result.body);
    }

    private static HttpResult requestJson(String method, String urlString, JSONObject body, String token) throws Exception {
        RelayHttp.Result result = RelayHttp.request(method, urlString, body, token, false);
        return new HttpResult(result.status, result.body);
    }

    private static String normalizeRelayUrl(String value) { String url = value == null || value.trim().isEmpty() ? DEFAULT_RELAY_URL : value.trim(); while (url.endsWith("/")) url = url.substring(0, url.length() - 1); if (!CloudConfigStore.isSecureRelayUrl(url)) throw new IllegalArgumentException("Relay URL 必须使用 HTTPS，且不能包含用户名或密码"); return url; }
    private static String enc(String value) throws Exception { return URLEncoder.encode(value, StandardCharsets.UTF_8.name()); }
    private static String required(JSONObject object, String name) { String value = object.optString(name, "").trim(); if (value.isEmpty()) throw new IllegalStateException("云配对响应缺少 " + name); return value; }
    private static String errorMessage(Exception e) { String message = e.getMessage(); return message == null || message.isEmpty() ? e.getClass().getSimpleName() : message; }
    private static void callback(PairCallback callback, boolean success, String message) { if (callback != null) callback.completed(success, message); }
    private static void fail(Context app, PairCallback callback, String message) { TargetStore.prefs(app).edit().putString("cloud_last_error", message).apply(); callback(callback, false, message); }
    private static final class HttpResult { final int status; final String body; HttpResult(int status, String body) { this.status = status; this.body = body == null ? "" : body; } }
}
