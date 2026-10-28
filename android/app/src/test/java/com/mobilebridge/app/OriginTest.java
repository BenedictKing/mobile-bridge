package com.mobilebridge.app;

import static org.junit.Assert.assertEquals;

import org.junit.Test;

public final class OriginTest {
    @Test
    public void normalizesPublicHttpsOrigin() throws Exception {
        assertEquals("https://codex.example.com", MainActivity.normalizeOrigin("https://codex.example.com/").toString());
        assertEquals("https://codex.example.com:8443", MainActivity.normalizeOrigin("https://codex.example.com:8443").toString());
    }

    @Test(expected = Exception.class)
    public void rejectsHttp() throws Exception {
        MainActivity.normalizeOrigin("http://codex.example.com");
    }

    @Test(expected = Exception.class)
    public void rejectsPaths() throws Exception {
        MainActivity.normalizeOrigin("https://codex.example.com/chat");
    }
}
