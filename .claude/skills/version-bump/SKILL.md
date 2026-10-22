---
name: version-bump
description: 升级 mobile-bridge 版本号并发布：更新 VERSION 版本源、校验补全 CHANGELOG、重写 RELEASE_NOTES、通过 make check 门禁后自动提交，可选打 tag 并推送触发 GitHub Actions 发布。当用户提到升级版本、版本号、发布版本、更新版本、bump、release、打 tag 发布、准备发版时使用此技能，即使用户没有明说"版本号升级"。
allowed-tools: Bash, Read, Write, Edit
---

# 版本号升级技能（mobile-bridge）

## 版本来源

- **唯一权威来源**：根目录 `VERSION`，单行，格式 `v{major}.{minor}.{patch}`（可含预发布后缀，如 `v2.0.0-preview.4`）。
- **派生同步项**：`wails/wails.json` 的 `info.productVersion`（同版本号，**无 `v` 前缀**）。
- 不修改 `go.mod` / `wails/go.mod`（其中是依赖版本，与应用版本无关）。
- `VERSION` 缺失时中止并提示用户（不自动重建，避免掩盖意外删除）。

## 触发条件与参数

用户输入包含：升级版本、版本号、发布版本、更新版本、bump、release、准备发版 → 执行本流程。

| 参数 | 含义 |
| --- | --- |
| 无参数 / `patch` | patch +1（默认） |
| `minor` | minor +1，patch 归零 |
| `major` | major +1，minor、patch 归零 |
| 具体版本号（如 `2.1.0`、`2.0.0-preview.4`） | 直接使用该版本号 |
| `--no-tag` / "不要 tag" | 只提交，不打 tag 不推送 |
| 主题一句话（可选） | 用于 RELEASE_NOTES 标题 |

## 发布契约预检（三态）

打 tag 前必须确认 `.github/workflows/release.yml` 可用：

1. `release.yml` 存在，validate 段读取 `VERSION` 校验 tag。
2. 不再引用已移除的 `package.json`、`scripts/sign-update.cjs`、`desktop/update-public-key.pem`。
3. `RELEASE_NOTES.md` 存在。

判定：

- 全部通过 → `contract-ok`，可打 tag。
- 引用缺失或不一致 → `contract-broken`：**拒绝打 tag**，报告缺失项并提示修复（版本落盘、CHANGELOG、commit 不受影响，照常进行）。
- `release.yml` 不存在 → `no-release-workflow`：仅提示无 CI 发布，流程照常。

## 执行步骤

### 1. 预检

```bash
cat VERSION
git status --porcelain
git branch --show-current
git rev-parse -q --verify "refs/tags/v{新版本号}"
```

- 根据参数计算新版本（semver 合法，且必须大于当前版本，含预发布语义比较）。
- 目标 tag 已存在 → 中止。
- 工作区有本流程文件（VERSION、CHANGELOG.md、RELEASE_NOTES.md、wails/wails.json）之外的未提交改动 → 询问用户是否一并提交；拒绝则中止。

### 2. CHANGELOG 校验与补全

以 `git describe --tags --abbrev=0` 为 base（如 `v0.1.0`）：

```bash
git log {base}..HEAD --pretty=%s | grep -cvE "^(Merge |chore\(release\))"
```

- 有效提交为 0 → 中止（没有新变更）。
- 逐条与 `## [Unreleased]` 区块比对，遗漏提交生成条目：`feat` → `### Added`；`fix` / `perf` / `refactor` → `### Changed / Fixed`；其余（docs / chore / ci / build）→ `### Changed / Fixed`。沿用仓库现有的中文长段风格。
- **先处理错位**：出现在旧版本区块中的本版本提交条目，剪切移动到 `[Unreleased]`，避免同一提交出现两次。
- **合并重复的 `### Changed / Fixed` 标题**（历史遗留多处分节），条目去重。
- 覆盖率校验必须通过：已记录条目数 ≥ 有效提交数（关联提交可合并为一条），否则回溯补全。

### 3. 版本落盘

