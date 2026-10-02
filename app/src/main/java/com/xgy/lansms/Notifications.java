package com.xgy.lansms;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.content.Context;
import android.os.Build;

public final class Notifications {
    public static final String SERVICE_CH = "receiver_service";
    public static final String SMS_CH = "received_sms";
    private Notifications() {}

    public static void ensureChannels(Context c) {
        if (Build.VERSION.SDK_INT >= 26) {
            NotificationManager nm = c.getSystemService(NotificationManager.class);
            if (nm == null) return;
            nm.createNotificationChannel(new NotificationChannel(SERVICE_CH, "局域网与云端接收服务", NotificationManager.IMPORTANCE_LOW));
            NotificationChannel sms = new NotificationChannel(SMS_CH, "转发短信", NotificationManager.IMPORTANCE_HIGH);
            sms.setDescription("从手机经局域网转发过来的短信");
            nm.createNotificationChannel(sms);
        }
    }

    /** Ongoing-notification text; the service compares it to skip identical re-posts. */
    public static String serviceText(Context c, String code) {
        int cloudReceivers = CloudConfigStore.receiverLinks(c).size();
        String cloud = AccountStore.receiveEnabled(c) && AccountStore.hasAccount(c) ? "账号接收已开启"
                : cloudReceivers == 0 ? "云接收未配对" : "云接收 " + cloudReceivers + " 条链路";
        return "端口 58123 · 局域网配对码 " + code + " · " + cloud;
    }

    public static Notification serviceNotification(Context c, String code) {
        ensureChannels(c);
        return new Notification.Builder(c, SERVICE_CH)
                .setSmallIcon(android.R.drawable.stat_notify_sync)
                .setContentTitle("MsgDock 接收端运行中")
                .setContentText(serviceText(c, code))
                .setOngoing(true)
                .build();
    }

    /** Returns false when Windows-style delivery cannot be confirmed by Android. */
    public static boolean showSms(Context c, String from, String text, String device) {
        ensureChannels(c);
        NotificationManager nm = c.getSystemService(NotificationManager.class);
        if (nm == null || !nm.areNotificationsEnabled()) return false;
        if (Build.VERSION.SDK_INT >= 33 && c.checkSelfPermission("android.permission.POST_NOTIFICATIONS")
                != android.content.pm.PackageManager.PERMISSION_GRANTED) return false;
        if (Build.VERSION.SDK_INT >= 26) {
            NotificationChannel channel = nm.getNotificationChannel(SMS_CH);
            if (channel == null || channel.getImportance() == NotificationManager.IMPORTANCE_NONE) return false;
        }
        String sender = from == null || from.isEmpty() ? "短信" : from;
        // An explicit OTP goes into the title so it is readable without expanding the card.
        String code = OtpCode.find(text);
        // Lock screen with "hide sensitive content": show that an SMS arrived, never its body.
        Notification lockScreen = new Notification.Builder(c, SMS_CH)
                .setSmallIcon(android.R.drawable.sym_action_email)
                .setContentTitle("MsgDock")
                .setContentText("收到 1 条短信")
                .build();
        int id = (int)(System.currentTimeMillis() & 0x7fffffff);
        Notification.Builder b = new Notification.Builder(c, SMS_CH)
                .setSmallIcon(android.R.drawable.sym_action_email)
                .setContentTitle(code.isEmpty() ? sender : sender + " · 验证码 " + code)
                .setContentText(text)
                .setStyle(new Notification.BigTextStyle().bigText(text))
                .setSubText(device)
                .setCategory(Notification.CATEGORY_MESSAGE)
                .setVisibility(Notification.VISIBILITY_PRIVATE)
                .setPublicVersion(lockScreen)
                .setContentIntent(android.app.PendingIntent.getActivity(c, 0,
                    new android.content.Intent(c, MainActivity.class), android.app.PendingIntent.FLAG_IMMUTABLE | android.app.PendingIntent.FLAG_UPDATE_CURRENT))
                .setAutoCancel(true);
        if (!code.isEmpty()) {
            // Explicit, non-exported receiver; the per-notification request code keeps
            // each code's PendingIntent distinct instead of overwriting the previous one.
            android.content.Intent copy = new android.content.Intent(c, OtpCopyReceiver.class)
                    .setAction(OtpCopyReceiver.ACTION_COPY)
                    .putExtra(OtpCopyReceiver.EXTRA_CODE, code);
            android.app.PendingIntent pending = android.app.PendingIntent.getBroadcast(c, id, copy,
                    android.app.PendingIntent.FLAG_IMMUTABLE | android.app.PendingIntent.FLAG_UPDATE_CURRENT);
            b.addAction(new Notification.Action.Builder(
                    android.graphics.drawable.Icon.createWithResource(c, android.R.drawable.ic_menu_save),
                    "复制验证码", pending).build());
        }
        try {
            nm.notify(id, b.build());
            return true;
        } catch (RuntimeException ignored) {
            return false;
        }
    }

    /** Emits a short operational notification used for device-backup recovery. */
    public static boolean showDeviceNotice(Context c, String title, String text) {
        ensureChannels(c);
        NotificationManager nm = c.getSystemService(NotificationManager.class);
        if (nm == null || !nm.areNotificationsEnabled()) return false;
        if (Build.VERSION.SDK_INT >= 33 && c.checkSelfPermission("android.permission.POST_NOTIFICATIONS")
                != android.content.pm.PackageManager.PERMISSION_GRANTED) return false;
        Notification.Builder b = new Notification.Builder(c, SMS_CH)
                .setSmallIcon(android.R.drawable.stat_notify_sync)
                .setContentTitle(title == null || title.isEmpty() ? "MsgDock" : title)
                .setContentText(text == null ? "" : text)
                .setAutoCancel(true);
        try {
            nm.notify((int)(System.currentTimeMillis() & 0x7fffffff), b.build());
            return true;
        } catch (RuntimeException ignored) { return false; }
    }
}
