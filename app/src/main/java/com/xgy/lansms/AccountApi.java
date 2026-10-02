package com.xgy.lansms;

import android.content.Context;

import org.json.JSONObject;
import org.json.JSONArray;

import java.nio.charset.StandardCharsets;
import java.util.List;
import java.util.concurrent.Executors;
import java.util.concurrent.ScheduledExecutorService;
import java.util.concurrent.ScheduledFuture;
import java.util.concurrent.ThreadFactory;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicBoolean;

/** Minimal native client for the account-sync API at /api/v1. */
public final class AccountApi {
    public static final String DEFAULT_API_URL = RelayHttp.PRIMARY;
    private static final String API_PREFIX = "/api/v1";
    private static final Object RETRY_LOCK = new Object();
    private static final ScheduledExecutorService RETRY_EXECUTOR =
            Executors.newSingleThreadScheduledExecutor(new ThreadFactory() {
                @Override public Thread newThread(Runnable runnable) {
                    Thread thread = new Thread(runnable, "msgdock-account-retry");
                    thread.setDaemon(true);
                    return thread;
                }
            });
    private static final AtomicBoolean ACCOUNT_FLUSHING = new AtomicBoolean(false);
    private static final AtomicBoolean ACCOUNT_POLLING = new AtomicBoolean(false);
    private static ScheduledFuture<?> retryFuture;
    private static long retryDueAt = -1L;

    private AccountApi() {}

    public interface Callback {
        void completed(boolean success, String message);
    }

    public static void register(Context context, String username, String email, String password,
                                Callback callback) {
        Context app = context.getApplicationContext();
        CloudRelay.executor().execute(() -> {
            try {
                require(username, "用户名");
                require(email, "邮箱");
                require(password, "密码");
                JSONObject body = new JSONObject().put("username", username.trim())
                        .put("email", email.trim()).put("password", password);
                HttpResult result = request("POST", endpoint("/auth/register"), body, null, true);
                JSONObject response = successObject(result, "注册失败");
                saveAuthentication(app, response);
                if (!ensureDeviceSync(app)) throw new IllegalStateException("设备注册失败");
                CloudSyncJobService.schedule(app);
                scheduleOutboxFlush(app);
                complete(callback, true, "注册并登录成功，账号同步已启用");
            } catch (Exception e) {
                scheduleAccountRetry(app);
                complete(callback, false, errorMessage(e));
            }
        });
    }

    public static void login(Context context, String identifier, String password, Callback callback) {
        Context app = context.getApplicationContext();
        CloudRelay.executor().execute(() -> {
            try {
                require(identifier, "用户名或邮箱");
                require(password, "密码");
                JSONObject body = new JSONObject().put("identifier", identifier.trim())
                        .put("password", password);
                HttpResult result = request("POST", endpoint("/auth/login"), body, null, true);
                JSONObject response = successObject(result, "登录失败");
                saveAuthentication(app, response);
                if (!ensureDeviceSync(app)) throw new IllegalStateException("设备注册失败");
                CloudSyncJobService.schedule(app);
                scheduleOutboxFlush(app);
                complete(callback, true, "登录成功，账号同步已启用");
            } catch (Exception e) {
                scheduleAccountRetry(app);
                complete(callback, false, errorMessage(e));
            }
        });
    }

    public static void logout(Context context, Callback callback) {
        Context app = context.getApplicationContext();
        String token;
        synchronized (AccountStore.LOCK) {
            token = AccountStore.sessionToken(app);
            AccountOutboxStore.clear(app);
            AccountStore.clear(app);
        }
        CloudRelay.executor().execute(() -> {
            String message = "已退出账号";
            try {
                if (!token.isEmpty()) {
                    HttpResult result = request("POST", endpoint("/auth/logout"), null, token, true);
                    if (result.status < 200 || result.status >= 300) message = "本地已退出账号（服务器响应 HTTP " + result.status + ")";
                }
            } catch (Exception e) {
                message = "本地已退出账号（服务器暂时不可达）";
            }
            complete(callback, true, message);
        });
    }

