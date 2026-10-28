package com.mobilebridge.app;

import java.net.URI;
import java.util.List;
import java.util.concurrent.CompletionService;
import java.util.concurrent.ExecutionException;
import java.util.concurrent.ExecutorCompletionService;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;

final class EndpointSelector {
    interface Probe {
        boolean test(URI candidate) throws Exception;
    }

    URI select(List<URI> candidates, Probe probe) {
        if (candidates.isEmpty()) return null;
        ExecutorService executor = Executors.newFixedThreadPool(Math.min(4, candidates.size()));
        CompletionService<URI> completion = new ExecutorCompletionService<>(executor);
        try {
            for (URI candidate : candidates) {
                completion.submit(() -> probe.test(candidate) ? candidate : null);
            }
            long deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(3);
            for (int i = 0; i < candidates.size(); i++) {
                long remaining = deadline - System.nanoTime();
                if (remaining <= 0) break;
                Future<URI> future = completion.poll(remaining, TimeUnit.NANOSECONDS);
                if (future == null) break;
                URI selected = future.get();
                if (selected != null) return selected;
            }
            return null;
        } catch (InterruptedException interrupted) {
            Thread.currentThread().interrupt();
            return null;
        } catch (ExecutionException ignored) {
            return null;
        } finally {
            executor.shutdownNow();
        }
    }
}
