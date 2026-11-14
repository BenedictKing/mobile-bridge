package com.mobilebridge.app;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNull;

import org.junit.Test;

public final class RouteCredentialsTest {
    @Test
    public void derivesTheServerRouteAndProofVectors() {
        String target = "http://192.168.1.2:8787";
        assertEquals("3d91b58504a6cc3a159005ee7b16c7ae503ca6ac2a6a3c893837083c236b864a",
            RouteCredentials.identifier("secret"));
        assertEquals("cmbroute1_9f2e11e3449b0960d5883226dfb9044c64da2339dabe4d831b7c7bbf1d08c1be",
            RouteCredentials.routeToken("secret", target));
        assertEquals("a51e16f93c369cebdb836736bbcd29818e7572adb29593b937073d139989e0ad",
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

    @Test public void selectsTheGatewayNamespaceRegardlessOfCookieOrder() {
        String name = "codex_mobile_session_0123456789abcdef01234567";
        String stale = "codex_mobile_session=old; codex_mobile_session_aaaaaaaaaaaaaaaaaaaaaaaa=other";
        for (String header : new String[]{stale + "; " + name + "=current", name + "=current; " + stale}) {
            assertNull(RouteCredentials.root(header));
            assertEquals("current", RouteCredentials.root(header, name).token);
            assertEquals(name, RouteCredentials.root(header, name).name);
        }
    }

    @Test public void migratesLegacyCookieButDoesNotReplaceAnInvalidNamespacedCookie() {
        String name = "codex_mobile_session_0123456789abcdef01234567";
        assertEquals(name + "=old", RouteCredentials.root("codex_mobile_session=old", name).cookie());
        assertNull(RouteCredentials.root(name + "=cmbroute1_invalid; codex_mobile_session=old", name));
        assertNull(RouteCredentials.root("codex_mobile_session_aaaaaaaaaaaaaaaaaaaaaaaa=other", name));
        assertNull(RouteCredentials.root("codex_mobile_session=old", "unrelated-cookie"));
    }
}
