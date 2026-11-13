package com.mobilebridge.app;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertTrue;

import org.json.JSONObject;
import org.junit.Test;

import java.io.BufferedReader;
import java.io.Closeable;
import java.io.InputStreamReader;
import java.net.ServerSocket;
import java.net.Socket;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicReference;

public final class DirectTransportTest {
    @Test
    public void mdnsUsesProofThenRootBootstrapThenRouteCookie() throws Exception {
        String device = "0123456789abcdef0123456789abcdef";
        AtomicReference<String> proofCookie = new AtomicReference<>("missing");
        AtomicReference<String> authCookie = new AtomicReference<>();
        AtomicReference<String> businessCookie = new AtomicReference<>();
        TestServer server = new TestServer();
        server.handler = (path, query, cookie) -> {
            if ("/api/connection/prove".equals(path)) {
                proofCookie.set(cookie);
                // Independently implement the Go wire contract, not the client helper.
                String key = sha256("root-secret");
                if (!sha256(key).equals(query(query, "id"))) throw new IllegalArgumentException("wrong identity");
                javax.crypto.Mac mac = javax.crypto.Mac.getInstance("HmacSHA256");
                mac.init(new javax.crypto.spec.SecretKeySpec(key.getBytes(StandardCharsets.UTF_8), "HmacSHA256"));
                return new JSONObject().put("deviceId", device)
                    .put("proof", hex(mac.doFinal((device + "\n" + server.origin() + "\n"
                        + query(query, "challenge")).getBytes(StandardCharsets.UTF_8)))).toString();
            }
            if ("/api/auth".equals(path)) {
                authCookie.set(cookie);
                return new JSONObject().put("authenticated", true).put("instanceId", "new-instance")
                    .put("mobileRoutes", new JSONObject().put("deviceId", device)
                        .put("endpoints", new org.json.JSONArray().put(server.origin()))).toString();
            }
            businessCookie.set(cookie);
            return "{}";
        };
        CountDownLatch verified = new CountDownLatch(1);
        DirectTransport transport = new DirectTransport(URI.create("https://public.example"), new DirectTransport.Listener() {
            @Override public void onRoute(String mode, String host) {}
            @Override public void onVerified(DirectTransport.VerifiedRoute route) { verified.countDown(); }
        });
        try {
            transport.refresh(java.util.Collections.singletonList(
                new Candidate(URI.create(server.origin()), device, Candidate.MDNS, 1)),
                "codex_mobile_session=root-secret", device, "");
            assertTrue("route was not verified", verified.await(5, TimeUnit.SECONDS));
            assertNull("proof must be anonymous", proofCookie.get());
            assertEquals("codex_mobile_session=root-secret", authCookie.get());
            NativeHttp.Response response = transport.open(URI.create("https://public.example/api/chats"), "GET");
            assertEquals(200, response.status);
            response.close();
            assertEquals("codex_mobile_session=" + RouteCredentials.routeToken("root-secret", server.origin()), businessCookie.get());
        } finally {
            transport.close();
            server.close();
        }
    }

    private static String query(String query, String name) {
        for (String part : query.split("&")) {
            String[] pair = part.split("=", 2);
            if (pair.length == 2 && pair[0].equals(name)) return pair[1];
        }
        return "";
    }

    private static String sha256(String value) throws Exception {
        return hex(java.security.MessageDigest.getInstance("SHA-256").digest(value.getBytes(StandardCharsets.UTF_8)));
    }

    private static String hex(byte[] value) {
        StringBuilder result = new StringBuilder();
        for (byte b : value) result.append(String.format("%02x", b & 255));
        return result.toString();
    }

    private static final class TestServer implements Closeable {
        interface Handler { String respond(String path, String query, String cookie) throws Exception; }
        volatile Handler handler;
        private final ServerSocket socket;
        private final ExecutorService executor = Executors.newCachedThreadPool();

        TestServer() throws Exception {
            socket = new ServerSocket(0, 16, java.net.InetAddress.getByName("127.0.0.1"));
            executor.execute(this::accept);
        }

        String origin() { return "http://127.0.0.1:" + socket.getLocalPort(); }

        private void accept() {
            try {
                while (!socket.isClosed()) {
                    Socket client = socket.accept();
                    executor.execute(() -> serve(client));
                }
            } catch (Exception ignored) {}
        }

        private void serve(Socket client) {
            try (Socket connection = client) {
                BufferedReader reader = new BufferedReader(new InputStreamReader(connection.getInputStream(), StandardCharsets.US_ASCII));
                String request = reader.readLine();
                if (request == null) return;
                String target = request.split(" ")[1];
                String cookie = null;
                for (String line = reader.readLine(); line != null && !line.isEmpty(); line = reader.readLine()) {
                    if (line.regionMatches(true, 0, "Cookie:", 0, 7)) cookie = line.substring(7).trim();
                }
                URI uri = URI.create(target);
                byte[] body = handler.respond(uri.getPath(), uri.getRawQuery(), cookie).getBytes(StandardCharsets.UTF_8);
                String headers = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: "
                    + body.length + "\r\nConnection: close\r\n\r\n";
                connection.getOutputStream().write(headers.getBytes(StandardCharsets.US_ASCII));
                connection.getOutputStream().write(body);
                connection.getOutputStream().flush();
            } catch (Exception ignored) {}
        }

        @Override public void close() {
            try { socket.close(); } catch (Exception ignored) {}
            executor.shutdownNow();
        }
    }
}