    /** Ensures a session-authenticated account has a separate device token. */
    public static boolean ensureDeviceSync(Context context) throws Exception {
        Context app = context.getApplicationContext();
        if (AccountStore.authRequired(app)) throw new IllegalStateException("设备授权已失效，请重新登录");
        String session = AccountStore.sessionToken(app);
        if (session.isEmpty()) return false;
        JSONObject body = new JSONObject().put("id", AccountStore.ensureDeviceId(app))
                .put("name", AccountStore.deviceName(app)).put("type", "android");
        HttpResult result = request("POST", endpoint("/devices"), body, session, false);
        if (result.status == 401 || result.status == 403) {
            synchronized (AccountStore.LOCK) {
                if (session.equals(AccountStore.sessionToken(app))) AccountStore.requireLogin(app);
            }
            throw new IllegalStateException("账号会话已失效，请重新登录");
        }
        JSONObject response = successObject(result, "设备注册失败");
        JSONObject device = response.optJSONObject("device");
        String token = response.optString("device_token", "").trim();
        if (token.isEmpty()) throw new IllegalStateException("设备注册响应缺少 device_token");
        String name = device == null ? AccountStore.deviceName(app) : device.optString("name", AccountStore.deviceName(app));
        synchronized (AccountStore.LOCK) {
            if (!session.equals(AccountStore.sessionToken(app))) return false;
            AccountStore.saveDeviceToken(app, token, name);
        }
        return true;
    }

    /** Uploads due account messages. The caller supplies the JobScheduler worker thread. */
    public static boolean flushOutbox(Context context) {
        Context app = context.getApplicationContext();
        int initial = AccountOutboxStore.count(app);
        if (initial < 0) return false;
        if (!AccountStore.hasAccount(app)) return initial == 0;
        if (AccountStore.authRequired(app)) return true; // Wait for explicit login, not automatic re-enrollment.
        if (!ACCOUNT_FLUSHING.compareAndSet(false, true)) return true;
        try {
            try {
                if (!AccountStore.hasDeviceToken(app) && !ensureDeviceSync(app)) {
                    if (initial > 0) markDueFailures(app, "账号设备注册失败");
                    return false;
                }
            } catch (Exception e) {
                android.util.Log.w("MsgDock", "账号设备注册失败", e);
                if (initial > 0) markDueFailures(app, errorMessage(e));
                return false;
            }
            if (initial == 0) return true;
            String uploadUser;
            String uploadToken;
            List<AccountOutboxStore.Entry> entries;
            synchronized (AccountStore.LOCK) {
                uploadUser = AccountStore.userId(app);
                uploadToken = AccountStore.deviceToken(app);
                entries = AccountOutboxStore.due(app, System.currentTimeMillis(), 20);
            }
            for (AccountOutboxStore.Entry entry : entries) {
                if (!uploadUser.equals(AccountStore.userId(app)) || !uploadToken.equals(AccountStore.deviceToken(app))) return false;
                try {
                    JSONObject body = AccountOutboxStore.payload(entry.clientMessageId, entry.sender,
                            entry.body, entry.receivedAt);
                    HttpResult result;
                    SyncClock.beginRequest(); // status bookkeeping only
                    try {
                        result = request("POST", endpoint("/messages"), body, uploadToken, false);
                    } finally {
                        SyncClock.endRequest();
                    }
                    synchronized (AccountStore.LOCK) {
                    if (!uploadUser.equals(AccountStore.userId(app)) || !uploadToken.equals(AccountStore.deviceToken(app))) return false;
                    if (result.status >= 200 && result.status < 300) {
                        AccountOutboxStore.remove(app, entry.clientMessageId);
                        SyncClock.markSuccess(app);
                        SyncClock.markForwarded(app, "账号");
                    } else {
                        if (result.status == 401 || result.status == 403) AccountStore.requireLogin(app);
                        else SyncClock.markFailure();
                        AccountOutboxStore.markFailure(app, entry.clientMessageId, httpError(result));
                    }
                    }
                } catch (Exception e) {
                    SyncClock.markFailure();
                    synchronized (AccountStore.LOCK) {
                        if (uploadUser.equals(AccountStore.userId(app)) && uploadToken.equals(AccountStore.deviceToken(app)))
                            AccountOutboxStore.markFailure(app, entry.clientMessageId, errorMessage(e));
                    }
                }
            }
            return AccountOutboxStore.count(app) == 0;
        } finally {
            ACCOUNT_FLUSHING.set(false);
            scheduleOutboxFlush(app);
        }
    }

