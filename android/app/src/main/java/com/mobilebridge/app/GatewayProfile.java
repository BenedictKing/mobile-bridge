package com.mobilebridge.app;

import org.json.JSONArray;
import org.json.JSONObject;

import java.net.URI;
import java.util.ArrayList;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Set;

/** A saved gateway endpoint; credentials remain in the gateway's WebView cookie jar. */
final class GatewayProfile {
    final String id;
    String name;
    String origin;
    String deviceId = "";
    String cookieName = "";
    long lastSuccessAt;
    final List<String> localOrigins = new ArrayList<>();

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
                .put("origin", origin)
                .put("deviceId", deviceId)
                .put("cookieName", cookieName)
                .put("lastSuccessAt", lastSuccessAt)
                .put("localOrigins", new JSONArray(localOrigins));
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
        GatewayProfile profile = new GatewayProfile(id, name, origin);
        profile.deviceId = value.optString("deviceId", "");
        profile.cookieName = value.optString("cookieName", "");
        profile.lastSuccessAt = value.optLong("lastSuccessAt");
        JSONArray origins = value.optJSONArray("localOrigins");
        if (origins != null) {
            for (int i = 0; i < origins.length() && profile.localOrigins.size() < 8; i++) {
                String candidate = origins.optString(i, "");
                if (localOrigin(candidate)) profile.localOrigins.add(candidate);
            }
        }
        return profile;
    }

    void recordAuthentication(JSONObject auth, String preferredOrigin) {
        cookieName = auth.optString("cookieName", cookieName);
        JSONObject routes = auth.optJSONObject("mobileRoutes");
        if (routes == null) return;
        String id = routes.optString("deviceId", "");
        if (!id.matches("^[a-f0-9]{32}$")) return;
        deviceId = id;
        Set<String> origins = new LinkedHashSet<>();
        if (localOrigin(preferredOrigin)) origins.add(preferredOrigin);
        origins.addAll(localOrigins);
        JSONArray direct = auth.optJSONArray("nativeDirect");
        if (direct != null) {
            for (int i = 0; i < direct.length(); i++) addLocal(origins, direct.optString(i, ""));
        }
        JSONArray endpoints = routes.optJSONArray("endpoints");
        if (endpoints != null) {
            for (int i = 0; i < endpoints.length(); i++) addLocal(origins, endpoints.optString(i, ""));
        }
        localOrigins.clear();
        for (String candidate : origins) {
            if (localOrigins.size() == 8) break;
            localOrigins.add(candidate);
        }
        lastSuccessAt = System.currentTimeMillis();
    }

    List<Candidate> recoveryCandidates(long generation) {
        List<Candidate> result = new ArrayList<>();
        for (String value : localOrigins) {
            try { result.add(new Candidate(URI.create(value), deviceId, Candidate.CACHE, generation)); }
            catch (Exception ignored) {}
        }
        return result;
    }

    private static void addLocal(Set<String> origins, String value) {
        if (localOrigin(value)) origins.add(value);
    }

    private static boolean localOrigin(String value) {
        if (value == null) return false;
        try {
            URI uri = URI.create(value);
            return "http".equalsIgnoreCase(uri.getScheme()) && uri.getUserInfo() == null
                && uri.getRawQuery() == null && uri.getRawFragment() == null
                && (uri.getRawPath() == null || uri.getRawPath().isEmpty())
                && BonjourDiscovery.localAddress(uri.getHost()) && uri.getPort() > 0;
        } catch (Exception ignored) {
            return false;
        }
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
