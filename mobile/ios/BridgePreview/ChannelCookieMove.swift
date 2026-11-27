import Foundation

/// WebKit shares a Cookie slot across ports and schemes. Keep it reserved until
/// the handoff commits or compensation finishes, including across actor yields.
@MainActor final class ChannelCookieMoves {
    private var busy = false
    private var waiters: [CheckedContinuation<Void, Never>] = []

    func run(_ move: @MainActor () async -> Void) async {
        if busy { await withCheckedContinuation { waiters.append($0) } }
        else { busy = true }
        defer {
            if waiters.isEmpty { busy = false }
            else { waiters.removeFirst().resume() }
        }
        await move()
    }
}

@MainActor struct ChannelCookieMove {
    struct Slot: Equatable {
        let name: String, host: String, path: String
        init(name: String, host: String, path: String) {
            self.name = name
            self.host = host.trimmingCharacters(in: CharacterSet(charactersIn: ".")).lowercased()
            self.path = path
        }
        init(_ cookie: HTTPCookie) { self.init(name: cookie.name, host: cookie.domain, path: cookie.path) }
        func matches(_ cookie: HTTPCookie) -> Bool { self == Slot(cookie) }
    }

    enum Outcome: Equatable { case committed, stale, conflict, failed, rollbackUnverified }
    enum Compensation { case restore, remove, leave }

    let cookie: HTTPCookie
    let sourceSlot: Slot
    let sourceHTTPS: Bool
    let requiresSource: Bool
    let knownTokens: Set<String>
    let read: @MainActor () async -> [HTTPCookie]
    let set: @MainActor (HTTPCookie) async -> Void
    let remove: @MainActor (HTTPCookie) async -> Void

    private func source(_ cookies: [HTTPCookie]) -> [HTTPCookie] {
        cookies.filter {
            sourceSlot.matches($0) && (!$0.isSecure || sourceHTTPS) &&
                ($0.expiresDate.map { $0 > Date() } ?? true)
        }
    }

    private static func same(_ a: HTTPCookie?, _ b: HTTPCookie?) -> Bool {
        guard let a, let b else { return a == nil && b == nil }
        let sameExpiration: Bool
        if let lhs = a.expiresDate, let rhs = b.expiresDate { sameExpiration = abs(lhs.timeIntervalSince(rhs)) < 2 }
        else { sameExpiration = a.expiresDate == nil && b.expiresDate == nil }
        return Slot(a) == Slot(b) && a.domain.lowercased() == b.domain.lowercased() &&
            a.value == b.value && a.isSecure == b.isSecure &&
            a.isHTTPOnly == b.isHTTPOnly && sameExpiration
    }

    func run(valid: @MainActor () -> Bool, bindingUnchanged: @MainActor () -> Bool,
             prepare: @MainActor () async -> Bool, commit: @MainActor () -> Bool, reject: @MainActor () -> Void,
             compensation: @MainActor () -> Compensation) async -> Outcome {
        guard valid() else { return .stale }
        let target = Slot(cookie), initial = await read(), previous = initial.filter(target.matches)
        let originalSource = source(initial)
        guard valid() else { return .stale }
        guard bindingUnchanged(), previous.count <= 1, originalSource.count <= 1,
              (previous + originalSource).allSatisfy({ knownTokens.contains($0.value) }) else { return .conflict }
        guard !requiresSource || !originalSource.isEmpty else { return .failed }

        let expectedSource = target == sourceSlot ? source([cookie]).first : originalSource.first
        func written(_ values: [HTTPCookie]) -> Bool {
            let found = values.filter(target.matches), sourceCookies = source(values)
            return found.count == 1 && Self.same(found.first, cookie) &&
                sourceCookies.count <= 1 && Self.same(sourceCookies.first, expectedSource)
        }

        await set(cookie)
        let staged = await read()
        if valid(), bindingUnchanged(), written(staged), await prepare() {
            let latest = await read()
            if valid(), bindingUnchanged(), written(latest), commit() { return .committed }
        }

        reject() // Release a waiting WebKit prompt before another CookieStore read.
        let latest = await read().filter(target.matches)
        let stillCurrent = valid()
        let failure: Outcome = stillCurrent ? .failed : .stale
        let action = compensation()
        guard action != .leave else { return failure }
        let carriesNewToken = previous.first?.value != cookie.value
        guard latest.count == 1, let stagedCookie = latest.first,
              Self.same(stagedCookie, cookie) || (carriesNewToken && stagedCookie.value == cookie.value) else {
            // A rejected write or a later login owns the actual slot.
            return carriesNewToken && latest.contains(where: { $0.value == cookie.value }) ? .rollbackUnverified : failure
        }
        let restore = action == .restore ? previous.first : nil
        if let restore { await set(restore) }
        else { await remove(stagedCookie) }
        let checked = await read().filter(target.matches)
        if let restore {
            switch compensation() {
            case .leave: return failure // A new binding took over.
            case .restore: return checked.count == 1 && Self.same(checked.first, restore) ? failure : .rollbackUnverified
            case .remove:
                // Logout could have arrived while WebKit was restoring the old value.
                guard checked.count == 1, Self.same(checked.first, restore) else {
                    return checked.isEmpty ? failure : .rollbackUnverified
                }
                await remove(restore)
                let remaining = await read().filter(target.matches)
                return compensation() == .leave || !remaining.contains(where: { $0.value == restore.value }) ? failure : .rollbackUnverified
            }
        }
        return compensation() == .leave || !checked.contains(where: { $0.value == cookie.value }) ? failure : .rollbackUnverified
    }
}
