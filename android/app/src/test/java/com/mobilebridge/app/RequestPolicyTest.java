package com.mobilebridge.app;

import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import org.junit.Test;

import java.net.URI;

public final class RequestPolicyTest {
    private static final URI PUBLIC = URI.create("https://codex.example.com");

    @Test
    public void proxiesOnlySafeApiReadsOnPublicOrigin() {
        assertTrue(RequestPolicy.shouldProxy(PUBLIC, URI.create("https://codex.example.com/api/sessions"), "GET"));
        assertTrue(RequestPolicy.shouldProxy(PUBLIC, URI.create("https://codex.example.com/api/sessions"), "HEAD"));
        assertFalse(RequestPolicy.shouldProxy(PUBLIC, URI.create("https://codex.example.com/api/sessions"), "POST"));
        assertFalse(RequestPolicy.shouldProxy(PUBLIC, URI.create("https://codex.example.com/app.js"), "GET"));
        assertFalse(RequestPolicy.shouldProxy(PUBLIC, URI.create("https://other.example.com/api/sessions"), "GET"));
        assertFalse(RequestPolicy.shouldProxy(PUBLIC, URI.create("http://codex.example.com/api/sessions"), "GET"));
    }
}
