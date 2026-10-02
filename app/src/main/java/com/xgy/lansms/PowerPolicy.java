package com.xgy.lansms;

import android.content.Context;
import android.os.BatteryManager;
import android.os.PowerManager;

/**
 * Battery-aware pacing for the receiver loops. Sending stays event-driven: an SMS
 * broadcast never waits for any of these intervals. Only idle polling and LAN
 * announcements are relaxed, and any new message, screen-on or charger event
 * returns the loops to their fast interval.
 */
public final class PowerPolicy {
    /** Battery percentage at or below which idle polling drops to the low-power interval. */
    static final int LOW_BATTERY_PERCENT = 15;
    /** Empty polls at the fast interval before relaxing, so a burst of SMS stays instant. */
    static final int FAST_ROUNDS = 10;
    static final int MEDIUM_ROUNDS = 30;
    static final long MEDIUM_IDLE_MS = 10_000L;
    static final long SLOW_IDLE_MS = 15_000L;
    static final long LOW_POWER_IDLE_MS = 60_000L;
    static final long ANNOUNCE_ACTIVE_MS = 2_200L;
    /** Still shorter than the 5.2 s sender scan window, so a scan always sees one packet. */
    static final long ANNOUNCE_IDLE_MS = 4_000L;

    private PowerPolicy() {}

    public static final class Snapshot {
        public final boolean interactive;
        public final boolean charging;
        public final boolean lowPower;

        Snapshot(boolean interactive, boolean charging, boolean lowPower) {
            this.interactive = interactive;
            this.charging = charging;
            this.lowPower = lowPower;
        }

        /** Screen on or plugged in: keep the original fast pacing. */
        boolean active() { return interactive || charging; }
    }

    public static Snapshot read(Context context) {
        boolean interactive = true;
        boolean powerSave = false;
        boolean charging = false;
        int percent = -1;
        try {
            PowerManager pm = context.getSystemService(PowerManager.class);
            if (pm != null) {
                interactive = pm.isInteractive();
                powerSave = pm.isPowerSaveMode();
            }
            BatteryManager bm = context.getSystemService(BatteryManager.class);
            if (bm != null) {
                charging = bm.isCharging();
                percent = bm.getIntProperty(BatteryManager.BATTERY_PROPERTY_CAPACITY);
            }
        } catch (RuntimeException ignored) {
            // Unknown power state keeps the fast, previously shipped behaviour.
        }
        return new Snapshot(interactive, charging, isLowPower(powerSave, percent, charging));
    }

    static boolean isLowPower(boolean powerSave, int batteryPercent, boolean charging) {
        if (charging) return false;
        return powerSave || (batteryPercent >= 0 && batteryPercent <= LOW_BATTERY_PERCENT);
    }

    /**
     * Delay before the next successful-but-empty poll. {@code fastDelay} is the loop's
     * normal interval; the result is never shorter than it.
     */
    static long idlePollDelay(long fastDelay, int emptyRounds, Snapshot power) {
        if (power.active()) return fastDelay;
        if (power.lowPower) return Math.max(fastDelay, LOW_POWER_IDLE_MS);
        if (emptyRounds < FAST_ROUNDS) return fastDelay;
        if (emptyRounds < MEDIUM_ROUNDS) return Math.max(fastDelay, MEDIUM_IDLE_MS);
        return Math.max(fastDelay, SLOW_IDLE_MS);
    }

    static long announceDelay(Snapshot power) {
        return power.active() ? ANNOUNCE_ACTIVE_MS : ANNOUNCE_IDLE_MS;
    }
}
