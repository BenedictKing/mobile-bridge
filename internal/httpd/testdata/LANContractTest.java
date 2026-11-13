package com.mobilebridge.app;

import java.net.URI;
import java.net.HttpURLConnection;
import java.nio.charset.StandardCharsets;

public final class LANContractTest {
    public static void main(String[] args) throws Exception {
        String target = args[0], device = args[1], root = args[2];
        String challenge = "ab".repeat(32);
        URI url = URI.create(target + "/api/connection/prove?device=" + device
            + "&id=" + RouteCredentials.identifier(root) + "&challenge=" + challenge);
        HttpURLConnection connection = (HttpURLConnection) url.toURL().openConnection();
        connection.setConnectTimeout(3000);
        connection.setReadTimeout(3000);
        try {
            if (connection.getResponseCode() != 200) throw new AssertionError("Go rejected Java proof identity");
            String body = new String(connection.getInputStream().readAllBytes(), StandardCharsets.UTF_8);
            if (!body.contains(RouteCredentials.proof(root, device, target, challenge))) {
                throw new AssertionError("Go proof does not match Java verifier");
            }
        } finally { connection.disconnect(); }
        System.out.println("Java: anonymous proof against Go passed");
    }
}
