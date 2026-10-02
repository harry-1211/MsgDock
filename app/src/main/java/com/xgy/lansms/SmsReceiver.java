package com.xgy.lansms;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.os.PowerManager;
import android.provider.Telephony;
import android.telephony.SmsMessage;
import java.util.UUID;

public class SmsReceiver extends BroadcastReceiver {
    /** Stays well inside the broadcast timeout; unsent cloud copies remain queued on disk. */
    static final long FORWARD_BUDGET_MS = 8_000L;

    @Override public void onReceive(Context context, Intent intent) {
        if (!Telephony.Sms.Intents.SMS_RECEIVED_ACTION.equals(intent.getAction())) return;
        final PendingResult pending = goAsync();
        // A short, bounded wake lock keeps the CPU up while the posts finish; with the
        // screen off the phone could otherwise suspend mid-request.
        PowerManager.WakeLock wake = null;
        try {
            PowerManager pm = context.getSystemService(PowerManager.class);
            if (pm != null) {
                wake = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "MsgDock:sms-forward");
                wake.setReferenceCounted(false);
                wake.acquire(FORWARD_BUDGET_MS + 2_000L);
            }
        } catch (RuntimeException ignored) {
            wake = null;
        }
        final PowerManager.WakeLock heldWake = wake;
        new Thread(() -> {
            try {
                SmsMessage[] msgs = Telephony.Sms.Intents.getMessagesFromIntent(intent);
                if (msgs == null || msgs.length == 0) return;
                String from = msgs[0].getDisplayOriginatingAddress();
                StringBuilder text = new StringBuilder();
                for (SmsMessage m : msgs) text.append(m.getDisplayMessageBody());
                int sim = intent.getIntExtra("subscription", intent.getIntExtra("slot", -1));
                Forwarder.forwardAndWait(context.getApplicationContext(), UUID.randomUUID().toString(), from,
                        text.toString(), System.currentTimeMillis(), sim, FORWARD_BUDGET_MS);
            } finally {
                if (heldWake != null) {
                    try { heldWake.release(); } catch (RuntimeException ignored) {}
                }
                pending.finish();
            }
        }, "sms-forward").start();
    }
}
