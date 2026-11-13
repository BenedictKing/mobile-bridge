import Foundation
import Network

@MainActor
final class BonjourDiscovery {
    static let serviceType = "_mobile-bridge._tcp"
    private let deviceID: String
    private let onCandidates: ([String]) -> Void
    private var browser: NWBrowser?
    private var connections: [String: NWConnection] = [:]
    private var resolved: [String: String] = [:]
    private var generation = 0

    init(deviceID: String, onCandidates: @escaping ([String]) -> Void) {
        self.deviceID = deviceID
        self.onCandidates = onCandidates
    }

    func start() {
        stop()
        guard deviceID.range(of: "^[a-f0-9]{32}$", options: .regularExpression) != nil else { return }
        generation += 1
        let current = generation
        let browser = NWBrowser(for: .bonjourWithTXTRecord(type: Self.serviceType, domain: nil), using: .tcp)
        self.browser = browser
        browser.browseResultsChangedHandler = { [weak self] results, _ in
            Task { @MainActor in
                guard let self, self.generation == current, self.browser === browser else { return }
                self.update(results)
            }
        }
        browser.stateUpdateHandler = { [weak self] state in
            if case .failed = state {
                Task { @MainActor in self?.stop() }
            }
        }
        browser.start(queue: .main)
    }

    private func update(_ results: Set<NWBrowser.Result>) {
        var active = Set<String>()
        for result in results {
            let key = String(describing: result.endpoint)
            active.insert(key)
            guard connections[key] == nil, case .bonjour(let record) = result.metadata,
                  case .string(let version) = record.getEntry(for: "v"), version == "1",
                  case .string(let id) = record.getEntry(for: "id"), id == deviceID else { continue }
            resolve(result.endpoint, key: key)
        }
        for (key, connection) in connections where !active.contains(key) {
            connection.cancel()
            connections.removeValue(forKey: key)
            resolved.removeValue(forKey: key)
        }
    }

    private func resolve(_ endpoint: NWEndpoint, key: String) {
        let current = generation
        let connection = NWConnection(to: endpoint, using: .tcp)
        connections[key] = connection
        connection.stateUpdateHandler = { [weak self] state in
            Task { @MainActor in
                guard let self, self.generation == current, self.connections[key] === connection else { return }
                switch state {
                case .ready:
                    guard let remote = connection.currentPath?.remoteEndpoint,
                          case .hostPort(let host, let port) = remote,
                          let origin = Self.origin(deviceID: self.deviceID, host: host, port: port.rawValue) else {
                        connection.cancel()
                        self.connections.removeValue(forKey: key)
                        return
                    }
                    if self.resolved[key] != origin {
                        self.resolved[key] = origin
                        self.onCandidates(Array(Set(self.resolved.values)).sorted())
                    }
                    connection.cancel()
                    self.connections.removeValue(forKey: key)
                case .failed, .cancelled:
                    self.connections.removeValue(forKey: key)
                default:
                    break
                }
            }
        }
        connection.start(queue: .main)
    }

    func stop() {
        generation += 1
        browser?.cancel()
        browser = nil
        connections.values.forEach { $0.cancel() }
        connections.removeAll()
        resolved.removeAll()
    }

    static func origin(deviceID: String, host: NWEndpoint.Host, port: UInt16) -> String? {
        let value: String
        switch host {
        case .ipv4(let address): value = String(describing: address)
        case .ipv6(let address): value = String(describing: address)
        default: return nil
        }
        return origin(deviceID: deviceID, host: value, port: port)
    }

    static func origin(deviceID: String, host: String, port: UInt16) -> String? {
        let normalized = host.trimmingCharacters(in: CharacterSet(charactersIn: "[]")).split(separator: "%").first?.lowercased() ?? ""
        guard deviceID.range(of: "^[a-f0-9]{32}$", options: .regularExpression) != nil,
              port > 0, GatewayURL.local(host), normalized != "localhost", normalized != "::1",
              !normalized.hasPrefix("127."), !normalized.hasPrefix("fe80:") else { return nil }
        var components = URLComponents()
        components.scheme = "http"
        components.host = host.lowercased()
        components.port = Int(port)
        guard let value = components.string,
              let canonical = try? GatewayURL.origin(value), canonical == value else { return nil }
        return value
    }
}
