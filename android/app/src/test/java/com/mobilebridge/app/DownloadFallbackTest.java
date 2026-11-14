package com.mobilebridge.app;

import org.junit.Test;
import static org.junit.Assert.*;
import java.net.URI;
import org.json.JSONObject;

public final class DownloadFallbackTest {
    @Test public void retriesOnlyAuthenticationAndTransportFailures() {
        for (int reason : new int[]{401, 403, 408, 429, 500, 503, 1002, 1004, 1008}) {
            assertTrue("expected retry for " + reason, DownloadFallback.retryable(reason));
        }
        for (int reason : new int[]{0, 404, 410, 1000, 1001, 1005, 1006, 1007, 1009, 1010}) {
            assertFalse("unexpected retry for " + reason, DownloadFallback.retryable(reason));
        }
    }

    @Test public void persistsSessionIdentityWithoutCredentials() throws Exception {
        JSONObject record = DownloadFallback.record("https://public.example",
            URI.create("https://public.example/api/chats"), "app.apk", null, null,
            "codex_mobile_session=root-secret");
        assertFalse(record.toString().contains("root-secret"));
        assertEquals(DownloadFallback.session("codex_mobile_session=root-secret; other=value"), record.getString("session"));
        assertNotEquals(DownloadFallback.session("codex_mobile_session=other-account"), record.getString("session"));
        assertEquals("", DownloadFallback.session(null));
    }

    @Test public void retryUsesTheOriginalNamespaceWithStaleCookiesPresent() throws Exception {
        String name = "codex_mobile_session_0123456789abcdef01234567";
        JSONObject record = DownloadFallback.record("https://public.example",
            URI.create("https://public.example/api/chats"), "app.apk", null, null, name + "=current");
        String header = "codex_mobile_session=stale; " + name + "=current";
        RouteCredentials.Root root = RouteCredentials.root(header, record.getString("cookieName"));
        assertEquals(record.getString("session"), DownloadFallback.session(root.cookie()));
    }
}
