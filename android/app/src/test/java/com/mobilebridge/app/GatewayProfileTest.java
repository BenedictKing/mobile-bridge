package com.mobilebridge.app;

import static org.junit.Assert.assertEquals;

import org.junit.Test;

public final class GatewayProfileTest {
    @Test
    public void displaysAProfileWithoutCredentials() {
        GatewayProfile profile = new GatewayProfile("home", "家里 Mac", "https://home.example.com");

        assertEquals("home", profile.id);
        assertEquals("https://home.example.com", profile.origin);
        assertEquals("家里 Mac · home.example.com", profile.displayName());
    }

    @Test
    public void fallsBackToHostForAnUnnamedProfile() {
        GatewayProfile profile = new GatewayProfile("office", null, "https://office.example.com");

        assertEquals("新网关 · office.example.com", profile.displayName());
    }
}