- `echo "v{新版本号}" > VERSION`
- `wails/wails.json`：`info.productVersion` 精确替换为 `{新版本号去 v}`，保持 JSON 合法。
- `CHANGELOG.md`：`## [Unreleased]` 归档为 `## [v{新版本号}] - YYYY-MM-DD`，并在其上新建空骨架（沿用现有分组标题）：

  ```markdown
  ## [Unreleased]

  ### Added

  ### Changed / Fixed

  ## [v{新版本号}] - YYYY-MM-DD
  ```

- 扫描硬编码旧版本引用，报告中列出，**不自动修改**：

  ```bash
  grep -rn "v{旧版本号}" README.md HOMEBREW.md docs/ 2>/dev/null
  ```

### 4. RELEASE_NOTES.md 重写

整文件替换为当前版本发布说明（`release.yml` 以它作 `--notes-file`）：

```markdown
## v{新版本号} · {一句话主题}

（CHANGELOG 新版本节内容，去掉区块标题行）
```

主题取用户输入，或从本版本主要内容提炼一句话。

### 5. 验证门禁

```bash
make check
```

- `make check` = build + vet + test + race，**必须通过**。
- 失败 → 中止：报告失败步骤，改动保留在工作区，不 commit、不打 tag。
- 用户要求时可追加 `make conformance`（需 python3）。

### 6. 提交与出站

验证通过后自动本地提交（遵循全局规则，无需询问）：

```bash
git add VERSION CHANGELOG.md RELEASE_NOTES.md wails/wails.json
git commit -m "chore(release): v{新版本号}" -m "{可选补充行}"
```

打 tag（本地动作；`--no-tag` 跳过）：

```bash
git tag -a "v{新版本号}" -m "v{新版本号}"
```

**推送必须先经用户显式确认**（连同当前分支尚未推送的既有提交一并说明）：

```bash
git push origin {当前分支}
git push origin "v{新版本号}"
```

推送后提示跟踪发布构建：

```bash
gh run list --limit 5
gh run watch
```

## 中止条件

- `VERSION` 缺失或格式非法
- 新版本 ≤ 当前版本，或目标 tag 已存在
- 无有效提交（没有新变更）
- 工作区有流程外未提交改动且用户拒绝一并处理
- `make check` 失败
- `contract-broken` 时打 tag 请求（文档与 commit 部分照常，仅 tag/推送被拒）

## 确认点

1. **tag 创建**：本地动作，默认执行；`--no-tag` 整体跳过 tag 与 push。
2. **push**：出站动作，永远需要用户明确"是 / 确认 / 继续"；commit + tag 分开列出待推送内容。

## 输出格式

```
版本升级完成:
- 原版本: v0.1.0
- 新版本: v0.2.0
- 升级类型: minor
- CHANGELOG: 补全 N 条，合并重复分组
- 旧版本引用扫描: README.md:41 等（未自动修改）
- 验证: make check 通过
- Commit: {hash}（chore(release): v0.2.0）
- Tag: v0.2.0（已创建 / 未创建）
- 推送: 已推送（release run 已触发）/ 等待确认
```

中止时输出：失败的步骤、原因、修复建议；已产生改动的状态。

## 示例场景

1. **"升级版本并发布"** → patch +1，全流程走完，push 前确认。
2. **"发 minor 版本，不要 tag"** → minor +1，提交后停止。
3. **"版本号改为 2.0.0-preview.4"** → 指定版本；连字符 tag 在 CI 中自动标记 prerelease。
4. **"当前版本是多少"** → 只读回答 `cat VERSION`，不触发流程。

## 注意事项

- 提交信息用 `git commit -m` 多参拼接，不写 `.git/COMMIT_EDITMSG`。
- `wails/wails.json` 必须保持合法 JSON。
- 预发布版本（含连字符）在 release workflow 中自动 `--prerelease`，发布为 draft，需在 GitHub Releases 页手动确认。
- 本 skill 只在用户显式确认后 push；绝不 force push、不改写历史。
