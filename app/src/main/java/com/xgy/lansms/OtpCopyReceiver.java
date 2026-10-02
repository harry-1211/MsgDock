package com.xgy.lansms;

import android.content.BroadcastReceiver;
import android.content.ClipData;
import android.content.ClipDescription;
import android.content.ClipboardManager;
import android.content.Context;
import android.content.Intent;
import android.os.Build;
import android.os.PersistableBundle;
import android.widget.Toast;

/**
 * "复制验证码" action on an SMS notification. Not exported: only our own explicit
 * PendingIntent can reach it. The code travels inside the intent, so nothing is
 * read back from storage and the clipboard is marked sensitive on Android 13+.
 */
public final class OtpCopyReceiver extends BroadcastReceiver {
    public static final String ACTION_COPY = "com.xgy.lansms.action.COPY_OTP";
    public static final String EXTRA_CODE = "code";

    @Override public void onReceive(Context context, Intent intent) {
        if (intent == null || !ACTION_COPY.equals(intent.getAction())) return;
        String code = intent.getStringExtra(EXTRA_CODE);
        if (code == null || code.isEmpty()) return;
        if (!copy(context, code)) return;
        // Android 13+ shows its own clipboard confirmation; older versions need a hint.
        if (Build.VERSION.SDK_INT < 33) Toast.makeText(context, "已复制验证码", Toast.LENGTH_SHORT).show();
    }

    static boolean copy(Context context, String code) {
        ClipboardManager clipboard = context.getSystemService(ClipboardManager.class);
        if (clipboard == null) return false;
        ClipData clip = ClipData.newPlainText("MsgDock", code);
        if (Build.VERSION.SDK_INT >= 33) {
            PersistableBundle extras = new PersistableBundle();
            extras.putBoolean(ClipDescription.EXTRA_IS_SENSITIVE, true);
            clip.getDescription().setExtras(extras);
        }
        try {
            clipboard.setPrimaryClip(clip);
            return true;
        } catch (RuntimeException e) {
            return false;
        }
    }
}
