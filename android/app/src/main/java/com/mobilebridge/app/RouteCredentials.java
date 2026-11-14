package com.mobilebridge.app;

import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.SecureRandom;

import javax.crypto.Mac;
import javax.crypto.spec.SecretKeySpec;

final class RouteCredentials {
    static final class Root {
        final String name;
        final String token;

        Root(String name, String token) {
            this.name = name;
            this.token = token;
        }

        String cookie() {
            return name + "=" + token;
        }
    }

    private RouteCredentials() {}

    static Root root(String cookieHeader) {
        if (cookieHeader == null) return null;
        Root selected = null;
        for (String part : cookieHeader.split(";")) {
            Root candidate = parseRoot(part);
            if (candidate == null) continue;
            // 无网关命名空间时，多枚登录 Cookie 的顺序不代表身份。
            if (selected != null) return null;
            selected = candidate;
        }
        return selected;
    }

    static Root root(String cookieHeader, String cookieName) {
        if (cookieName == null || cookieName.isEmpty()) return root(cookieHeader);
        if (cookieHeader == null || !sessionName(cookieName)) return null;
        Root legacy = null;
        for (String part : cookieHeader.split(";")) {
            int equals = part.indexOf('=');
            if (equals <= 0) continue;
            String name = part.substring(0, equals).trim();
            if (cookieName.equals(name)) return parseRoot(part);
            if ("codex_mobile_session".equals(name)) legacy = parseRoot(part);
        }
        // 与服务端的旧登录兼容一致，但后续派生凭据必须使用当前命名空间。
        return legacy == null ? null : new Root(cookieName, legacy.token);
    }

    private static boolean sessionName(String name) {
        return "codex_mobile_session".equals(name) || name.matches("^codex_mobile_session_[a-f0-9]{24}$");
    }

    private static Root parseRoot(String part) {
        int equals = part.indexOf('=');
        if (equals <= 0) return null;
        String name = part.substring(0, equals).trim();
        String token = part.substring(equals + 1).trim();
        return sessionName(name) && !token.isEmpty() && !token.startsWith("cmbroute1_")
            ? new Root(name, token) : null;
    }

    static String routeToken(String rootToken, String target) {
        return "cmbroute1_" + hmac(rootToken, "gateway-route-v1\n" + target);
    }

    static String identifier(String rootToken) {
        return sha256(sha256(rootToken));
    }

    static String proof(String rootToken, String deviceId, String target, String challenge) {
        return hmac(sha256(rootToken), deviceId + "\n" + target + "\n" + challenge);
    }

    static String challenge() {
        byte[] bytes = new byte[32];
        new SecureRandom().nextBytes(bytes);
        return hex(bytes);
    }

    static String sha256(String value) {
        try {
            return hex(MessageDigest.getInstance("SHA-256").digest(value.getBytes(StandardCharsets.UTF_8)));
        } catch (Exception impossible) {
            throw new IllegalStateException(impossible);
        }
    }

    static String hmac(String key, String value) {
        try {
            Mac mac = Mac.getInstance("HmacSHA256");
            mac.init(new SecretKeySpec(key.getBytes(StandardCharsets.UTF_8), "HmacSHA256"));
            return hex(mac.doFinal(value.getBytes(StandardCharsets.UTF_8)));
        } catch (Exception impossible) {
            throw new IllegalStateException(impossible);
        }
    }

    static String hex(byte[] bytes) {
        StringBuilder result = new StringBuilder(bytes.length * 2);
        for (byte value : bytes) result.append(String.format("%02x", value & 0xff));
        return result.toString();
    }
}
