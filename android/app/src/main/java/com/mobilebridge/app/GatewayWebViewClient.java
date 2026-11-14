package com.mobilebridge.app;

import android.webkit.CookieManager;
import android.webkit.WebResourceRequest;
import android.webkit.WebResourceResponse;
import android.webkit.WebView;
import android.webkit.WebViewClient;

import org.json.JSONObject;

import java.io.ByteArrayInputStream;
import java.net.URI;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

final class GatewayWebViewClient extends WebViewClient {
    interface PageListener {
        void onLoaded();

        void onFailed(String reason);

        default void onAuthenticated(JSONObject auth) {}
    }

    private final URI publicOrigin;
    private final DirectTransport direct;
    private final PageListener pages;
    private final android.content.Context context;
    private final java.util.Set<Candidate> manifestCandidates = new java.util.LinkedHashSet<>();
    private final java.util.Set<Candidate> mdnsCandidates = new java.util.LinkedHashSet<>();
    private BonjourDiscovery bonjour;
    private String lastCookie = "";
    private String deviceId = "";
    private String instanceId = "";
    private boolean failed;

    GatewayWebViewClient(android.content.Context context, URI publicOrigin,
                         DirectTransport direct, PageListener pages) {
        this.context = context.getApplicationContext();
        this.publicOrigin = publicOrigin;
        this.direct = direct;
        this.pages = pages;
    }

    @Override
    public void onPageStarted(WebView view, String url, android.graphics.Bitmap favicon) {
        failed = false;
    }

    @Override
    public void onPageFinished(WebView view, String url) {
        if (!failed) pages.onLoaded();
    }

    @Override
    public void onReceivedError(WebView view, WebResourceRequest request, android.webkit.WebResourceError error) {
        if (!request.isForMainFrame()) return;
        failed = true;
        pages.onFailed(String.valueOf(error.getDescription()));
    }

    @Override
    public void onReceivedHttpError(WebView view, WebResourceRequest request, WebResourceResponse response) {
        if (!request.isForMainFrame() || response.getStatusCode() < 500) return;
        failed = true;
        pages.onFailed("HTTP " + response.getStatusCode());
    }

    @Override
    public WebResourceResponse shouldInterceptRequest(WebView view, WebResourceRequest request) {
        URI uri = URI.create(request.getUrl().toString());
        if (!RequestPolicy.shouldProxy(publicOrigin, uri, request.getMethod())) return null;
        String cookie = CookieManager.getInstance().getCookie(publicOrigin.toString());
        if (cookie == null) cookie = "";
        if ("/api/auth".equals(uri.getPath())) {
            return proxyAuth(uri, cookie);
        }
        NativeHttp.Response local = direct.open(uri, request.getMethod());
        if (local == null) return null;
        return webResponse(local, false);
    }

    private WebResourceResponse proxyAuth(URI uri, String cookie) {
        try {
            Map<String, String> headers = new HashMap<>();
            headers.put("Cookie", cookie);
            headers.put("X-Mobile-Bridge-Native", "1");
            NativeHttp.Response response = NativeHttp.open(uri, "GET", headers, 10000, 15000);
            byte[] body = NativeHttp.readAllAndClose(response);
            syncPublicCookies(response.headers);
            if (response.status == 200) discover(body, cookie);
            return webResponse(response, new ByteArrayInputStream(body), true);
        } catch (Exception ignored) {
            return null;
        }
    }

