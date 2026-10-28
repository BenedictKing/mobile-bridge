package com.mobilebridge.app;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertNull;

import org.junit.Test;

import java.net.URI;
import java.util.Arrays;
import java.util.Collections;

public final class EndpointSelectorTest {
    private static final URI SLOW = URI.create("http://192.0.2.10:8787");
    private static final URI FAST = URI.create("http://192.0.2.11:8787");
    private static final URI DOWN = URI.create("http://192.0.2.12:8787");

    @Test
    public void selectsFirstVerifiedEndpointByLatency() {
        EndpointSelector selector = new EndpointSelector();
        URI selected = selector.select(Arrays.asList(SLOW, FAST, DOWN), candidate -> {
            if (candidate == DOWN) return false;
            Thread.sleep(candidate == SLOW ? 500 : 20);
            return true;
        });
        assertEquals(FAST, selected);
    }

    @Test
    public void rejectsWrongOrUnavailableEndpoints() {
        EndpointSelector selector = new EndpointSelector();
        assertNull(selector.select(Arrays.asList(SLOW, DOWN), candidate -> false));
        assertNull(selector.select(Collections.emptyList(), candidate -> true));
    }
}