    /**
     * Schedules one in-process retry for the earliest persisted account outbox deadline.
     * JobScheduler remains the durable fallback when this process is killed or suspended.
     */
    public static void scheduleOutboxFlush(Context context) {
        Context app = context.getApplicationContext();
        if (!AccountStore.hasAccount(app)) return;
        if (AccountStore.authRequired(app)) return;
        if (AccountOutboxStore.count(app) <= 0) return;
        long next = AccountOutboxStore.nextAttemptAt(app);
        if (next < 0L) return;
        long now = System.currentTimeMillis();
        long dueAt = Math.max(now, next);
        synchronized (RETRY_LOCK) {
            if (retryFuture != null && retryFuture.isDone()) {
                retryFuture = null;
                retryDueAt = -1L;
            }
            if (retryFuture != null && !shouldReplaceRetryTimer(retryDueAt, dueAt)) return;
            if (retryFuture != null) retryFuture.cancel(false);
            retryDueAt = dueAt;
            retryFuture = RETRY_EXECUTOR.schedule(() -> runScheduledFlush(app, dueAt),
                    Math.max(0L, dueAt - System.currentTimeMillis()), TimeUnit.MILLISECONDS);
        }
    }

    static boolean shouldReplaceRetryTimer(long currentDueAt, long requestedDueAt) {
        return currentDueAt < 0L || requestedDueAt < currentDueAt;
    }

    private static void runScheduledFlush(Context app, long scheduledDueAt) {
        synchronized (RETRY_LOCK) {
            if (retryDueAt != scheduledDueAt) return;
            retryFuture = null;
            retryDueAt = -1L;
        }
        flushOutbox(app);
    }

    private static void markDueFailures(Context context, String error) {
        long now = System.currentTimeMillis();
        for (AccountOutboxStore.Entry entry : AccountOutboxStore.due(context, now, 1000)) {
            AccountOutboxStore.markFailure(context, entry.clientMessageId, error);
        }
    }

    private static void scheduleAccountRetry(Context context) {
        if (!AccountStore.hasAccount(context)) return;
        CloudSyncJobService.schedule(context);
        scheduleOutboxFlush(context);
    }

    public static String statusText(Context context) {
        return AccountStore.statusText(context);
    }

    /** One bounded HTTPS page; the foreground service schedules subsequent pages independently of LAN. */
    public static boolean pollInbox(Context context) {
        Context app = context.getApplicationContext();
        if (!AccountStore.receiveEnabled(app) || !AccountStore.hasAccount(app)) return true;
        if (AccountStore.authRequired(app)) return false;
        if (!ACCOUNT_POLLING.compareAndSet(false, true)) return true;
        String user = AccountStore.userId(app);
        String token = "";
        try {
            if (!AccountStore.hasDeviceToken(app) && !ensureDeviceSync(app)) return false;
            token = AccountStore.deviceToken(app);
            long after = AccountStore.lastSeq(app);
            JSONObject response = successObject(request("GET", endpoint("/messages?after=" + after + "&limit=100"),
                    null, token, false), "接收失败");
            JSONArray rows = response.getJSONArray("messages");
            validatePage(rows, after);
            String receivedFrom = null;
            synchronized (AccountStore.LOCK) {
                if (!sameReceiver(app, user, token)) return true; // Discard late responses after logout/account switch.
                for (int i = 0; i < rows.length(); i++) {
                    JSONObject sms = accountMessage(user, AccountStore.deviceId(app), rows.getJSONObject(i));
                    if (!CloudInboxStore.acceptAccount(app.getFilesDir(), sms))
                        throw new IllegalStateException("无法保存账号收件箱，稍后重试");
                    // History and pending notification must be durable BEFORE committing the cursor.
                    AccountStore.saveLastSeq(app, sms.getLong("seq"));
                    if (!sms.optBoolean("silent", false)) receivedFrom = sms.optString("device", "");
                }
                AccountStore.setReceiveStatus(app, "已连接 · 收件进度 " + AccountStore.lastSeq(app));
            }
            if (receivedFrom != null) SyncClock.markReceived(app, receivedFrom);
            SyncClock.markSuccess(app);
            return true;
        } catch (Exception e) {
            boolean auth = e instanceof ApiException && (((ApiException)e).status == 401 || ((ApiException)e).status == 403);
            if (!auth) SyncClock.markFailure();
            synchronized (AccountStore.LOCK) {
                if (sameReceiver(app, user, token)) {
                    if (auth) AccountStore.requireLogin(app);
                    AccountStore.setReceiveStatus(app, "暂未连接，自动重试");
                }
            }
            return false;
        } finally { ACCOUNT_POLLING.set(false); }
    }

