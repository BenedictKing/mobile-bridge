import Foundation

@main struct OfflineHistoryTests {
    private static let id = UUID(uuidString: "a98a4718-1358-40c8-98f0-61f2fac41022")!
    private static let device = String(repeating: "e", count: 32)
    private static let scope = OfflineHistoryScope(device: device, binding: "fixture-binding")

    private static func body(title: String, id: UUID = id, device: String = device, rows: Int = 1) throws -> Data {
        try JSONSerialization.data(withJSONObject: [
            "version": 1, "deviceId": device, "host": "local", "agent": "codex", "id": id.uuidString,
            "title": title, "capturedAt": 1_789_876_543, "throughTurn": "turn-one",
            "rows": (0..<rows).map { index in
                ["key": "row-\(index)", "version": String(repeating: "b", count: 24), "turnId": "turn-one",
                 "role": "assistant", "text": "离线🚀 可读原文"]
            }
        ])
    }

    private static func transfer(_ bytes: Data, scope: OfflineHistoryScope = scope, id: UUID = id) throws -> OfflineHistoryTransfer {
        try .init(bytes: bytes, digest: OfflineHistoryScope.digest(bytes), scope: scope, id: id)
    }

    private static func fails(_ operation: () throws -> Void, check: (Error) -> Bool = { _ in true }) {
        do { try operation(); preconditionFailure("Expected a rejected operation") }
        catch { precondition(check(error), "Wrong error: \(error)") }
    }

    static func main() throws {
        let temp = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString, isDirectory: true)
        defer { try? FileManager.default.removeItem(at: temp) }
        let root = temp.appendingPathComponent("Archive", isDirectory: true)
        var store = OfflineHistoryStore(root: root)
        let original = try body(title: "原副本"), replacement = try body(title: "更新后的副本")
        let old = try transfer(original)
        try store.save(old, to: scope)
        // A new store has no process or network state to help it read.
        let reopened = OfflineHistoryStore(root: root)
        let cold = try reopened.load(scope, id)
        precondition(cold.title == "原副本" && cold.rows[0].text == "离线🚀 可读原文")
        precondition(reopened.list(scope).map(\.id) == [id])

        fails { _ = try OfflineHistoryTransfer(bytes: replacement, digest: OfflineHistoryScope.digest(original), scope: scope, id: id) }
        fails { _ = try transfer(body(title: "", id: UUID())) }
        fails { _ = try transfer(body(title: "", device: "other")) }
        fails { _ = try transfer(body(title: "", rows: 0)) }
        fails { _ = try transfer(body(title: "", rows: 4001)) }

        let refreshed = try transfer(replacement)
        store.maxBytes = replacement.count - 1
        fails({ try store.save(refreshed, to: scope) }, check: { $0 as? OfflineHistoryError == .full })
        let retained = try store.load(scope, id)
        precondition(retained.title == "原副本")
        store.maxBytes = replacement.count
        store.maxEntries = 1
        try store.save(refreshed, to: scope)
        let updated = try store.load(scope, id)
        precondition(updated.title == "更新后的副本")

        let rotated = OfflineHistoryScope(device: device, binding: "another-login")
        precondition(store.list(rotated).isEmpty)
        fails { _ = try store.load(rotated, id) }
        fails({ try store.save(refreshed, to: rotated) }, check: { $0 as? OfflineHistoryError == .invalid })
        store.revoke(scope)
        precondition(reopened.list(scope).isEmpty)
        fails { _ = try reopened.load(scope, id) }
        try store.save(transfer(replacement, scope: rotated), to: rotated)
        try store.remove(rotated, id)
        precondition(store.list(rotated).isEmpty)
        print("OfflineHistory: integrity, cold reopen, quota, binding isolation, revoke and remove passed")
    }
}
