package com.mobilebridge.app;

import static org.junit.Assert.assertEquals;
import static org.junit.Assert.assertFalse;
import static org.junit.Assert.assertTrue;

import org.junit.Test;

import java.nio.charset.StandardCharsets;
import java.util.Arrays;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

public final class BonjourDiscoveryTest {
    private static final String DEVICE = "0123456789abcdef0123456789abcdef";

    @Test
    public void acceptsMatchingServicesAndPrivateAddresses() {
        Map<String, byte[]> txt = new HashMap<>();
        txt.put("v", "1".getBytes(StandardCharsets.UTF_8));
        txt.put("id", DEVICE.getBytes(StandardCharsets.UTF_8));
        List<Candidate> result = BonjourDiscovery.candidates(DEVICE, 8787,
            Arrays.asList("192.168.43.2", "8.8.8.8", "fe80::1", "fd12::2"), txt, 7);
        assertEquals(2, result.size());
        assertEquals("http://192.168.43.2:8787", result.get(0).origin.toString());
        assertEquals(Candidate.MDNS, result.get(0).source);
        assertEquals(7, result.get(0).generation);
    }

    @Test
    public void rejectsUnknownVersionsWrongDevicesAndBadPorts() {
        Map<String, byte[]> txt = new HashMap<>();
        txt.put("v", "2".getBytes(StandardCharsets.UTF_8));
        txt.put("id", DEVICE.getBytes(StandardCharsets.UTF_8));
        assertTrue(BonjourDiscovery.candidates(DEVICE, 8787, Arrays.asList("10.0.0.2"), txt, 0).isEmpty());
        txt.put("v", "1".getBytes(StandardCharsets.UTF_8));
        txt.put("id", "ffffffffffffffffffffffffffffffff".getBytes(StandardCharsets.UTF_8));
        assertTrue(BonjourDiscovery.candidates(DEVICE, 8787, Arrays.asList("10.0.0.2"), txt, 0).isEmpty());
        assertTrue(BonjourDiscovery.candidates(DEVICE, 0, Arrays.asList("10.0.0.2"), txt, 0).isEmpty());
    }

    @Test
    public void localAddressRejectsPublicLoopbackAndLinkLocal() {
        assertTrue(BonjourDiscovery.localAddress("172.16.3.4"));
        assertFalse(BonjourDiscovery.localAddress("172.32.3.4"));
        assertFalse(BonjourDiscovery.localAddress("127.0.0.1"));
        assertFalse(BonjourDiscovery.localAddress("169.254.1.2"));
        assertFalse(BonjourDiscovery.localAddress("gateway.example.com"));
        assertFalse(BonjourDiscovery.localAddress("fc.example.com"));
        assertFalse(BonjourDiscovery.localAddress("192.168.999.1"));
    }
}
