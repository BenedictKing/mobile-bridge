package com.mobilebridge.app;

import java.io.ByteArrayOutputStream;
import java.io.Closeable;
import java.io.IOException;
import java.io.InputStream;
import java.net.HttpURLConnection;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.util.Collections;
import java.util.List;
import java.util.Map;

final class NativeHttp {
    private NativeHttp() {}

    static final class Response implements Closeable {
        final URI uri;
        final int status;
        final String reason;
        final String contentType;
        final Map<String, List<String>> headers;
        final InputStream body;
        private final HttpURLConnection connection;

        Response(URI uri, HttpURLConnection connection) throws IOException {
            this.uri = uri;
            this.connection = connection;
            status = connection.getResponseCode();
            reason = connection.getResponseMessage() == null ? "" : connection.getResponseMessage();
            contentType = connection.getContentType();
            headers = connection.getHeaderFields() == null
                ? Collections.emptyMap() : connection.getHeaderFields();
            InputStream stream = status >= 400 ? connection.getErrorStream() : connection.getInputStream();
            body = stream == null ? new java.io.ByteArrayInputStream(new byte[0]) : stream;
        }

        @Override
        public void close() throws IOException {
            try {
                body.close();
            } finally {
                connection.disconnect();
            }
        }
    }

    static Response open(URI uri, String method, Map<String, String> headers,
                         int connectTimeoutMs, int readTimeoutMs) throws IOException {
        HttpURLConnection connection = (HttpURLConnection) uri.toURL().openConnection();
        connection.setRequestMethod(method);
        connection.setInstanceFollowRedirects(false);
        connection.setConnectTimeout(connectTimeoutMs);
        connection.setReadTimeout(readTimeoutMs);
        for (Map.Entry<String, String> header : headers.entrySet()) {
            connection.setRequestProperty(header.getKey(), header.getValue());
        }
        try {
            return new Response(uri, connection);
        } catch (IOException | RuntimeException error) {
            connection.disconnect();
            throw error;
        }
    }

    static byte[] readAllAndClose(Response response) throws IOException {
        try (response) {
            ByteArrayOutputStream output = new ByteArrayOutputStream();
            byte[] buffer = new byte[8192];
            int count;
            while ((count = response.body.read(buffer)) != -1) output.write(buffer, 0, count);
            return output.toByteArray();
        }
    }

    static String readStringAndClose(Response response) throws IOException {
        return new String(readAllAndClose(response), StandardCharsets.UTF_8);
    }
}
