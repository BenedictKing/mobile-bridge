import UIKit
@preconcurrency import WebKit
import Security

/// Only opaque IDs are stored in preferences. Full pairing links live in the device Keychain.
enum DshMobileStore {
    static var ids: [String] { UserDefaults.standard.stringArray(forKey: "dshMobileConnections") ?? [] }
    private static func query(_ id: String) -> [String: Any] { [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: "codex-bridge-dsh-mobile-v1", kSecAttrAccount as String: id] }
    static func load(_ id: String) -> URL? {
        var q = query(id); q[kSecReturnData as String] = true; q[kSecMatchLimit as String] = kSecMatchLimitOne
        var item: CFTypeRef?
        guard SecItemCopyMatching(q as CFDictionary, &item) == errSecSuccess, let data = item as? Data, let value = String(data: data, encoding: .utf8) else { return nil }
        return try? DshMobileURL.connection(value)
    }
    @discardableResult static func save(_ url: URL) throws -> String {
        let id = ids.first(where: { load($0) == url }) ?? UUID().uuidString
        var q = query(id); q[kSecValueData as String] = Data(url.absoluteString.utf8); q[kSecAttrAccessible as String] = kSecAttrAccessibleWhenUnlockedThisDeviceOnly
        let result = SecItemAdd(q as CFDictionary, nil)
        guard result == errSecSuccess || result == errSecDuplicateItem else { throw DshMobileURL.Invalid() }
        if !ids.contains(id) { UserDefaults.standard.set(ids + [id], forKey: "dshMobileConnections") }
        return id
    }
    static func remove(_ id: String) { SecItemDelete(query(id) as CFDictionary); UserDefaults.standard.set(ids.filter { $0 != id }, forKey: "dshMobileConnections") }
}

/// External content has its own website store, with no Bridge scripts or message handlers.
final class DshMobileController: UIViewController, WKNavigationDelegate, WKUIDelegate {
    private let entry: URL
    private var web: WKWebView!
    init(_ url: URL) { entry = url; super.init(nibName: nil, bundle: nil) }
    required init?(coder: NSCoder) { fatalError("init(coder:) has not been implemented") }
    override func viewDidLoad() {
        super.viewDidLoad(); title = "DSH Mobile"; view.backgroundColor = .systemBackground
        navigationItem.largeTitleDisplayMode = .never
        navigationController?.navigationBar.tintColor = .label
        navigationItem.leftBarButtonItem = UIBarButtonItem(image: UIImage(systemName: "desktopcomputer"), style: .plain, target: self, action: #selector(close))
        navigationItem.leftBarButtonItem?.accessibilityLabel = MobileStrings.text("我的电脑")
        navigationItem.rightBarButtonItem = UIBarButtonItem(barButtonSystemItem: .refresh, target: self, action: #selector(refresh))
        navigationItem.rightBarButtonItem?.accessibilityLabel = MobileStrings.text("刷新")
        let config = WKWebViewConfiguration(); config.websiteDataStore = .nonPersistent()
        web = WKWebView(frame: .zero, configuration: config); web.navigationDelegate = self; web.uiDelegate = self; web.allowsBackForwardNavigationGestures = true
        web.translatesAutoresizingMaskIntoConstraints = false; view.addSubview(web)
        NSLayoutConstraint.activate([web.topAnchor.constraint(equalTo: view.safeAreaLayoutGuide.topAnchor), web.bottomAnchor.constraint(equalTo: view.safeAreaLayoutGuide.bottomAnchor), web.leadingAnchor.constraint(equalTo: view.leadingAnchor), web.trailingAnchor.constraint(equalTo: view.trailingAnchor)])
        web.load(URLRequest(url: entry))
    }
    @objc private func close() { dismiss(animated: true) }
    @objc private func refresh() { web.reload() }
    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction, decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
        guard let url = action.request.url else { decisionHandler(.cancel); return }
        if DshMobileURL.same(url, entry), action.targetFrame != nil { decisionHandler(.allow); return }
        decisionHandler(.cancel)
        guard action.navigationType == .linkActivated, action.targetFrame?.isMainFrame != false, ["https", "http"].contains(url.scheme ?? "") else { return }
        let alert = UIAlertController(title: MobileStrings.text("在浏览器打开外部链接？"), message: url.host, preferredStyle: .alert)
        alert.addAction(UIAlertAction(title: MobileStrings.text("取消"), style: .cancel))
        alert.addAction(UIAlertAction(title: MobileStrings.text("打开"), style: .default) { _ in UIApplication.shared.open(url) }); present(alert, animated: true)
    }
    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
        guard (error as NSError).code != NSURLErrorCancelled else { return }
        let alert = UIAlertController(title: MobileStrings.text(MobileStrings.text("连接失败，请刷新重试")), message: nil, preferredStyle: .alert)
        alert.addAction(UIAlertAction(title: MobileStrings.text("确定"), style: .default)); present(alert, animated: true)
    }
}
