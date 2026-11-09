package com.mobilebridge.app;

import org.json.JSONObject;

import java.net.URI;

/** A saved gateway endpoint; credentials remain in the gateway's WebView cookie jar. */
final class GatewayProfile {
    final String id;
    String name;
    String origin;

    GatewayProfile(String id, String name, String origin) {
        this.id = id;
        this.name = name;
        this.origin = origin;
    }

    JSONObject toJson() {
        try {
            return new JSONObject()
                .put("id", id)
                .put("name", name)
                .put("origin", origin);
        } catch (Exception impossible) {
            throw new IllegalStateException("Unable to encode gateway profile", impossible);
        }
    }

    static GatewayProfile fromJson(JSONObject value) {
        if (value == null) return null;
        String id = value.optString("id", "").trim();
        if (id.isEmpty()) return null;
        String origin = value.optString("origin", "").trim();
        String name = value.optString("name", "").trim();
        if (name.isEmpty()) name = originHost(origin);
        if (name.isEmpty()) name = "新网关";
        return new GatewayProfile(id, name, origin);
    }

    String displayName() {
        String label = name == null || name.trim().isEmpty() ? "新网关" : name.trim();
        String host = originHost(origin);
        return host.isEmpty() ? label : label + " · " + host;
    }

    private static String originHost(String value) {
        if (value == null || value.trim().isEmpty()) return "";
        try {
            URI uri = URI.create(value.trim());
            return uri.getHost() == null ? "" : uri.getHost();
        } catch (Exception ignored) {
            return "";
        }
    }
}
