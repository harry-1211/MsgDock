package com.xgy.lansms;

import android.content.Context;
import org.json.JSONArray;
import org.json.JSONObject;
import java.io.File;
import java.io.FileInputStream;
import java.io.FileOutputStream;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.List;

/** Small durable JSON outbox. It intentionally does not prune messages. */
public final class CloudOutboxStore {
    private static final Object LOCK = new Object();
    private static final String FILE_NAME = "cloud-outbox.json";
    private static final String DEAD_FILE_NAME = "cloud-outbox-dead-letter.jsonl";
    private static final String KEY_LAST_ERROR = "cloud_last_error";
    private static final String KEY_LAST_SUCCESS = "cloud_last_success";
    private CloudOutboxStore() {}

    public static void enqueue(Context context, JSONObject envelope) throws Exception {
        synchronized (LOCK) {
            try {
                List<Entry> entries = readLocked(context);
                String id = envelope.getString("id");
                String target = envelope.optString("targetDeviceId", "");
                String key = target + "\u0000" + id;
                for (Entry entry : entries) if (key.equals(entry.key)) return;
                entries.add(new Entry(key, id, envelope.toString(), 0, 0L, ""));
                if (!writeLocked(context, entries)) throw new IOException("无法持久化云端 outbox");
            } catch (Exception e) {
                recordError(context, "云端 outbox 写入失败（原文件已保留）：" + errorMessage(e));
                throw e;
            }
        }
    }

    public static List<Entry> due(Context context, long now, int limit) {
        synchronized (LOCK) {
            List<Entry> result = new ArrayList<>();
            try {
                for (Entry entry : readLocked(context)) {
                    if (entry.nextAttemptAt <= now && result.size() < limit) result.add(entry);
                }
            } catch (OutboxCorruptException ignored) {
                // readLocked recorded the error and deliberately did not touch the file.
            }
            return result;
        }
    }

    public static void remove(Context context, String key) {
        synchronized (LOCK) {
            try {
                List<Entry> entries = readLocked(context);
                java.util.Iterator<Entry> iterator = entries.iterator();
                while (iterator.hasNext()) {
                    Entry entry = iterator.next();
                    if (key.equals(entry.key) || key.equals(entry.id)) iterator.remove();
                }
                if (!writeLocked(context, entries)) recordError(context, "云端 outbox 更新失败（原文件已保留）");
            } catch (OutboxCorruptException ignored) {
                // Never replace a corrupt file with an empty or partially parsed list.
            }
        }
    }

    public static void markFailure(Context context, String key, String error) {
        synchronized (LOCK) {
            boolean corrupt = false;
            try {
                List<Entry> entries = readLocked(context);
                for (Entry entry : entries) {
                    if (key.equals(entry.key) || key.equals(entry.id)) {
                        entry.attempts++;
                        long delay = Math.min(6L * 60L * 60L * 1000L,
                                15_000L * (1L << Math.min(8, Math.max(0, entry.attempts - 1))));
                        entry.nextAttemptAt = System.currentTimeMillis() + delay;
                        entry.lastError = error == null ? "unknown error" : error;
                        break;
                    }
                }
                if (!writeLocked(context, entries)) recordError(context, "云端 outbox 更新失败（原文件已保留）");
            } catch (OutboxCorruptException ignored) {
                // Keep the corrupt file and the previously recorded diagnostic intact.
                corrupt = true;
            }
            if (!corrupt) recordError(context, error == null ? "unknown error" : error);
        }
    }

    /** Moves a permanently rejected envelope out of the active retry queue. */
    public static void moveToDeadLetter(Context context, String key, String error) {
        synchronized (LOCK) {
            try {
                List<Entry> entries = readLocked(context);
                Entry found = null;
                for (Entry entry : entries) {
                    if (key.equals(entry.key) || key.equals(entry.id)) { found = entry; break; }
                }
                if (found == null) {
                    recordError(context, error == null ? "云端消息已进入死信队列" : error);
                    return;
                }
                appendDeadLetterLocked(context, found, error);
                java.util.Iterator<Entry> iterator = entries.iterator();
                while (iterator.hasNext()) {
                    Entry entry = iterator.next();
                    if (key.equals(entry.key) || key.equals(entry.id)) iterator.remove();
                }
                if (!writeLocked(context, entries)) {
                    recordError(context, "云端死信迁移后活动队列更新失败（原文件已保留）");
                } else {
                    recordError(context, error == null ? "云端消息已移入死信队列" : error);
                }
            } catch (OutboxCorruptException ignored) {
                // Keep a corrupt active queue untouched for manual recovery.
            } catch (Exception e) {
                recordError(context, "云端死信迁移失败：" + errorMessage(e));
            }
        }
    }

