import Foundation

@main
struct LANContractTest {
    static func main() async throws {
        let args = CommandLine.arguments
        let target = args[1], device = args[2], root = args[3], name = args[4]
        let anchor = "https://unreachable.invalid"
        let wrong = await GatewayChannels.prepare(String(repeating: "f", count: 32), anchor: anchor,
            target: target, root: root, name: name)
        precondition(!wrong, "wrong device must not bootstrap")
        let ready = await GatewayChannels.prepare(device, anchor: anchor, target: target, root: root, name: name)
        precondition(ready, "new LAN origin must bootstrap without the anchor")
        let again = await GatewayChannels.prepare(device, anchor: anchor, target: target, root: root, name: name)
        precondition(again, "bound route must remain usable")
        let config = URLSessionConfiguration.ephemeral
        config.httpCookieStorage = nil
        let session = URLSession(configuration: config)
        defer { session.invalidateAndCancel() }
        var request = URLRequest(url: URL(string: target + "/api/auth")!)
        request.setValue(name + "=" + GatewayChannels.token(root, anchor: anchor, target: target), forHTTPHeaderField: "Cookie")
        request.setValue("BridgeMobile/0.1-iOS", forHTTPHeaderField: "User-Agent")
        let (data, response) = try await session.data(for: request)
        let auth = try JSONSerialization.jsonObject(with: data) as! [String: Any]
        precondition((response as? HTTPURLResponse)?.statusCode == 200 && auth["authenticated"] as? Bool == true)
        print("Swift: root proof, new route bootstrap, reused route and route auth passed")
    }
}
