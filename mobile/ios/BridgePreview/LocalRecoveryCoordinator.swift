import Foundation

@MainActor
final class LocalRecoveryCoordinator {
    private let deviceID: String
    private let onCandidates: ([String]) -> Void
    private var discovery: BonjourDiscovery?
    private let defaults = UserDefaults.standard

    init(deviceID: String, onCandidates: @escaping ([String]) -> Void) {
        self.deviceID = deviceID
        self.onCandidates = onCandidates
    }

    func start() {
        stop()
        let cached = Self.cached(deviceID: deviceID)
        if !cached.isEmpty { onCandidates(cached) }
        let discovery = BonjourDiscovery(deviceID: deviceID) { [weak self] origins in
            guard let self else { return }
            let merged = self.remember(origins)
            if !merged.isEmpty { self.onCandidates(merged) }
        }
        self.discovery = discovery
        discovery.start()
    }

    func stop() {
        discovery?.stop()
        discovery = nil
    }

    private func remember(_ origins: [String]) -> [String] {
        var values = Self.cached(deviceID: deviceID)
        var added: [String] = []
        for origin in origins where !values.contains(origin) {
            values.insert(origin, at: 0)
            added.append(origin)
        }
        if !added.isEmpty {
            defaults.set(Array(values.prefix(8)), forKey: Self.key(deviceID))
        }
        return added
    }

    static func cached(deviceID: String) -> [String] {
        guard deviceID.range(of: "^[a-f0-9]{32}$", options: .regularExpression) != nil else { return [] }
        return (UserDefaults.standard.stringArray(forKey: key(deviceID)) ?? []).filter {
            guard let value = try? GatewayURL.origin($0), value == $0 else { return false }
            return value == GatewayURL.httpOrigin(value) && value.hasPrefix("http://")
        }
    }

    static func clear(deviceID: String) {
        UserDefaults.standard.removeObject(forKey: key(deviceID))
    }

    private static func key(_ deviceID: String) -> String { "mdns:" + deviceID }
}
