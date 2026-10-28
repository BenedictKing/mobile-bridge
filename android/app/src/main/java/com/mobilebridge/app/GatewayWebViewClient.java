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
    }

    private final URI publicOrigin;
    private final DirectTransport direct;
    private final PageListener pages;
    private boolean failed;

    GatewayWebViewClient(URI publicOrigin, DirectTransport direct, PageListener pages) {
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
        NativeHttp.Response local = direct.open(uri, request.getMethod(), cookie);
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
                direct.clear();
                return;
            }
            List<URI> candidates = new ArrayList<>();
            if (auth.has("nativeDirect")) {
                for (int i = 0; i < auth.getJSONArray("nativeDirect").length(); i++) {
                    try {
                        candidates.add(URI.create(auth.getJSONArray("nativeDirect").getString(i)));
                    } catch (Exception ignored) {
                        // Ignore malformed candidates; never scan or infer other addresses.
                    }
                }
            }
            direct.refresh(candidates, cookie, auth.optString("instanceId"));
        } catch (Exception ignored) {
            direct.clear();
        }
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