    private static boolean sameReceiver(Context context, String user, String token) {
        return AccountStore.receiveEnabled(context) && !user.isEmpty()
                && user.equals(AccountStore.userId(context)) && token.equals(AccountStore.deviceToken(context));
    }

    static void validatePage(JSONArray rows, long after) throws Exception {
        for (int i = 0; i < rows.length(); i++) {
            long seq = rows.getJSONObject(i).getLong("seq");
            if (seq <= after) throw new IllegalArgumentException("收件序号无效");
            after = seq;
        }
    }

    static JSONObject accountMessage(String user, String deviceId, JSONObject row) throws Exception {
        String uuid = row.getString("client_message_id");
        if (user.isEmpty() || uuid.isEmpty() || row.getLong("seq") <= 0) throw new IllegalArgumentException("收件标识无效");
        JSONObject device = row.optJSONObject("source_device");
        boolean own = device != null && !deviceId.isEmpty() && deviceId.equals(device.optString("id"));
        return new JSONObject().put("id", "account:" + user + ":" + row.getLong("seq"))
                .put("deliveryId", uuid).put("accountUserId", user).put("source", "account")
                .put("seq", row.getLong("seq")).put("from", row.getString("sender"))
                .put("text", row.getString("body")).put("receivedAt", row.getLong("received_at"))
                .put("device", device == null ? "已移除设备" : device.optString("name", "Android"))
                .put("silent", own);
    }

    private static void saveAuthentication(Context context, JSONObject response) {
        synchronized (AccountStore.LOCK) {
        JSONObject user = response.optJSONObject("user");
        String session = response.optString("session_token", "").trim();
        if (session.isEmpty()) throw new IllegalStateException("账号响应缺少 session_token");
        String userId = user == null ? "" : user.optString("id", "");
        // Never upload messages captured while another account was active to the
        // newly authenticated account.  Same-account re-login keeps its queue.
        String previousUserId = AccountStore.userId(context);
        if (!previousUserId.isEmpty() && !previousUserId.equals(userId)) {
            AccountOutboxStore.clear(context);
        }
        AccountStore.saveAuthentication(context,
                userId,
                user == null ? "" : user.optString("username", ""),
                user == null ? "" : user.optString("email", ""),
                session, response.optLong("expires_at", 0L));
        }
    }

    private static JSONObject successObject(HttpResult result, String prefix) throws Exception {
        if (result.status < 200 || result.status >= 300) {
            throw new ApiException(result.status, prefix + " HTTP " + result.status
                    + (result.body.isEmpty() ? "" : "：" + apiError(result.body)));
        }
        return result.body.trim().isEmpty() ? new JSONObject() : new JSONObject(result.body);
    }

    private static HttpResult request(String method, String urlString, JSONObject body, String token, boolean nativeAuth) throws Exception {
        RelayHttp.Result result = RelayHttp.request(method, urlString, body, token, nativeAuth);
        return new HttpResult(result.status, result.body);
    }

    private static String endpoint(String path) {
        return DEFAULT_API_URL + API_PREFIX + (path.startsWith("/") ? path : "/" + path);
    }

    private static String apiError(String body) {
        try {
            JSONObject value = new JSONObject(body);
            String error = value.optString("error", "");
            if (!error.isEmpty()) return error;
            String message = value.optString("message", "");
            if (!message.isEmpty()) return message;
        } catch (Exception ignored) { }
        return body.length() > 256 ? body.substring(0, 256) : body;
    }

    private static String httpError(HttpResult result) {
        return "账号消息上传 HTTP " + result.status
                + (result.body.isEmpty() ? "" : "：" + apiError(result.body));
    }


    private static void require(String value, String label) {
        if (value == null || value.trim().isEmpty()) throw new IllegalArgumentException(label + "不能为空");
    }

    private static void complete(Callback callback, boolean success, String message) {
        if (callback == null) return;
        try {
            new android.os.Handler(android.os.Looper.getMainLooper()).post(() -> callback.completed(success, message));
        } catch (RuntimeException ignored) {
            callback.completed(success, message);
        }
    }

    private static String errorMessage(Exception e) {
        String value = e.getMessage();
        return value == null || value.isEmpty() ? e.getClass().getSimpleName() : value;
    }

    private static final class HttpResult {
        final int status;
        final String body;
        HttpResult(int status, String body) {
            this.status = status;
            this.body = body == null ? "" : body;
        }
    }

    private static final class ApiException extends Exception {
        final int status;
        ApiException(int status, String message) { super(message); this.status = status; }
    }
}
