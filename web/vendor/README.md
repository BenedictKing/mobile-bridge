# markdown-it

- 固定版本：15.0.2（MIT，见 `markdown-it.LICENSE`）
- 上游：https://github.com/markdown-it/markdown-it
- 分发包：https://registry.npmjs.org/markdown-it/-/markdown-it-15.0.2.tgz
- 文件：`dist/browser/markdown-it.umd.min.js`，原样保存为 `markdown-it.min.js`
- SHA-256：`635972b985228e8af9f0143647c68616b7a3bb09f6946e7e4a52e43dcf5e7be5`

浏览器从网关本机加载，无需 CDN 或前端构建步骤。升级时同时更新版本、许可证和校验值，并运行 `tests/markdown.test.js` 的浏览器检查。

## KaTeX

- 固定版本：0.18.9（MIT，见 `katex/LICENSE`）
- 上游：https://github.com/KaTeX/KaTeX
- 分发包：https://registry.npmjs.org/katex/-/katex-0.18.9.tgz
- 包完整性：`sha512-8ad9RyoKsb/g8/yLFE+KAlP+DhbCTRUNi/V9XGsxn0R+trJJltNwzcDNo0q/DEkOy5fQUQTAQyCXCYSE+OakTQ==`
- `katex/katex.min.js` SHA-256：`155f6c2d673c5912e3b48f45d8830eaad18ae1953915939ceb648ea8b9c3e7e2`

## markdown-it-texmath

- 固定版本：1.0.0（MIT，见 `texmath.LICENSE`）
- 上游：https://github.com/goessner/markdown-it-texmath
- 分发包：https://registry.npmjs.org/markdown-it-texmath/-/markdown-it-texmath-1.0.0.tgz
- 包完整性：`sha512-4hhkiX8/gus+6e53PLCUmUrsa6ZWGgJW2XCW6O0ASvZUiezIK900ZicinTDtG3kAO2kon7oUA/ReWmpW2FByxg==`
- `texmath.js` SHA-256：`926e075a1745e1019813b8418b90183feea0c7b3782c85dceb4f246da999edc6`

KaTeX 脚本、样式和字体原样保存到 `katex/`；texmath 原样保存为 `texmath.js`。所有资源由本机网关提供，运行时不访问 CDN。公式升级后运行 `tests/math.test.js`，检查代码隔离、危险 TeX、流式输入和窄屏公式滚动。

## Dagre

- 固定版本：`@dagrejs/dagre` 3.1.3、`@dagrejs/graphlib` 4.0.6（MIT，见 `dagre.LICENSE`）
- 上游：https://github.com/dagrejs/dagre
- 浏览器包：https://cdn.jsdelivr.net/npm/@dagrejs/dagre@3.1.3/+esm
- 文件：`dagre.min.js` 由上述浏览器包封装成 UMD，保留完整实现，同时支持浏览器 `globalThis.dagre` 与 Node 测试 `require()`
- SHA-256：`278a58b93f310c268ba67805ca421360bbdd0cdf237958992cecffaaeb4497fd`

团队 Run 页只使用 Dagre 的 DAG 布局，节点和连线仍由项目自身 DOM/SVG 渲染。资源由本机网关加载，运行时不访问 CDN；升级后运行 `tests/test_team_ui.js`。
