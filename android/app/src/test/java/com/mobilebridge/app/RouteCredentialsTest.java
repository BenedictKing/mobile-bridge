package com.mobilebridge.app;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNull;

import org.junit.Test;

public final class RouteCredentialsTest {
    @Test
    public void derivesTheServerRouteAndProofVectors() {
        String target = "http://192.168.1.2:8787";
        assertEquals("2bb80d537b1da3e38bd30361aa855686bde0eacd7162fef6a25fe97bf527a25b",
            RouteCredentials.identifier("secret"));
        assertEquals("cmbroute1_9f2e11e3449b0960d5883226dfb9044c64da2339dabe4d831b7c7bbf1d08c1be",
            RouteCredentials.routeToken("secret", target));
        assertEquals("37726173c3f686d15ef271ebbe28e74f4b63b593667921aa47ac4987b2a5643e",
            RouteCredentials.proof("secret", "0123456789abcdef0123456789abcdef", target, "ab".repeat(32)));
    }

    @Test
    public void extractsOnlyARootSessionCookie() {
        RouteCredentials.Root root = RouteCredentials.root(
            "theme=dark; codex_mobile_session_0123456789abcdef01234567=secret; csrf=other");
        assertEquals("codex_mobile_session_0123456789abcdef01234567", root.name);
        assertEquals("secret", root.token);
        assertNull(RouteCredentials.root("codex_mobile_session=cmbroute1_abc"));
    }
}
