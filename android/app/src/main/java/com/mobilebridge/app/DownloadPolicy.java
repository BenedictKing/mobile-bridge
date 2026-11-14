package com.mobilebridge.app;

import java.net.URI;
import java.util.regex.Pattern;

final class DownloadPolicy {
    private static final String SESSION = "(?:session_)?[0-9a-f-]{36}";
    private static final Pattern ARTIFACT = Pattern.compile(
        "^/api/sessions/" + SESSION + "/(?:files/[a-f0-9]{64}|workspace/download)$");
    private static final Pattern DESKTOP_ARTIFACT = Pattern.compile(
        "^/api/desktop-sessions/(?:claude|deepseek|zcode)/workspace/download$");
    private static final Pattern IMAGE = Pattern.compile(
        "^/api/sessions/" + SESSION + "/(?:uploads/[0-9a-f-]{36}/preview|desktop-images/[a-f0-9]{64})$" +
        "|^/api/desktop-sessions/(?:claude|deepseek|zcode)/(?:uploads/[0-9a-f-]{36}/preview|images/[a-f0-9]{64})$");
    private static final Pattern DIAGNOSTIC_QUERY = Pattern.compile(
        "^thread=(?:session_)?[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$");

    private DownloadPolicy() {}

    static URI resolve(URI publicOrigin, String value) {
        URI target = publicOrigin.resolve(value);
        return allows(publicOrigin, target) ? target : null;
    }

    static boolean allows(URI publicOrigin, URI target) {
        if (!sameOrigin(publicOrigin, target) || target.getRawFragment() != null) return false;
        String path = target.getRawPath();
        if (path == null) return false;
        if (ARTIFACT.matcher(path).matches() || DESKTOP_ARTIFACT.matcher(path).matches()) return true;
        if (IMAGE.matcher(path).matches()) return hasSingleDownloadFlag(target.getRawQuery());
        return "/api/sessions/diagnostics".equals(path) &&
            (target.getRawQuery() == null || DIAGNOSTIC_QUERY.matcher(target.getRawQuery()).matches());
    }

    private static boolean sameOrigin(URI publicOrigin, URI target) {
        return publicOrigin.getScheme() != null && publicOrigin.getHost() != null &&
            target.getUserInfo() == null &&
            publicOrigin.getScheme().equalsIgnoreCase(target.getScheme()) &&
            publicOrigin.getHost().equalsIgnoreCase(target.getHost()) &&
            publicOrigin.getPort() == target.getPort();
    }

    private static boolean hasSingleDownloadFlag(String query) {
        if (query == null) return false;
        int flags = 0;
        for (String part : query.split("&")) {
            if (part.startsWith("download=")) {
                if (!"download=1".equals(part)) return false;
                flags++;
            }
        }
        return flags == 1;
    }
}