    /** Moves active messages for a deleted sender link out of the retry queue. */
    public static void moveForLinkToDeadLetter(Context context, CloudConfigStore.CloudLink link, String reason) {
        if (link == null) return;
        synchronized (LOCK) {
            try {
                List<Entry> entries = readLocked(context);
                List<Entry> matches = new ArrayList<>();
                for (Entry entry : entries) {
                    try {
                        JSONObject envelope = new JSONObject(entry.envelope);
                        if (link.roomId.equals(envelope.optString("roomId", ""))
                                && link.deviceId.equals(envelope.optString("senderDeviceId", ""))
                                && link.peerDeviceId.equals(envelope.optString("targetDeviceId", ""))) matches.add(entry);
                    } catch (Exception ignored) { }
                }
                if (matches.isEmpty()) return;
                for (Entry entry : matches) appendDeadLetterLocked(context, entry,
                        reason == null ? "关联云链路已删除" : reason);
                entries.removeAll(matches);
                if (!writeLocked(context, entries)) recordError(context, "删除云链路后 outbox 清理失败（原文件已保留）");
            } catch (OutboxCorruptException ignored) {
                // Keep a corrupt active queue untouched for manual recovery.
            } catch (Exception e) {
                recordError(context, "删除云链路后 outbox 清理失败：" + errorMessage(e));
            }
        }
    }

    public static int count(Context context) {
        synchronized (LOCK) {
            try { return readLocked(context).size(); }
            catch (OutboxCorruptException ignored) { return -1; }
        }
    }

    public static int deadLetterCount(Context context) {
        synchronized (LOCK) {
            File file = new File(context.getFilesDir(), DEAD_FILE_NAME);
            if (!file.isFile()) return 0;
            int count = 0;
            try (java.io.BufferedReader reader = new java.io.BufferedReader(new java.io.InputStreamReader(
                    new FileInputStream(file), StandardCharsets.UTF_8))) {
                while (reader.readLine() != null) count++;
            } catch (Exception ignored) { return -1; }
            return count;
        }
    }

    /**
     * Newest-first dead letters for the home screen. Envelopes are ciphertext, so a
     * row only carries the time, the attempt count and the reason; nothing is
     * modified or pruned.
     */
    public static List<JSONObject> deadLetters(Context context, int limit) {
        synchronized (LOCK) {
            List<JSONObject> out = new ArrayList<>();
            File file = new File(context.getFilesDir(), DEAD_FILE_NAME);
            if (!file.isFile() || limit <= 0) return out;
            try (java.io.BufferedReader reader = new java.io.BufferedReader(new java.io.InputStreamReader(
                    new FileInputStream(file), StandardCharsets.UTF_8))) {
                String line;
                while ((line = reader.readLine()) != null) {
                    try { out.add(new JSONObject(line)); } catch (Exception ignored) { }
                }
            } catch (Exception ignored) { }
            java.util.Collections.reverse(out);
            return out.size() > limit ? new ArrayList<>(out.subList(0, limit)) : out;
        }
    }

    public static String lastError(Context context) {
        return TargetStore.prefs(context).getString(KEY_LAST_ERROR, "");
    }

    public static long lastSuccess(Context context) {
        return TargetStore.prefs(context).getLong(KEY_LAST_SUCCESS, 0L);
    }

    public static void markSuccess(Context context) {
        TargetStore.prefs(context).edit().putLong(KEY_LAST_SUCCESS, System.currentTimeMillis()).remove(KEY_LAST_ERROR).apply();
    }

    private static List<Entry> readLocked(Context context) {
        List<Entry> result = new ArrayList<>();
        File file = new File(context.getFilesDir(), FILE_NAME);
        if (!file.isFile()) return result;
        try (FileInputStream input = new FileInputStream(file)) {
            ByteArrayOutputStream output = new ByteArrayOutputStream();
            byte[] buffer = new byte[4096];
            int n;
            while ((n = input.read(buffer)) >= 0) output.write(buffer, 0, n);
            byte[] bytes = output.toByteArray();
            JSONArray array = new JSONArray(new String(bytes, StandardCharsets.UTF_8));
            for (int i = 0; i < array.length(); i++) {
                JSONObject value = array.optJSONObject(i);
                if (value == null) throw new IOException("第 " + i + " 项不是对象");
                String id = value.optString("id", "");
                String envelope = value.optString("envelope", "");
                if (id.isEmpty() || envelope.isEmpty()) throw new IOException("第 " + i + " 项缺少 id 或 envelope");
                JSONObject parsedEnvelope = new JSONObject(envelope);
                if (!id.equals(parsedEnvelope.optString("id", ""))) throw new IOException("第 " + i + " 项 id 不一致");
                int attempts = value.has("attempts") ? value.getInt("attempts") : 0;
                long nextAttemptAt = value.has("nextAttemptAt") ? value.getLong("nextAttemptAt") : 0L;
                if (attempts < 0 || nextAttemptAt < 0L) throw new IOException("第 " + i + " 项重试状态无效");
                String target = parsedEnvelope.optString("targetDeviceId", "");
                String key = value.optString("key", target + "\u0000" + id);
                result.add(new Entry(key, id, envelope, attempts,
                        nextAttemptAt, value.optString("lastError", "")));
            }
        } catch (Exception e) {
            String message = "云端 outbox 读取失败，原文件已保留：" + errorMessage(e);
            recordError(context, message);
            throw new OutboxCorruptException(message, e);
        }
        return result;
    }

