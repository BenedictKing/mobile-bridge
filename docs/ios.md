# iOS 原生容器

当前 iOS 目标位于 `mobile/ios/BridgePreview.xcodeproj`，包含主 App、Live Activity 扩展和通知服务扩展。业务聊天仍由根目录 `web/` 提供；`mobile/shared/web/` 只放两端共用的原生适配脚本。

## 构建

需要 macOS、Xcode 和 iOS SDK。在仓库根目录运行：

```bash
make build-ios
```

产物为 `dist/mobile-bridge-<version>-iOS-unsigned.ipa`。这是未签名包，适合检查构建和资源完整性，不能直接安装到 iPhone。脚本会验证三个 bundle 的 ID、版本、arm64 可执行文件以及共享脚本和客户端图标是否打包。

也可以直接打开 `mobile/ios/BridgePreview.xcodeproj`，在三个 target 的 Signing & Capabilities 中选择自己的 Apple Developer Team，再连接设备运行。覆盖安装时必须保持相同的 Bundle ID 和签名身份，才能保留 Keychain 绑定与 WebView 数据。

## 原生壳包含的能力

- 保存、重命名、切换和移除多台电脑网关。
- 使用 Keychain 保存设备绑定，不把网关密码写入手机。
- 局域网/公网入口探测、延迟展示和自动信道恢复，写操作仍固定走当前公网入口。
- 文件下载断点恢复、文件选择、通知服务扩展和按电脑选择通知来源。
- iOS 16.2 及以上的执行状态 Live Activity；系统能力或推送未配置时会回退到前台同步。
- 共享剪贴板、主题、系统栏、页面导航与 HTML/图片预览隔离。

这些能力来自上游原生壳代码，是否在当前网关端到端可用还取决于 API 契约。当前 Go 网关已经提供网页聊天、`/api/auth`、会话、直接局域网读取和文件路由；上游 iOS 还依赖的 `/api/mobile/events`、`/api/mobile/push`、`/api/connection/prove`、`/api/connection/routes` 及部分诊断接口尚未在本仓库补齐。因此当前 IPA 可以构建并打开网页工作台，但后台电脑通知、持久设备绑定、入口迁移和完整自动信道恢复需要下一阶段先补后端协议。

当前没有在真实 iPhone 或签名安装环境完成验收；模拟器设备也取决于本机是否安装对应 Runtime。构建通过不代表相机扫码、蜂窝网络、锁屏通知、Live Activity 或 App 覆盖升级已经实机验证。
