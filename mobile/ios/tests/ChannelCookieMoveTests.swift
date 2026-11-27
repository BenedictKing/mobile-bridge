import Foundation

@main @MainActor struct ChannelCookieMoveTests {
    private static func cookie(_ host: String, _ value: String, secure: Bool = false) -> HTTPCookie {
        var fields: [HTTPCookiePropertyKey: Any] = [
            .name: "bridge", .domain: host, .path: "/", .value: value,
            .expires: Date(timeIntervalSince1970: 2_100_000_000)
        ]
        if secure { fields[.secure] = "TRUE" }
        return HTTPCookie(properties: fields)!
    }

    private final class Jar {
        var values: [HTTPCookie]
        var writes: [String] = []
        var rejectWrites: Set<String> = []
        var rejectDelete = false
        var onSet: (String) -> Void = { _ in }
        init(_ values: [HTTPCookie]) { self.values = values }
        func set(_ cookie: HTTPCookie) {
            writes.append(cookie.value)
            guard !rejectWrites.contains(cookie.value) else { return }
            values.removeAll(where: ChannelCookieMove.Slot(cookie).matches)
            values.append(cookie)
            onSet(cookie.value)
        }
        func remove(_ cookie: HTTPCookie) {
            writes.append("-" + cookie.value)
            if !rejectDelete { values.removeAll(where: ChannelCookieMove.Slot(cookie).matches) }
        }
    }

    private static func move(_ jar: Jar, target: HTTPCookie, sourceHTTPS: Bool = false,
                             valid: () -> Bool = { true }, unchanged: () -> Bool = { true },
                             prepare: () async -> Bool = { true }, commit: () -> Bool = { true },
                             compensate: () -> ChannelCookieMove.Compensation = { .restore }) async -> ChannelCookieMove.Outcome {
        let move = ChannelCookieMove(cookie: target, sourceSlot: .init(name: "bridge", host: "source.test", path: "/"),
            sourceHTTPS: sourceHTTPS, requiresSource: true, knownTokens: ["source", "previous", "next"],
            read: { jar.values }, set: { jar.set($0) }, remove: { jar.remove($0) })
        return await move.run(valid: valid, bindingUnchanged: unchanged,
                              prepare: prepare, commit: commit, reject: {}, compensation: compensate)
    }

    static func main() async {
        let source = cookie("source.test", "source"), previous = cookie("target.test", "previous")
        let target = cookie("target.test", "next", secure: true)
        let shared = cookie("source.test", "next", secure: true)

        let sameSlot = Jar([source])
        let success = await move(sameSlot, target: shared)
        precondition(success == .committed && sameSlot.values.map(\.value) == ["next"] && sameSlot.values[0].isSecure)

        let stale = Jar([source])
        var current = true
        let cancelled = await move(stale, target: shared, valid: { current }, prepare: { current = false; return false })
        precondition(cancelled == .stale && stale.writes == ["next", "source"] && !stale.values[0].isSecure)

        let failure = Jar([source, previous])
        let rejected = await move(failure, target: target, prepare: { false }, commit: { preconditionFailure("Handshaking failed") })
        precondition(rejected == .failed && failure.writes == ["next", "previous"])

        let keychain = Jar([source])
        let uncommitted = await move(keychain, target: target, commit: { false })
        precondition(uncommitted == .failed && keychain.writes == ["next", "-next"])

        let ignored = Jar([source, previous])
        ignored.rejectWrites = ["next"]
        let ignoredResult = await move(ignored, target: target, prepare: { preconditionFailure("Write did not land") })
        precondition(ignoredResult == .failed && ignored.writes == ["next"] && ignored.values.map(\.value) == ["source", "previous"])

        let weakened = Jar([source, previous])
        weakened.onSet = { value in
            if value == "next" { weakened.values[1] = cookie("target.test", value) }
        }
        let insecure = await move(weakened, target: target, prepare: { preconditionFailure("Secure was stripped") })
        precondition(insecure == .failed && weakened.writes == ["next", "previous"])

        let foreign = Jar([source, cookie(".target.test", "another-login")])
        let denied = await move(foreign, target: target)
        precondition(denied == .conflict && foreign.writes.isEmpty)

        let rotated = Jar([source, previous])
        var binding = true
        let invalidated = await move(rotated, target: target, unchanged: { binding }, prepare: {
            binding = false
            rotated.set(cookie("target.test", "new-login"))
            return true
        }, commit: { preconditionFailure("Binding rotated") }, compensate: { .leave })
        precondition(invalidated == .failed && rotated.writes == ["next", "new-login"])

        let logout = Jar([source, previous])
        let revoked = await move(logout, target: target, prepare: { false }, compensate: { .remove })
        precondition(revoked == .failed && logout.writes == ["next", "-next"] && logout.values.map(\.value) == ["source"])

        let lateLogout = Jar([source, previous])
        var loggedOut = false
        lateLogout.onSet = { if $0 == "previous" { loggedOut = true } }
        let lateRevoked = await move(lateLogout, target: target, prepare: { false },
                                     compensate: { loggedOut ? .remove : .restore })
        precondition(lateRevoked == .failed && lateLogout.writes == ["next", "previous", "-previous"] &&
                     lateLogout.values.map(\.value) == ["source"])

        let sourceChanged = Jar([source, previous])
        let sourceResult = await move(sourceChanged, target: target, prepare: {
            sourceChanged.set(cookie("source.test", "new-source")); return true
        }, commit: { preconditionFailure("Source rotated") })
        precondition(sourceResult == .failed && sourceChanged.values.map(\.value) == ["new-source", "previous"])

        let stuck = Jar([source])
        stuck.rejectDelete = true
        let unknown = await move(stuck, target: target, prepare: { false })
        precondition(unknown == .rollbackUnverified)

        let queue = ChannelCookieMoves()
        var order: [String] = []
        var resume: CheckedContinuation<Void, Never>?
        let first = Task { await queue.run {
            order.append("stage")
            await withCheckedContinuation { resume = $0 }
            order.append("rollback")
        }}
        while resume == nil { await Task.yield() }
        let second = Task {
            order.append("waiting")
            await queue.run { order.append("next-stage") }
        }
        while !order.contains("waiting") { await Task.yield() }
        precondition(order == ["stage", "waiting"])
        resume!.resume()
        await first.value; await second.value
        precondition(order == ["stage", "waiting", "rollback", "next-stage"])
        print("ChannelCookieMove: 13 scenarios passed")
    }
}
