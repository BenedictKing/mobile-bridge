import Foundation
import Darwin

enum DshMobileURL {
    struct Invalid: LocalizedError { var errorDescription: String? { "请使用完整的 DSH Mobile HTTPS 链接，或局域网 HTTP 链接。" } }
    static func connection(_ input: String) throws -> URL {
        let value = input.trimmingCharacters(in: .whitespacesAndNewlines)
        guard value.utf16.count <= 8192, !value.unicodeScalars.contains(where: { $0.value <= 32 || $0.value == 127 || $0.value == 92 }),
              value.range(of: #"%(?![0-9a-fA-F]{2})"#, options: .regularExpression) == nil,
              var c = URLComponents(string: value), let host = c.host, !host.isEmpty, let scheme = c.scheme?.lowercased(),
              c.user == nil, c.password == nil, c.port == nil || (1...65535).contains(c.port!),
              scheme == "https" || (scheme == "http" && local(host)) else { throw Invalid() }
        c.scheme = scheme; c.host = host.lowercased()
        if c.percentEncodedPath.isEmpty { c.percentEncodedPath = "/" }
        guard let url = c.url else { throw Invalid() }; return url
    }
    static func local(_ host: String) -> Bool {
        if GatewayURL.local(host) { return true }
        let literal = host.trimmingCharacters(in: CharacterSet(charactersIn: "[]"))
        var address = in6_addr()
        guard !literal.contains("%"), inet_pton(AF_INET6, literal, &address) == 1 else { return false }
        return withUnsafeBytes(of: address) { bytes in (bytes[0] & 254) == 252 || bytes[0] == 254 && (bytes[1] & 192) == 128 }
    }
    static func display(_ url: URL) -> String {
        guard var c = URLComponents(url: url, resolvingAgainstBaseURL: false) else { return "DSH Mobile" }
        c.path = ""; c.query = nil; c.fragment = nil; return c.string ?? "DSH Mobile"
    }
    static func same(_ url: URL, _ base: URL) -> Bool {
        guard let valid = try? connection(url.absoluteString) else { return false }
        return valid.scheme == base.scheme && valid.host == base.host && (valid.port ?? (valid.scheme == "https" ? 443 : 80)) == (base.port ?? (base.scheme == "https" ? 443 : 80))
    }
}
