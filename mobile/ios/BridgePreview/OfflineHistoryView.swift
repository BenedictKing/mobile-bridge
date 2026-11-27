import UIKit

final class OfflineHistoryListController: UITableViewController {
    private let store: OfflineHistoryStore
    private let scopes: () -> [OfflineHistoryScope: String]
    private var entries: [(OfflineHistoryStore.Entry, String)] = []
    private var reload: Task<Void, Never>?

    init(store: OfflineHistoryStore, scopes: @escaping () -> [OfflineHistoryScope: String]) {
        self.store = store; self.scopes = scopes
        super.init(style: .insetGrouped)
        title = MobileStrings.text("离线历史")
    }
    required init?(coder: NSCoder) { fatalError() }

    override func viewDidLoad() {
        super.viewDidLoad()
        tableView.register(UITableViewCell.self, forCellReuseIdentifier: "archive")
        NotificationCenter.default.addObserver(self, selector: #selector(refresh), name: UIApplication.didBecomeActiveNotification, object: nil)
    }
    override func viewWillAppear(_ animated: Bool) { super.viewWillAppear(animated); refresh() }

    @objc private func refresh() {
        reload?.cancel()
        let allowed = Array(scopes().keys), store = self.store
        entries.removeAll(); tableView.reloadData()
        empty("正在读取离线历史…")
        reload = Task { [weak self] in
            let entries = await Task.detached(priority: .userInitiated) { allowed.flatMap(store.list) }.value
            guard let self, !Task.isCancelled else { return }
            let current = self.scopes()
            self.entries = entries.sorted { ($0.capturedAt ?? 0) > ($1.capturedAt ?? 0) }.compactMap { entry in
                current[entry.scope].map { (entry, $0) }
            }
            if self.entries.isEmpty { self.empty("暂无离线历史。请先在已配对电脑的菜单中保存聊天。") }
            else { self.tableView.backgroundView = nil }
            self.tableView.reloadData()
        }
    }

    private func empty(_ text: String) {
        let label = UILabel()
        label.font = .preferredFont(forTextStyle: .body); label.adjustsFontForContentSizeCategory = true
        label.textAlignment = .center; label.textColor = .secondaryLabel; label.numberOfLines = 0
        label.text = MobileStrings.text(text)
        tableView.backgroundView = label
    }
    override func tableView(_ tableView: UITableView, numberOfRowsInSection section: Int) -> Int { entries.count }
    override func tableView(_ tableView: UITableView, cellForRowAt indexPath: IndexPath) -> UITableViewCell {
        let (entry, computer) = entries[indexPath.row]
        let cell = tableView.dequeueReusableCell(withIdentifier: "archive", for: indexPath)
        var content = UIListContentConfiguration.subtitleCell()
        content.text = entry.title.map { $0.isEmpty ? MobileStrings.text("Codex 聊天") : $0 } ?? MobileStrings.text("无法读取的离线历史")
        content.secondaryText = [computer, entry.capturedAt.map { Date(timeIntervalSince1970: $0).formatted(date: .abbreviated, time: .shortened) }].compactMap { $0 }.joined(separator: " · ")
        cell.contentConfiguration = content; cell.accessoryType = .disclosureIndicator
        return cell
    }
    override func tableView(_ tableView: UITableView, didSelectRowAt indexPath: IndexPath) {
        tableView.deselectRow(at: indexPath, animated: true)
        let (entry, _) = entries[indexPath.row]
        let scope = entry.scope, id = entry.id, store = self.store
        let allowed = { [weak self] in self?.scopes()[scope] != nil }
        guard allowed() else { refresh(); return }
        Task { [weak self] in
            let history = await Task.detached(priority: .userInitiated) { try? store.load(scope, id) }.value
            guard let self, self.navigationController?.topViewController === self, allowed() else { return }
            guard let history else { self.alert("无法读取的离线历史"); return }
            self.navigationController?.pushViewController(OfflineHistoryReader(history: history, allowed: allowed), animated: true)
        }
    }
    override func tableView(_ tableView: UITableView, trailingSwipeActionsConfigurationForRowAt indexPath: IndexPath) -> UISwipeActionsConfiguration? {
        let entry = entries[indexPath.row].0
        let action = UIContextualAction(style: .destructive, title: MobileStrings.text("移除副本")) { [weak self] _,_,done in
            guard let self, self.scopes()[entry.scope] != nil else { done(false); return }
            do { try self.store.remove(entry.scope, entry.id); done(true); self.refresh() }
            catch { done(false); self.alert("无法移除离线历史") }
        }
        let configuration = UISwipeActionsConfiguration(actions: [action])
        configuration.performsFirstActionWithFullSwipe = false
        return configuration
    }
    private func alert(_ key: String) {
        let alert = UIAlertController(title: MobileStrings.text(key), message: nil, preferredStyle: .alert)
        alert.addAction(UIAlertAction(title: MobileStrings.text("好"), style: .default))
        present(alert, animated: true)
    }
}

private final class OfflineHistoryReader: UITableViewController {
    private let history: OfflineHistory
    private let allowed: () -> Bool
    init(history: OfflineHistory, allowed: @escaping () -> Bool) {
        self.history = history; self.allowed = allowed
        super.init(style: .insetGrouped)
        title = history.title.isEmpty ? MobileStrings.text("Codex 聊天") : history.title
        navigationItem.largeTitleDisplayMode = .never
    }
    required init?(coder: NSCoder) { fatalError() }
    override func viewDidLoad() {
        super.viewDidLoad()
        tableView.register(UITableViewCell.self, forCellReuseIdentifier: "text")
        NotificationCenter.default.addObserver(self, selector: #selector(checkBinding), name: UIApplication.didBecomeActiveNotification, object: nil)
    }
    override func viewWillAppear(_ animated: Bool) { super.viewWillAppear(animated); checkBinding() }
    @objc private func checkBinding() { if !allowed() { navigationController?.popViewController(animated: false) } }

    override func tableView(_ tableView: UITableView, titleForHeaderInSection section: Int) -> String? {
        MobileStrings.text("已保存的只读副本") + " · " + Date(timeIntervalSince1970: history.capturedAt).formatted(date: .abbreviated, time: .shortened)
    }
    override func tableView(_ tableView: UITableView, titleForFooterInSection section: Int) -> String? {
        MobileStrings.text("截止回合") + ": " + history.throughTurn
    }
    override func tableView(_ tableView: UITableView, numberOfRowsInSection section: Int) -> Int { allowed() ? history.rows.count : 0 }
    override func tableView(_ tableView: UITableView, cellForRowAt indexPath: IndexPath) -> UITableViewCell {
        let row = history.rows[indexPath.row], cell = tableView.dequeueReusableCell(withIdentifier: "text", for: indexPath)
        var config = UIListContentConfiguration.subtitleCell()
        config.text = role(row); config.secondaryText = String(row.text.unicodeScalars.prefix(360))
        config.secondaryTextProperties.numberOfLines = 6
        cell.contentConfiguration = config; cell.accessoryType = .disclosureIndicator
        return cell
    }
    override func tableView(_ tableView: UITableView, didSelectRowAt indexPath: IndexPath) {
        guard allowed() else { checkBinding(); return }
        let row = history.rows[indexPath.row]
        navigationController?.pushViewController(OfflineHistoryText(title: role(row), body: row.text, allowed: allowed), animated: true)
    }
    private func role(_ row: OfflineHistory.Row) -> String {
        switch row.role {
        case "user": return MobileStrings.text("你")
        case "assistant", "agent": return "Codex"
        case "error": return MobileStrings.text("错误")
        default: return MobileStrings.text("执行过程")
        }
    }
}

private final class OfflineHistoryText: UIViewController {
    private let body: String, allowed: () -> Bool
    init(title: String, body: String, allowed: @escaping () -> Bool) {
        self.body = body; self.allowed = allowed
        super.init(nibName: nil, bundle: nil); self.title = title
    }
    required init?(coder: NSCoder) { fatalError() }
    override func loadView() {
        let text = UITextView()
        text.backgroundColor = .systemBackground; text.textColor = .label
        text.isEditable = false; text.isSelectable = true; text.adjustsFontForContentSizeCategory = true
        text.font = .preferredFont(forTextStyle: .body)
        text.textContainerInset = UIEdgeInsets(top: 16, left: 16, bottom: 16, right: 16)
        text.text = allowed() ? body : nil
        view = text
    }
    override func viewDidLoad() {
        super.viewDidLoad()
        NotificationCenter.default.addObserver(self, selector: #selector(checkBinding), name: UIApplication.didBecomeActiveNotification, object: nil)
    }
    @objc private func checkBinding() {
        if !allowed() { (view as? UITextView)?.text = nil; navigationController?.popToRootViewController(animated: false) }
    }
}
