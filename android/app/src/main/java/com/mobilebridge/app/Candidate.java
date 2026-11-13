package com.mobilebridge.app;

import java.net.URI;
import java.util.Objects;

final class Candidate {
    static final String PUBLIC_MANIFEST = "publicManifest";
    static final String NATIVE_DIRECT = "nativeDirect";
    static final String MDNS = "mdns";
    static final String CACHE = "cache";

    final URI origin;
    final String deviceId;
    final String source;
    final long generation;

    Candidate(URI origin, String deviceId, String source, long generation) {
        this.origin = origin;
        this.deviceId = deviceId == null ? "" : deviceId;
        this.source = source;
        this.generation = generation;
    }

    boolean requiresProof() {
        return MDNS.equals(source) || CACHE.equals(source);
    }

    @Override
    public boolean equals(Object other) {
        if (!(other instanceof Candidate)) return false;
        Candidate candidate = (Candidate) other;
        return origin.equals(candidate.origin) && deviceId.equals(candidate.deviceId);
    }

    @Override
    public int hashCode() {
        return Objects.hash(origin, deviceId);
    }
}
