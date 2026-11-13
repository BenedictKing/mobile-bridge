package com.mobilebridge.app;

import android.content.Context;
import android.os.Handler;
import android.os.Looper;

import java.net.URI;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.Map;

final class LocalRecoveryCoordinator {
    interface Listener {
        void onRecovered(DirectTransport.VerifiedRoute route);
        void onTimeout();
    }

    private final Handler main = new Handler(Looper.getMainLooper());
    private final GatewayProfile profile;
    private final String rootCookie;
    private final Listener listener;
    private final DirectTransport transport;
    private final BonjourDiscovery discovery;
    private final Map<String, Candidate> candidates = new LinkedHashMap<>();
    private final Runnable timeout = this::timeout;
    private volatile boolean started;
    private volatile boolean stopped;

    LocalRecoveryCoordinator(Context context, GatewayProfile profile, URI publicOrigin,
                             String rootCookie, Listener listener) {
        this.profile = profile;
        this.rootCookie = rootCookie;
        this.listener = listener;
        transport = new DirectTransport(publicOrigin, new DirectTransport.Listener() {
            @Override public void onRoute(String mode, String host) {}
            @Override public void onVerified(DirectTransport.VerifiedRoute route) {
                main.post(() -> {
                    if (started && !stopped) {
                        stopped = true;
                        discovery.stop();
                        listener.onRecovered(route);
                    }
                });
            }
        });
        discovery = new BonjourDiscovery(context, profile.deviceId, this::merge);
    }

    void start() {
        if (started || stopped || !profile.deviceId.matches("^[a-f0-9]{32}$")
            || RouteCredentials.root(rootCookie) == null) return;
        started = true;
        merge(profile.recoveryCandidates(0));
        discovery.start();
        main.postDelayed(timeout, 6000);
    }

    private synchronized void merge(java.util.List<Candidate> found) {
        if (!started || stopped) return;
        boolean changed = false;
        for (Candidate candidate : found) {
            if (!profile.deviceId.equals(candidate.deviceId)) continue;
            String key = candidate.origin.toString();
            if (!candidates.containsKey(key)) changed = true;
            candidates.put(key, candidate);
        }
        if (changed) refresh();
    }

    private void refresh() {
        if (!started || stopped) return;
        transport.refresh(new ArrayList<>(candidates.values()), rootCookie, profile.deviceId, "");
    }

    private void timeout() {
        if (!started || stopped) return;
        stopped = true;
        discovery.stop();
        transport.close();
        listener.onTimeout();
    }

    void close() {
        stopped = true;
        main.removeCallbacks(timeout);
        discovery.stop();
        transport.close();
    }
}