    private static void recordError(Context context, String message) {
        TargetStore.prefs(context.getApplicationContext()).edit()
                .putString(KEY_LAST_ERROR, message == null ? "unknown error" : message).apply();
    }

    private static String errorMessage(Exception e) {
        String message = e.getMessage();
        return message == null || message.isEmpty() ? e.getClass().getSimpleName() : message;
    }

    private static boolean writeLocked(Context context, List<Entry> entries) {
        File file = new File(context.getFilesDir(), FILE_NAME);
        File temp = new File(context.getFilesDir(), FILE_NAME + ".tmp");
        JSONArray array = new JSONArray();
        try {
            for (Entry entry : entries) {
                JSONObject value = new JSONObject();
                value.put("key", entry.key);
                value.put("id", entry.id);
                value.put("envelope", entry.envelope);
                value.put("attempts", entry.attempts);
                value.put("nextAttemptAt", entry.nextAttemptAt);
                value.put("lastError", entry.lastError == null ? "" : entry.lastError);
                array.put(value);
            }
            try (FileOutputStream output = new FileOutputStream(temp, false)) {
                output.write(array.toString().getBytes(StandardCharsets.UTF_8));
                output.flush();
                output.getFD().sync();
            }
            if (!temp.renameTo(file)) {
                try (FileOutputStream output = new FileOutputStream(file, false)) {
                    output.write(array.toString().getBytes(StandardCharsets.UTF_8));
                    output.flush();
                    output.getFD().sync();
                }
                temp.delete();
            }
            return true;
        } catch (Exception ignored) { return false; }
    }

    private static void appendDeadLetterLocked(Context context, Entry entry, String error) throws IOException {
        File file = new File(context.getFilesDir(), DEAD_FILE_NAME);
        if (file.isFile()) {
            try (java.io.BufferedReader reader = new java.io.BufferedReader(new java.io.InputStreamReader(
                    new FileInputStream(file), StandardCharsets.UTF_8))) {
                String line;
                while ((line = reader.readLine()) != null) {
                    try { if (entry.key.equals(new JSONObject(line).optString("key", ""))) return; }
                    catch (Exception ignored) { }
                }
            }
        }
        JSONObject value = new JSONObject();
        try {
            value.put("key", entry.key);
            value.put("id", entry.id);
            value.put("envelope", entry.envelope);
            value.put("attempts", entry.attempts);
            value.put("lastError", entry.lastError == null ? "" : entry.lastError);
            value.put("deadLetterAt", System.currentTimeMillis());
            value.put("deadLetterReason", error == null ? "unknown error" : error);
        } catch (Exception e) { throw new IOException("死信记录构造失败", e); }
        if (!appendLine(file, value.toString())) throw new IOException("死信记录写入失败");
    }

    private static boolean appendLine(File file, String line) {
        try (FileOutputStream output = new FileOutputStream(file, true)) {
            output.write((line + "\n").getBytes(StandardCharsets.UTF_8));
            output.flush();
            output.getFD().sync();
            return true;
        } catch (Exception ignored) { return false; }
    }

    public static final class Entry {
        public final String key;
        public final String id;
        public final String envelope;
        public int attempts;
        public long nextAttemptAt;
        public String lastError;

        Entry(String key, String id, String envelope, int attempts, long nextAttemptAt, String lastError) {
            this.key = key; this.id = id; this.envelope = envelope; this.attempts = attempts;
            this.nextAttemptAt = nextAttemptAt; this.lastError = lastError;
        }
    }

    public static final class OutboxCorruptException extends RuntimeException {
        OutboxCorruptException(String message, Throwable cause) { super(message, cause); }
    }
}
