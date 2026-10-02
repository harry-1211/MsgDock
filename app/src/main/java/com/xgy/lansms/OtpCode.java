package com.xgy.lansms;

import java.util.regex.Matcher;
import java.util.regex.Pattern;

/**
 * Verification-code detection for one-tap copy buttons. Mirrors the Windows
 * client's automatic rule: the digits must sit next to an explicit OTP keyword,
 * so order numbers, amounts and phone numbers are not offered as codes.
 */
final class OtpCode {
    private static final String KEYWORDS =
            "验证码|校验码|动态码|验证代码|确认码|verification code|security code|one.time (?:password|code)|otp";
    private static final Pattern AFTER_KEYWORD = Pattern.compile(
            "(?i)(?:" + KEYWORDS + ")[^0-9\\r\\n]{0,20}([0-9]{4,8})(?:[^0-9]|$)");
    private static final Pattern BEFORE_KEYWORD = Pattern.compile(
            "(?i)(?:^|[^0-9])([0-9]{4,8})[^0-9\\r\\n]{0,20}(?:" + KEYWORDS + ")");

    private OtpCode() {}

    /** Returns the code or an empty string when the message is not clearly an OTP. */
    static String find(String text) {
        if (text == null || text.isEmpty()) return "";
        for (Pattern pattern : new Pattern[]{AFTER_KEYWORD, BEFORE_KEYWORD}) {
            Matcher m = pattern.matcher(text);
            if (m.find()) return m.group(1);
        }
        return "";
    }
}
