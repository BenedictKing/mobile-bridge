package com.mobilebridge.app;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;

import org.json.JSONArray;
import org.json.JSONObject;
import org.junit.Test;

public final class RecoveryRecordTest {
    @Test
    public void savesIdentityAndBoundedLocalOriginsWithoutCredentials() throws Exception {
        GatewayProfile profile = new GatewayProfile("home", "Home", "https://home.example.com");
        JSONObject routes = new JSONObject()
            .put("deviceId", "0123456789abcdef0123456789abcdef")
            .put("endpoints", new JSONArray()
                .put("http://192.168.1.2:8787")
                .put("https://home.example.com")
                .put("http://8.8.8.8:8787"));
        JSONObject auth = new JSONObject().put("mobileRoutes", routes);
        profile.recordAuthentication(auth, "http://10.0.0.2:8787");

        JSONObject encoded = profile.toJson();
        GatewayProfile decoded = GatewayProfile.fromJson(encoded);
        assertEquals("0123456789abcdef0123456789abcdef", decoded.deviceId);
        assertEquals("http://10.0.0.2:8787", decoded.localOrigins.get(0));
        assertEquals(2, decoded.localOrigins.size());
        assertEquals(2, decoded.recoveryCandidates(4).size());
        assertEquals(Candidate.CACHE, decoded.recoveryCandidates(4).get(0).source);
        assertFalse(encoded.toString().contains("codex_mobile_session"));
        assertFalse(encoded.toString().contains("cmbroute1_"));
    }

    @Test
    public void keepsAtMostEightRecentLocalOrigins() throws Exception {
        JSONArray endpoints = new JSONArray();
        for (int i = 1; i <= 12; i++) endpoints.put("http://10.0.0." + i + ":8787");
        JSONObject auth = new JSONObject().put("mobileRoutes", new JSONObject()
            .put("deviceId", "0123456789abcdef0123456789abcdef").put("endpoints", endpoints));
        GatewayProfile profile = new GatewayProfile("home", "Home", "https://home.example.com");
        profile.recordAuthentication(auth, null);
        assertEquals(8, profile.localOrigins.size());
    }
}
