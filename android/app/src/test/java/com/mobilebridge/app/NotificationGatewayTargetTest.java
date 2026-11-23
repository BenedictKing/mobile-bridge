package com.mobilebridge.app;

import static org.junit.Assert.assertNull;
import static org.junit.Assert.assertSame;

import java.util.List;
import org.junit.Test;

public final class NotificationGatewayTargetTest {
    @Test
    public void resolvesTheNotificationGatewayInsteadOfTheActiveOne() {
        GatewayProfile home = new GatewayProfile("home", "Home", "https://home.example.com");
        GatewayProfile office = new GatewayProfile("office", "Office", "https://office.example.com");

        assertSame(home, NotificationGatewayTarget.resolve(List.of(home, office), "home"));
    }

    @Test
    public void doesNotFallBackWhenTheNotificationGatewayWasDeleted() {
        GatewayProfile active = new GatewayProfile("office", "Office", "https://office.example.com");

        assertNull(NotificationGatewayTarget.resolve(List.of(active), "deleted-home"));
    }
}
