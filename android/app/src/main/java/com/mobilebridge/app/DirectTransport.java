package com.mobilebridge.app;

import org.json.JSONObject;

import java.net.URI;
import java.util.ArrayList;
import java.util.HashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.atomic.AtomicInteger;

final class DirectTransport {
    // 线路状态：public 走公网地址，probing 正在验证局域网候选，lan 已切到 host 直连。
    interface Listener {
        void onRoute(String mode, String host);
    }

    private final EndpointSelector selector = new EndpointSelector();
    private final ExecutorService refreshExecutor = Executors.newSingleThreadExecutor();
    private final AtomicInteger generation = new AtomicInteger();
    private final Listener listener;
    private final URI publicOrigin;
    private volatile String instanceId = "";
    private volatile URI selected;
    private volatile List<URI> lastCandidates = new ArrayList<>();
    private volatile String lastCookie = "";

    DirectTransport(URI publicOrigin, Listener listener) {
        this.publicOrigin = publicOrigin;
        this.listener = listener;
    }

    URI selected() {
        return selected;
    }

    void refresh(List<URI> candidates, String cookie, String gatewayInstanceId) {
        int current = generation.incrementAndGet();
        lastCandidates = new ArrayList<>(candidates);
        lastCookie = cookie;
        instanceId = gatewayInstanceId;
        selected = null;
        if (candidates.isEmpty() || cookie.isEmpty()) {
            listener.onRoute("public", "");
            return;
        }
        listener.onRoute("probing", "");
        refreshExecutor.execute(() -> {
            URI found = selector.select(candidates, candidate -> verify(candidate, cookie, gatewayInstanceId));
            if (current != generation.get()) return;
            selected = found;
            if (found == null) listener.onRoute("public", "");
            else listener.onRoute("lan", found.getHost());
        });
    }

    private boolean verify(URI candidate, String cookie, String gatewayInstanceId) {
        try {
            NativeHttp.Response response = NativeHttp.open(candidate.resolve("/api/auth"), "GET",
                headers(cookie), 700, 1500);
            if (response.status != 200) {
                response.close();
                return false;
            }
            JSONObject auth = new JSONObject(NativeHttp.readStringAndClose(response));
            return auth.optBoolean("authenticated") && java.util.Objects.equals(gatewayInstanceId, auth.optString("instanceId"));
        } catch (Exception ignored) {
            return false;
        }
    }

    NativeHttp.Response open(URI publicRequest, String method, String cookie) {
        URI local = selected;
        if (local == null) return null;
        String target = publicRequest.getRawPath();
        if (publicRequest.getRawQuery() != null) target += "?" + publicRequest.getRawQuery();
        try {
            NativeHttp.Response response = NativeHttp.open(local.resolve(target), method,
                headers(cookie), 800, 0);
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
        selected = null;
        if (!lastCandidates.isEmpty() && !lastCookie.isEmpty()) {
            refresh(lastCandidates, lastCookie, instanceId);
        }
    }

    void clear() {
        generation.incrementAndGet();
        selected = null;
        lastCandidates = new ArrayList<>();
        lastCookie = "";
        instanceId = "";
        listener.onRoute("public", "");
    }

    private void invalidate() {
        selected = null;
        listener.onRoute("public", "");
        if (!lastCandidates.isEmpty() && !lastCookie.isEmpty()) refresh(lastCandidates, lastCookie, instanceId);
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
