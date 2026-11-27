import CryptoKit
import Foundation

struct OfflineHistory: Decodable, Sendable {
    struct Row: Decodable, Sendable {
        let key: String
        let version: String
        let turnId: String
        let role: String
        let text: String
    }

    static let maxBytes = 8 * 1024 * 1024
    let version: Int
    let deviceId: String
    let host: String
    let agent: String
    let id: UUID
    let title: String
    let capturedAt: TimeInterval
    let throughTurn: String
    let rows: [Row]

    static func decode(_ data: Data, id: UUID, device: String) throws -> Self {
        guard data.count <= maxBytes, let value = try? JSONDecoder().decode(Self.self, from: data),
              value.version == 1, value.id == id, value.deviceId == device, value.host == "local", value.agent == "codex",
              value.capturedAt > 0, value.capturedAt <= 4_102_444_800, value.title.utf8.count <= 1024,
              !value.throughTurn.isEmpty, value.throughTurn.utf8.count <= 128, (1...4000).contains(value.rows.count),
              Set(value.rows.map(\.key)).count == value.rows.count,
              value.rows.allSatisfy({ !$0.key.isEmpty && $0.key.utf8.count <= 64 && $0.version.utf8.count == 24 &&
                  $0.turnId.utf8.count <= 128 && $0.role.utf8.count <= 32 && $0.text.utf8.count <= 512 * 1024 }) else {
            throw OfflineHistoryError.invalid
        }
        return value
    }
}

enum OfflineHistoryError: Error {
    case invalid, authentication, expired, unsupported, missing, changing, tooLarge, full, busy
}

struct OfflineHistoryScope: Hashable, Sendable {
    let device: String
    let key: String

    init(device: String, binding: String) {
        self.device = device
        key = Self.digest(Data(("offline-history-v1\n" + device + "\n" + binding).utf8))
    }

    static func digest(_ data: Data) -> String {
        SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }
}

struct OfflineHistoryTransfer {
    let bytes: Data
    let history: OfflineHistory
    let scope: OfflineHistoryScope

    init(bytes: Data, digest: String, scope: OfflineHistoryScope, id: UUID) throws {
        guard bytes.count <= OfflineHistory.maxBytes, OfflineHistoryScope.digest(bytes) == digest else {
            throw OfflineHistoryError.invalid
        }
        self.bytes = bytes
        self.scope = scope
        history = try OfflineHistory.decode(bytes, id: id, device: scope.device)
    }

    static func fetch(address: String, cookie: String, scope: OfflineHistoryScope, id: UUID) async throws -> Self {
        guard var target = URLComponents(string: address + "/api/mobile/offline-history") else { throw OfflineHistoryError.invalid }
        target.queryItems = [
            .init(name: "id", value: id.uuidString.lowercased()), .init(name: "host", value: "local"), .init(name: "agent", value: "codex")
        ]
        guard let url = target.url else { throw OfflineHistoryError.invalid }
        var request = URLRequest(url: url, cachePolicy: .reloadIgnoringLocalCacheData)
        request.setValue(cookie, forHTTPHeaderField: "Cookie")
        request.setValue(GatewayURL.httpOrigin(address), forHTTPHeaderField: "Origin")
        request.setValue("BridgeMobile/0.1-iOS", forHTTPHeaderField: "User-Agent")
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        request.setValue("identity", forHTTPHeaderField: "Accept-Encoding")
        let config = URLSessionConfiguration.ephemeral
        config.httpShouldSetCookies = false; config.httpCookieStorage = nil; config.urlCredentialStorage = nil
        config.urlCache = nil; config.timeoutIntervalForRequest = 15; config.timeoutIntervalForResource = 60
        let session = URLSession(configuration: config, delegate: OfflineHistoryNoRedirect(), delegateQueue: nil)
        defer { session.invalidateAndCancel() }

        let (stream, response) = try await session.bytes(for: request)
        guard let http = response as? HTTPURLResponse, http.url == url else { throw OfflineHistoryError.invalid }
        switch http.statusCode {
        case 200: break
        case 401: throw OfflineHistoryError.expired
        case 403: throw OfflineHistoryError.authentication
        case 400,405: throw OfflineHistoryError.unsupported
        case 404: throw OfflineHistoryError.missing
        case 409: throw OfflineHistoryError.changing
        case 413: throw OfflineHistoryError.tooLarge
        case 429: throw OfflineHistoryError.busy
        default: throw OfflineHistoryError.invalid
        }
        guard http.mimeType == "application/json", let digest = http.value(forHTTPHeaderField: "X-Bridge-Offline-SHA256"),
              digest.utf8.count == 64, http.expectedContentLength <= OfflineHistory.maxBytes else { throw OfflineHistoryError.invalid }
        var bytes = Data()
        for try await byte in stream {
            guard bytes.count < OfflineHistory.maxBytes else { throw OfflineHistoryError.tooLarge }
            bytes.append(byte)
        }
        try Task.checkCancellation()
        return try .init(bytes: bytes, digest: digest, scope: scope, id: id)
    }
}

