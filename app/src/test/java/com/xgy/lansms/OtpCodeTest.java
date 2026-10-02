package com.xgy.lansms;

import static org.junit.Assert.assertEquals;

import org.junit.Test;

public class OtpCodeTest {
    @Test public void findsCodeNextToKeyword() {
        assertEquals("123456", OtpCode.find("【某银行】您的验证码为 123456，5 分钟内有效。"));
        assertEquals("8812", OtpCode.find("8812 是您的动态码，请勿泄露"));
        assertEquals("654321", OtpCode.find("Your verification code is 654321."));
    }

    @Test public void ignoresNumbersWithoutKeyword() {
        assertEquals("", OtpCode.find("您的订单 20261001 已发货，运单号 7788990011"));
        assertEquals("", OtpCode.find("本月话费 128 元"));
        assertEquals("", OtpCode.find(null));
    }

    @Test public void doesNotCutLongerDigitRuns() {
        assertEquals("", OtpCode.find("验证码 1234567890123"));
    }
}
