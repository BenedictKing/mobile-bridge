package com.mobilebridge.app;

import java.util.List;

final class NotificationGatewayTarget {
    private NotificationGatewayTarget() {}

    static GatewayProfile resolve(List<GatewayProfile> gateways, String gatewayId) {
        if (gatewayId == null || gatewayId.isEmpty()) return null;
        for (GatewayProfile gateway : gateways) {
            if (gatewayId.equals(gateway.id)) return gateway;
        }
        return null;
    }
}
