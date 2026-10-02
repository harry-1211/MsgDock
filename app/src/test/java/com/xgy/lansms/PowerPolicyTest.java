package com.xgy.lansms;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import org.junit.Test;

/** Pure coverage for battery-aware receiver pacing; sending never depends on it. */
public class PowerPolicyTest {
    private static PowerPolicy.Snapshot screenOn() { return new PowerPolicy.Snapshot(true, false, false); }
    private static PowerPolicy.Snapshot charging() { return new PowerPolicy.Snapshot(false, true, false); }
    private static PowerPolicy.Snapshot idleBattery() { return new PowerPolicy.Snapshot(false, false, false); }
    private static PowerPolicy.Snapshot lowPower() { return new PowerPolicy.Snapshot(false, false, true); }

    @Test public void screenOnOrChargingKeepsFastInterval() {
        assertEquals(3000L, PowerPolicy.idlePollDelay(3000L, 500, screenOn()));
        assertEquals(3000L, PowerPolicy.idlePollDelay(3000L, 500, charging()));
    }

    @Test public void burstWindowStaysFastBeforeRelaxing() {
        assertEquals(3000L, PowerPolicy.idlePollDelay(3000L, 0, idleBattery()));
        assertEquals(3000L, PowerPolicy.idlePollDelay(3000L, PowerPolicy.FAST_ROUNDS - 1, idleBattery()));
        assertEquals(10_000L, PowerPolicy.idlePollDelay(3000L, PowerPolicy.FAST_ROUNDS, idleBattery()));
        assertEquals(15_000L, PowerPolicy.idlePollDelay(3000L, PowerPolicy.MEDIUM_ROUNDS, idleBattery()));
    }

    @Test public void lowPowerUsesOneMinuteAndNeverShortensBaseline() {
        assertEquals(60_000L, PowerPolicy.idlePollDelay(3000L, 0, lowPower()));
        assertEquals(20_000L, PowerPolicy.idlePollDelay(20_000L, 50, idleBattery()));
    }

    @Test public void lowPowerDetection() {
        assertTrue(PowerPolicy.isLowPower(true, 80, false));
        assertTrue(PowerPolicy.isLowPower(false, PowerPolicy.LOW_BATTERY_PERCENT, false));
        assertFalse(PowerPolicy.isLowPower(false, PowerPolicy.LOW_BATTERY_PERCENT + 1, false));
        assertFalse(PowerPolicy.isLowPower(true, 5, true));
        assertFalse(PowerPolicy.isLowPower(false, -1, false));
    }

    @Test public void announceStaysInsideTheSenderScanWindow() {
        assertEquals(2200L, PowerPolicy.announceDelay(screenOn()));
        assertTrue(PowerPolicy.announceDelay(idleBattery()) < 5200L);
        assertTrue(PowerPolicy.announceDelay(lowPower()) < 5200L);
    }

    @Test public void periodicJobRelaxesOnlyWithoutPendingWork() {
        assertEquals(15L * 60L * 1000L, CloudSyncJobService.periodFor(true));
        assertEquals(6L * 60L * 60L * 1000L, CloudSyncJobService.periodFor(false));
    }
}
