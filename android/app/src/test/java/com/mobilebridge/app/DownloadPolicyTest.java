package com.mobilebridge.app;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;

import org.junit.Test;

import java.net.URI;

public final class DownloadPolicyTest {
    private static final URI PUBLIC = URI.create("https://codex.example.com");
    private static final String SESSION = "123e4567-e89b-42d3-a456-426614174000";
    private static final String APK = "/api/sessions/" + SESSION + "/files/" +
        "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef";

    @Test
    public void acceptsCurrentGatewayApkDownloads() {
        assertTrue(DownloadPolicy.allows(PUBLIC, URI.create("https://codex.example.com" + APK + "?host=local")));
    }

    @Test
    public void resolvesOnlyAllowedRelativeDownloads() {
        assertEquals(URI.create(PUBLIC + APK + "?host=local"),
            DownloadPolicy.resolve(PUBLIC, APK + "?host=local"));
        assertNull(DownloadPolicy.resolve(PUBLIC, "/app.js"));
    }

    @Test
    public void acceptsOtherGatewayArtifacts() {
        assertTrue(DownloadPolicy.allows(PUBLIC, URI.create(
            "https://codex.example.com/api/sessions/" + SESSION + "/workspace/download?host=local")));
        assertTrue(DownloadPolicy.allows(PUBLIC, URI.create(
            "https://codex.example.com/api/desktop-sessions/claude/workspace/download")));
        assertTrue(DownloadPolicy.allows(PUBLIC, URI.create(
            "https://codex.example.com/api/sessions/" + SESSION + "/uploads/" + SESSION + "/preview?download=1&host=local")));
        assertTrue(DownloadPolicy.allows(PUBLIC, URI.create(
            "https://codex.example.com/api/sessions/diagnostics?thread=" + SESSION)));
    }

    @Test
    public void rejectsUnrelatedOrUnsafeDownloads() {
        assertFalse(DownloadPolicy.allows(PUBLIC, URI.create("https://other.example.com" + APK)));
        assertFalse(DownloadPolicy.allows(PUBLIC, URI.create("http://codex.example.com" + APK)));
        assertFalse(DownloadPolicy.allows(PUBLIC, URI.create("https://codex.example.com/app.js")));
        assertFalse(DownloadPolicy.allows(PUBLIC, URI.create("https://codex.example.com" + APK + "#fragment")));
        assertFalse(DownloadPolicy.allows(PUBLIC, URI.create(
            "https://codex.example.com/api/sessions/" + SESSION + "/uploads/" + SESSION + "/preview?download=0")));
        assertFalse(DownloadPolicy.allows(PUBLIC, URI.create(
            "https://codex.example.com/api/sessions/" + SESSION + "/uploads/" + SESSION + "/preview?download=1&download=1")));
    }
}
