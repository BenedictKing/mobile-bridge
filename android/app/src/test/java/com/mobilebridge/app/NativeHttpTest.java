package com.mobilebridge.app;

import static org.junit.Assert.assertEquals;

import org.junit.Test;

import java.io.BufferedReader;
import java.io.InputStreamReader;
import java.net.ServerSocket;
import java.net.Socket;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.util.Collections;
import java.util.HashMap;
import java.util.Map;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.atomic.AtomicReference;

public final class NativeHttpTest {
    @Test
    public void identifiesAndroidAndAllowsUserAgentOverride() throws Exception {
        AtomicReference<String> userAgent = new AtomicReference<>();
        ServerSocket socket = new ServerSocket(0, 4, java.net.InetAddress.getByName("127.0.0.1"));
        ExecutorService executor = Executors.newSingleThreadExecutor();
        executor.execute(() -> {
            for (int i = 0; i < 2; i++) serve(socket, userAgent);
        });
        URI uri = URI.create("http://127.0.0.1:" + socket.getLocalPort() + "/api/auth");
        try {
            NativeHttp.readStringAndClose(NativeHttp.open(uri, "GET", Collections.emptyMap(), 1000, 1000));
            assertEquals("BridgeMobile/0.2.0-Android", userAgent.get());

            Map<String, String> headers = new HashMap<>();
            headers.put("User-Agent", "CustomAgent");
            NativeHttp.readStringAndClose(NativeHttp.open(uri, "GET", headers, 1000, 1000));
            assertEquals("CustomAgent", userAgent.get());
        } finally {
            socket.close();
            executor.shutdownNow();
        }
    }

    private static void serve(ServerSocket server, AtomicReference<String> userAgent) {
        try (Socket socket = server.accept()) {
            BufferedReader reader = new BufferedReader(new InputStreamReader(socket.getInputStream(), StandardCharsets.US_ASCII));
            reader.readLine();
            for (String line = reader.readLine(); line != null && !line.isEmpty(); line = reader.readLine()) {
                if (line.regionMatches(true, 0, "User-Agent:", 0, 11)) userAgent.set(line.substring(11).trim());
            }
            byte[] body = "{}".getBytes(StandardCharsets.UTF_8);
            String headers = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: "
                + body.length + "\r\nConnection: close\r\n\r\n";
            socket.getOutputStream().write(headers.getBytes(StandardCharsets.US_ASCII));
            socket.getOutputStream().write(body);
            socket.getOutputStream().flush();
        } catch (Exception ignored) {}
    }
}