private final class OfflineHistoryNoRedirect: NSObject, URLSessionTaskDelegate {
    func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
                    newRequest request: URLRequest, completionHandler: @escaping (URLRequest?) -> Void) {
        completionHandler(nil)
    }
}

struct OfflineHistoryStore: Sendable {
    struct Entry: Sendable {
        let scope: OfflineHistoryScope
        let id: UUID
        let title: String?
        let capturedAt: TimeInterval?
    }

    let root: URL
    var maxBytes = 64 * 1024 * 1024
    var maxEntries = 32

    private func folder(_ scope: OfflineHistoryScope) -> URL { root.appendingPathComponent(scope.key, isDirectory: true) }
    private func file(_ scope: OfflineHistoryScope, _ id: UUID) -> URL {
        folder(scope).appendingPathComponent(id.uuidString.lowercased()).appendingPathExtension("json")
    }

    func load(_ scope: OfflineHistoryScope, _ id: UUID) throws -> OfflineHistory {
        let url = file(scope, id)
        let values = try url.resourceValues(forKeys: [.isRegularFileKey, .fileSizeKey])
        guard values.isRegularFile == true, let size = values.fileSize, size <= OfflineHistory.maxBytes else {
            throw OfflineHistoryError.invalid
        }
        return try OfflineHistory.decode(Data(contentsOf: url), id: id, device: scope.device)
    }

    func list(_ scope: OfflineHistoryScope) -> [Entry] {
        files(in: folder(scope)).compactMap { url in
            guard let id = UUID(uuidString: url.deletingPathExtension().lastPathComponent) else { return nil }
            let history = try? load(scope, id)
            return .init(scope: scope, id: id, title: history?.title, capturedAt: history?.capturedAt)
        }
    }

    func save(_ transfer: OfflineHistoryTransfer, to scope: OfflineHistoryScope) throws {
        guard transfer.scope == scope, transfer.history.deviceId == scope.device, transfer.bytes.count <= OfflineHistory.maxBytes else {
            throw OfflineHistoryError.invalid
        }
        let fm = FileManager.default, destination = file(scope, transfer.history.id)
        let folders = (try? fm.contentsOfDirectory(at: root, includingPropertiesForKeys: [.isDirectoryKey], options: [.skipsHiddenFiles])) ?? []
        let other = folders.filter { (try? $0.resourceValues(forKeys: [.isDirectoryKey]).isDirectory) == true }
            .flatMap { files(in: $0) }.filter { $0.standardizedFileURL != destination.standardizedFileURL }
        let used = other.reduce(0) { sum, url in
            sum + ((try? url.resourceValues(forKeys: [.fileSizeKey]).fileSize) ?? 0)
        }
        guard other.count < maxEntries, transfer.bytes.count <= maxBytes - used else { throw OfflineHistoryError.full }
        try fm.createDirectory(at: folder(scope), withIntermediateDirectories: true)
        #if os(iOS)
        var protected = root, values = URLResourceValues()
        values.isExcludedFromBackup = true
        try protected.setResourceValues(values)
        try transfer.bytes.write(to: destination, options: [.atomic, .completeFileProtection])
        #else
        try transfer.bytes.write(to: destination, options: .atomic)
        #endif
    }

    func remove(_ scope: OfflineHistoryScope, _ id: UUID) throws {
        try FileManager.default.removeItem(at: file(scope, id))
    }

    func revoke(_ scope: OfflineHistoryScope) { try? FileManager.default.removeItem(at: folder(scope)) }

    private func files(in directory: URL) -> [URL] {
        ((try? FileManager.default.contentsOfDirectory(at: directory, includingPropertiesForKeys: [.isRegularFileKey, .fileSizeKey], options: [.skipsHiddenFiles])) ?? [])
            .filter { $0.pathExtension == "json" && (try? $0.resourceValues(forKeys: [.isRegularFileKey]).isRegularFile) == true }
    }
}
