package com.xgy.lansms;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.app.AlarmManager;
import android.app.PendingIntent;
import android.os.Build;
import android.os.SystemClock;

public class BootReceiver extends BroadcastReceiver {
    public static final String ACTION_RESTART_RECEIVER = "com.xgy.lansms.action.RESTART_RECEIVER";
    private static final int RESTART_REQUEST_CODE = 5812304;

    @Override public void onReceive(Context context, Intent intent) {
        String action = intent == null ? null : intent.getAction();
        if (!Intent.ACTION_BOOT_COMPLETED.equals(action)
                && !Intent.ACTION_MY_PACKAGE_REPLACED.equals(action)
                && !ACTION_RESTART_RECEIVER.equals(action)) return;
        ReceiverService.migrateReceiverEnabled(context);
        if (!TargetStore.prefs(context).getBoolean(ReceiverService.PREF_RECEIVER_ENABLED, false)) return;
        ReceiverService.ensureStarted(context);
    }

    /** Best-effort delayed restart for task removal; it never changes receiver_enabled. */
    public static void scheduleReceiverRestart(Context context) {
        AlarmManager alarm = (AlarmManager) context.getApplicationContext().getSystemService(Context.ALARM_SERVICE);
        if (alarm == null) return;
        long at = SystemClock.elapsedRealtime() + 1000L;
        PendingIntent restart = restartPendingIntent(context);
        // Android 12+ refuses background foreground-service starts from inexact alarms
        // unless the app is exempt from battery optimisation; an exact alarm is one of
        // the documented exemptions, so prefer it whenever the system grants it.
        try {
            if (Build.VERSION.SDK_INT < 31 || alarm.canScheduleExactAlarms()) {
                alarm.setExactAndAllowWhileIdle(AlarmManager.ELAPSED_REALTIME_WAKEUP, at, restart);
                return;
            }
        } catch (SecurityException ignored) {
            // Fall through to the inexact alarm used before.
        }
        alarm.setAndAllowWhileIdle(AlarmManager.ELAPSED_REALTIME_WAKEUP, at, restart);
    }

    public static void cancelReceiverRestart(Context context) {
        AlarmManager alarm = (AlarmManager) context.getApplicationContext().getSystemService(Context.ALARM_SERVICE);
        if (alarm != null) alarm.cancel(restartPendingIntent(context));
    }

    private static PendingIntent restartPendingIntent(Context context) {
        Intent intent = new Intent(context.getApplicationContext(), BootReceiver.class)
                .setAction(ACTION_RESTART_RECEIVER);
        int flags = PendingIntent.FLAG_UPDATE_CURRENT;
        if (Build.VERSION.SDK_INT >= 23) flags |= PendingIntent.FLAG_IMMUTABLE;
        return PendingIntent.getBroadcast(context.getApplicationContext(), RESTART_REQUEST_CODE, intent, flags);
    }
}
