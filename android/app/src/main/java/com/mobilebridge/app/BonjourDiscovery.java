package com.mobilebridge.app;

import android.content.Context;
import android.net.nsd.NsdManager;
import android.net.nsd.NsdServiceInfo;

import java.net.InetAddress;
import java.net.URI;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.atomic.AtomicLong;

final class BonjourDiscovery {
    interface Listener {
        void onCandidates(List<Candidate> candidates);
    }

    static final String SERVICE_TYPE = "_mobile-bridge._tcp";
    private final NsdManager manager;
    private final String deviceId;
    private final Listener listener;
    private final AtomicLong generation = new AtomicLong();
    private NsdManager.DiscoveryListener discovery;
    private final Set<String> resolving = new LinkedHashSet<>();

    BonjourDiscovery(Context context, String deviceId, Listener listener) {
        manager = (NsdManager) context.getSystemService(Context.NSD_SERVICE);
        this.deviceId = deviceId;
        this.listener = listener;
    }

    synchronized void start() {
        stop();
        if (manager == null || !deviceId.matches("^[a-f0-9]{32}$")) return;
        long current = generation.incrementAndGet();
        discovery = new NsdManager.DiscoveryListener() {
            @Override public void onDiscoveryStarted(String serviceType) {}
            @Override public void onStartDiscoveryFailed(String serviceType, int errorCode) { stop(); }
            @Override public void onStopDiscoveryFailed(String serviceType, int errorCode) {}
            @Override public void onDiscoveryStopped(String serviceType) {}
            @Override public void onServiceLost(NsdServiceInfo serviceInfo) {}

            @Override
            public void onServiceFound(NsdServiceInfo serviceInfo) {
                if (current != generation.get() || !serviceInfo.getServiceType().startsWith(SERVICE_TYPE)) return;
                resolve(serviceInfo, current);
            }
        };
        try {
            manager.discoverServices(SERVICE_TYPE, NsdManager.PROTOCOL_DNS_SD, discovery);
        } catch (RuntimeException ignored) {
            discovery = null;
        }
    }

    private void resolve(NsdServiceInfo serviceInfo, long current) {
        String key = serviceInfo.getServiceName() + "|" + serviceInfo.getServiceType();
        synchronized (this) {
            if (resolving.size() >= 16 || !resolving.add(key)) return;
        }
        manager.resolveService(serviceInfo, new NsdManager.ResolveListener() {
            @Override
            public void onResolveFailed(NsdServiceInfo serviceInfo, int errorCode) {
                synchronized (BonjourDiscovery.this) { resolving.remove(key); }
            }

            @Override
            public void onServiceResolved(NsdServiceInfo resolved) {
                synchronized (BonjourDiscovery.this) { resolving.remove(key); }
                if (current != generation.get()) return;
                List<String> hosts = new ArrayList<>();
                InetAddress host = resolved.getHost();
                if (host != null) hosts.add(host.getHostAddress());
                List<Candidate> candidates = candidates(deviceId, resolved.getPort(), hosts,
                    resolved.getAttributes(), current);
                if (!candidates.isEmpty()) listener.onCandidates(candidates);
            }
        });
    }

    synchronized void stop() {
        generation.incrementAndGet();
        resolving.clear();
        NsdManager.DiscoveryListener active = discovery;
        discovery = null;
        if (active != null) {
            try { manager.stopServiceDiscovery(active); } catch (RuntimeException ignored) {}
        }
    }

    static List<Candidate> candidates(String deviceId, int port, List<String> hosts,
                                      Map<String, byte[]> txt, long generation) {
        List<Candidate> result = new ArrayList<>();
        if (deviceId == null || !deviceId.matches("^[a-f0-9]{32}$") || port < 1 || port > 65535) return result;
        if (!"1".equals(text(txt, "v")) || !deviceId.equals(text(txt, "id"))) return result;
        Set<URI> seen = new LinkedHashSet<>();
        for (String host : hosts) {
            if (!localAddress(host)) continue;
            try {
                URI origin = new URI("http", null, host, port, null, null, null);
                if (seen.add(origin)) result.add(new Candidate(origin, deviceId, Candidate.MDNS, generation));
            } catch (Exception ignored) {}
        }
        return result;
    }

    static boolean localAddress(String host) {
        if (host == null || host.isEmpty()) return false;
        String value = host.toLowerCase();
        int zone = value.indexOf('%');
        if (zone >= 0) value = value.substring(0, zone);
        if (value.matches("^[0-9.]+$")) {
            String[] parts = value.split("\\.", -1);
            if (parts.length != 4) return false;
            int[] octets = new int[4];
            try {
                for (int i = 0; i < 4; i++) {
                    octets[i] = Integer.parseInt(parts[i]);
                    if (octets[i] < 0 || octets[i] > 255) return false;
                }
            } catch (Exception ignored) { return false; }
            return octets[0] == 10 || (octets[0] == 172 && octets[1] >= 16 && octets[1] <= 31)
                || (octets[0] == 192 && octets[1] == 168);
        }
        return value.contains(":") && (value.startsWith("fc") || value.startsWith("fd"));
    }

    private static String text(Map<String, byte[]> txt, String key) {
        byte[] value = txt == null ? null : txt.get(key);
        return value == null ? "" : new String(value, StandardCharsets.UTF_8);
    }
}
