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
        for (String part : cookieHeader.split(";")) {
            int equals = part.indexOf('=');
            if (equals <= 0) continue;
            String name = part.substring(0, equals).trim();
            String token = part.substring(equals + 1).trim();
            if (("codex_mobile_session".equals(name)
                || name.matches("^codex_mobile_session_[a-f0-9]{24}$")) && !token.isEmpty()
                && !token.startsWith("cmbroute1_")) {
                return new Root(name, token);
            }
        }
        return null;
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
