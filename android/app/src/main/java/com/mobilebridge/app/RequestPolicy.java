package com.mobilebridge.app;

import java.net.URI;

final class RequestPolicy {
    private RequestPolicy() {}

    static boolean shouldProxy(URI publicOrigin, URI request, String method) {
        if (!("GET".equalsIgnoreCase(method) || "HEAD".equalsIgnoreCase(method))) return false;
        if (!publicOrigin.getScheme().equalsIgnoreCase(request.getScheme())) return false;
        if (publicOrigin.getPort() != request.getPort()) return false;
        if (!publicOrigin.getHost().equalsIgnoreCase(request.getHost())) return false;
        String path = request.getPath();
        return path != null && path.startsWith("/api/");
    }
}
