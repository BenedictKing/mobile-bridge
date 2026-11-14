package com.mobilebridge.app;

import org.json.JSONArray;
import org.json.JSONObject;

import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.atomic.AtomicInteger;

final class DirectTransport {
    interface Listener {
        void onRoute(String mode, String host);
        default void onVerified(VerifiedRoute route) {}
    }

    static final class VerifiedRoute {
        final Candidate candidate;
        final String routeCookie;
        final JSONObject auth;

        VerifiedRoute(Candidate candidate, String routeCookie, JSONObject auth) {
            this.candidate = candidate;
            this.routeCookie = routeCookie;
            this.auth = auth;
        }
    }

    private static final class VerificationContext {
        final String cookie;
        final String deviceId;
        final String instanceId;
        final Map<String, VerifiedRoute> verified = new ConcurrentHashMap<>();

        VerificationContext(String cookie, String deviceId, String instanceId) {
            this.cookie = cookie;
            this.deviceId = deviceId;
            this.instanceId = instanceId;
        }
    }

    private final EndpointSelector selector = new EndpointSelector();
    private final ExecutorService refreshExecutor = Executors.newSingleThreadExecutor();
    private final AtomicInteger generation = new AtomicInteger();
    private final Listener listener;
    private final URI publicOrigin;
    private volatile String instanceId = "";
    private volatile String deviceId = "";
    private volatile VerifiedRoute selectedRoute;
    private volatile List<Candidate> lastCandidates = new ArrayList<>();
    private volatile String lastCookie = "";

    DirectTransport(URI publicOrigin, Listener listener) {
        this.publicOrigin = publicOrigin;
        this.listener = listener;
    }

    URI selected() {
        VerifiedRoute route = selectedRoute;
        return route == null ? null : route.candidate.origin;
    }

    void refresh(List<Candidate> candidates, String cookie, String expectedDeviceId, String gatewayInstanceId) {
        int current = generation.incrementAndGet();
        VerifiedRoute previous = selectedRoute;
        boolean keep = previous != null && cookie.equals(lastCookie)
            && deviceId.equals(expectedDeviceId == null ? "" : expectedDeviceId)
            && instanceId.equals(gatewayInstanceId == null ? "" : gatewayInstanceId)
            && candidates.stream().anyMatch(candidate -> candidate.origin.equals(previous.candidate.origin));
        lastCandidates = new ArrayList<>(candidates);
        lastCookie = cookie;
        deviceId = expectedDeviceId == null ? "" : expectedDeviceId;
        instanceId = gatewayInstanceId == null ? "" : gatewayInstanceId;
        if (!keep) selectedRoute = null;
        if (candidates.isEmpty() || cookie.isEmpty()) {
            listener.onRoute("public", "");
            return;
        }
        VerificationContext context = new VerificationContext(cookie, deviceId, instanceId);
        if (!keep) listener.onRoute("probing", "");
        refreshExecutor.execute(() -> {
            List<URI> origins = new ArrayList<>();
            for (Candidate candidate : candidates) origins.add(candidate.origin);
            URI found = selector.select(origins, origin -> verify(find(candidates, origin, context), context) != null);
            if (current != generation.get()) return;
            if (found == null) {
                selectedRoute = null;
                listener.onRoute("public", "");
                return;
            }
            VerifiedRoute route = context.verified.get(found.toString());
            if (route == null) {
                selectedRoute = null;
                listener.onRoute("public", "");
                return;
            }
            selectedRoute = route;
            listener.onVerified(route);
            listener.onRoute("lan", found.getHost());
        });
    }

    private Candidate find(List<Candidate> candidates, URI origin, VerificationContext context) {
        for (Candidate candidate : candidates) if (candidate.origin.equals(origin)) return candidate;
        return new Candidate(origin, context.deviceId, Candidate.NATIVE_DIRECT, generation.get());
    }

    private VerifiedRoute verify(Candidate candidate, VerificationContext context) {
        try {
            VerifiedRoute route = candidate.requiresProof() ? prove(candidate, context) : authenticateManifest(candidate, context);
            if (route != null) context.verified.put(candidate.origin.toString(), route);
            return route;
        } catch (Exception ignored) {
            return null;
        }
    }

    private VerifiedRoute authenticateManifest(Candidate candidate, VerificationContext context) throws Exception {
        NativeHttp.Response response = NativeHttp.open(candidate.origin.resolve("/api/auth"), "GET",
            headers(context.cookie), 700, 1500);
        if (response.status != 200) {
            response.close();
            return null;
        }
        JSONObject auth = new JSONObject(NativeHttp.readStringAndClose(response));
        if (!validAuth(candidate, auth, false, context)) return null;
        RouteCredentials.Root root = RouteCredentials.root(context.cookie, auth.optString("cookieName"));
        if (root == null) return null;
        String routeCookie = root.name + "=" + RouteCredentials.routeToken(root.token, candidate.origin.toString());
        return verifyRouteCookie(candidate, routeCookie, auth, context);
    }