    private void discover(byte[] body, String cookie) {
        try {
            JSONObject auth = new JSONObject(new String(body));
            if (!auth.optBoolean("authenticated")) {
                close();
                direct.clear();
                return;
            }
            JSONObject routes = auth.optJSONObject("mobileRoutes");
            synchronized (this) {
                manifestCandidates.clear();
                if (auth.has("nativeDirect")) {
                    for (int i = 0; i < auth.getJSONArray("nativeDirect").length(); i++) {
                        addCandidate(manifestCandidates, auth.getJSONArray("nativeDirect").getString(i),
                            routes, Candidate.NATIVE_DIRECT);
                    }
                }
                if (routes != null && routes.optJSONArray("endpoints") != null) {
                    for (int i = 0; i < routes.getJSONArray("endpoints").length(); i++) {
                        addCandidate(manifestCandidates, routes.getJSONArray("endpoints").getString(i), routes,
                            Candidate.PUBLIC_MANIFEST);
                    }
                }
                RouteCredentials.Root root = RouteCredentials.root(cookie, auth.optString("cookieName"));
                lastCookie = root == null ? "" : root.cookie();
                deviceId = routes == null ? "" : routes.optString("deviceId");
                instanceId = auth.optString("instanceId");
                if (bonjour == null && deviceId.matches("^[a-f0-9]{32}$")) {
                    bonjour = new BonjourDiscovery(context, deviceId, this::onBonjourCandidates);
                }
                if (bonjour != null) bonjour.start();
            }
            refreshCandidates();
            pages.onAuthenticated(auth);
        } catch (Exception ignored) {
            direct.clear();
        }
    }

    private void addCandidate(java.util.Set<Candidate> candidates, String value,
                              JSONObject routes, String source) {
        try {
            URI origin = URI.create(value);
            if (!"http".equalsIgnoreCase(origin.getScheme()) || !BonjourDiscovery.localAddress(origin.getHost())) return;
            String deviceId = routes == null ? "" : routes.optString("deviceId");
            candidates.add(new Candidate(origin, deviceId, source, 0));
        } catch (Exception ignored) {
            // Ignore malformed candidates; never scan or infer other addresses.
        }
    }

    private synchronized void onBonjourCandidates(List<Candidate> candidates) {
        mdnsCandidates.addAll(candidates);
        refreshCandidates();
    }

    private synchronized void refreshCandidates() {
        java.util.Set<Candidate> merged = new java.util.LinkedHashSet<>();
        merged.addAll(manifestCandidates);
        merged.addAll(mdnsCandidates);
        direct.refresh(new ArrayList<>(merged), lastCookie, deviceId, instanceId);
    }

    synchronized void pauseDiscovery() {
        if (bonjour != null) bonjour.stop();
    }

    synchronized void resumeDiscovery() {
        if (bonjour != null) bonjour.start();
    }

    synchronized void close() {
        if (bonjour != null) bonjour.stop();
        bonjour = null;
        manifestCandidates.clear();
        mdnsCandidates.clear();
    }

    private void syncPublicCookies(Map<String, List<String>> headers) {
        CookieManager manager = CookieManager.getInstance();
        for (Map.Entry<String, List<String>> header : headers.entrySet()) {
            if (header.getKey() != null && header.getKey().equalsIgnoreCase("Set-Cookie")) {
                for (String value : header.getValue()) manager.setCookie(publicOrigin.toString(), value);
            }
        }
        manager.flush();
    }

    private WebResourceResponse webResponse(NativeHttp.Response response, boolean publicResponse) {
        return webResponse(response, response.body, publicResponse);
    }

    private WebResourceResponse webResponse(NativeHttp.Response response, java.io.InputStream body,
                                            boolean publicResponse) {
        String mime = "application/octet-stream";
        if (response.contentType != null) mime = response.contentType.split(";", 2)[0];
        Map<String, String> headers = new HashMap<>();
        for (Map.Entry<String, List<String>> header : response.headers.entrySet()) {
            if (header.getKey() == null || header.getValue().isEmpty()) continue;
            if (!publicResponse && header.getKey().equalsIgnoreCase("Set-Cookie")) continue;
            headers.put(header.getKey(), header.getValue().get(0));
        }
        String reason = response.reason.isEmpty() ? "Response" : response.reason;
        return new WebResourceResponse(mime, "UTF-8", response.status, reason, headers, body);
    }
}