    private VerifiedRoute prove(Candidate candidate, VerificationContext context) throws Exception {
        RouteCredentials.Root root = RouteCredentials.root(context.cookie);
        if (root == null || context.deviceId.isEmpty() || !context.deviceId.equals(candidate.deviceId)) return null;
        String challenge = RouteCredentials.challenge();
        String target = candidate.origin.toString();
        String query = "id=" + RouteCredentials.identifier(root.token)
            + "&challenge=" + challenge + "&device=" + context.deviceId;
        Map<String, String> anonymous = new HashMap<>();
        anonymous.put("Accept", "application/json");
        anonymous.put("X-Mobile-Bridge-Native", "1");
        NativeHttp.Response proofResponse = NativeHttp.open(candidate.origin.resolve("/api/connection/prove?" + query),
            "GET", anonymous, 700, 1500);
        if (proofResponse.status != 200) {
            proofResponse.close();
            return null;
        }
        JSONObject proof = new JSONObject(NativeHttp.readStringAndClose(proofResponse));
        String expected = RouteCredentials.proof(root.token, context.deviceId, target, challenge);
        if (!context.deviceId.equals(proof.optString("deviceId"))
            || !MessageDigest.isEqual(expected.getBytes(StandardCharsets.UTF_8),
                proof.optString("proof").getBytes(StandardCharsets.UTF_8))) return null;

        NativeHttp.Response authResponse = NativeHttp.open(candidate.origin.resolve("/api/auth"), "GET",
            headers(root.cookie()), 700, 1500);
        if (authResponse.status != 200) {
            authResponse.close();
            return null;
        }
        JSONObject auth = new JSONObject(NativeHttp.readStringAndClose(authResponse));
        if (!validAuth(candidate, auth, true, context)) return null;
        return verifyRouteCookie(candidate,
            root.name + "=" + RouteCredentials.routeToken(root.token, target), auth, context);
    }

    private VerifiedRoute verifyRouteCookie(Candidate candidate, String cookie, JSONObject bootstrap,
                                           VerificationContext context) throws Exception {
        NativeHttp.Response response = NativeHttp.open(candidate.origin.resolve("/api/auth"), "GET",
            headers(cookie), 700, 1500);
        if (response.status != 200) {
            response.close();
            return null;
        }
        JSONObject auth = new JSONObject(NativeHttp.readStringAndClose(response));
        if (!validAuth(candidate, auth, false, context)
            || !bootstrap.optString("instanceId").equals(auth.optString("instanceId"))) return null;
        return new VerifiedRoute(candidate, cookie, auth);
    }

    private boolean validAuth(Candidate candidate, JSONObject auth, boolean requireManifest, VerificationContext context) {
        if (!auth.optBoolean("authenticated") || auth.optString("instanceId").isEmpty()) return false;
        if (!context.instanceId.isEmpty() && !context.instanceId.equals(auth.optString("instanceId"))) return false;
        JSONObject routes = auth.optJSONObject("mobileRoutes");
        if (routes == null) return !requireManifest && context.deviceId.isEmpty();
        if (!context.deviceId.isEmpty() && !context.deviceId.equals(routes.optString("deviceId"))) return false;
        if (!requireManifest) return true;
        JSONArray endpoints = routes.optJSONArray("endpoints");
        if (endpoints == null) return false;
        for (int i = 0; i < endpoints.length(); i++) {
            if (candidate.origin.toString().equals(endpoints.optString(i))) return true;
        }
        return false;
    }

    static final class DownloadTarget {
        final URI uri;
        final String cookie;

        DownloadTarget(URI uri, String cookie) {
            this.uri = uri;
            this.cookie = cookie;
        }
    }

    DownloadTarget downloadTarget(URI publicRequest) {
        VerifiedRoute route = selectedRoute;
        if (route == null || route.routeCookie.isEmpty()) return null;
        URI local = route.candidate.origin;
        String cookie = route.routeCookie;
        String target = publicRequest.getRawPath();
        if (publicRequest.getRawQuery() != null) target += "?" + publicRequest.getRawQuery();
        return new DownloadTarget(local.resolve(target), cookie);
    }

    NativeHttp.Response open(URI publicRequest, String method) {
        DownloadTarget target = downloadTarget(publicRequest);
        if (target == null) return null;
        try {
            NativeHttp.Response response = NativeHttp.open(target.uri, method, headers(target.cookie), 800, 0);
            if (response.status == 401 || response.status == 403) {
                response.close();
                invalidate();
                return null;
            }
            return response;
        } catch (Exception ignored) {
            invalidate();
            return null;
        }
    }

    void networkChanged() {
        selectedRoute = null;
        if (!lastCandidates.isEmpty() && !lastCookie.isEmpty()) {
            refresh(lastCandidates, lastCookie, deviceId, instanceId);
        }
    }

    void clear() {
        generation.incrementAndGet();
        selectedRoute = null;
        lastCandidates = new ArrayList<>();
        lastCookie = "";
        instanceId = "";
        deviceId = "";
        listener.onRoute("public", "");
    }

    private void invalidate() {
        selectedRoute = null;
        listener.onRoute("public", "");
        if (!lastCandidates.isEmpty() && !lastCookie.isEmpty()) refresh(lastCandidates, lastCookie, deviceId, instanceId);
    }

    private Map<String, String> headers(String cookie) {
        Map<String, String> headers = new HashMap<>();
        headers.put("Cookie", cookie);
        headers.put("Origin", publicOrigin.toString());
        headers.put("X-Mobile-Bridge-Native", "1");
        return headers;
    }

    void close() {
        generation.incrementAndGet();
        refreshExecutor.shutdownNow();
    }
}
