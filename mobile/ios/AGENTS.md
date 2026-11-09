# iOS 原生壳

- `BridgePreview/App.swift` 负责原生界面、导航、WKWebView 回调和接入边界；`WebScripts.swift` 只读取随 App 打包的脚本并提供 iOS viewport 设置。
- 两端共有的页面脚本只改 `../shared/web/`，不得重新复制进 Swift。同步 `BridgePreview.xcodeproj/project.pbxproj` 的主 App Resources；Android 会话续期脚本不进入 iOS。
- Bundle 资源缺失或 UTF-8 读取失败必须向连接调用者抛错，用既有 `info` 显示，不能返回空脚本、忽略错误或终止 App。页面加载前必须取得完整脚本。
- 保留每次连接生成的 token、当前 WebView 身份、主框架、source/current origin、前台状态和大小校验。`WKUserScript` 仍只在主框架 document end 注入；不得将校验挪到不可信网页中。
- 下载、Cookie、通知跳转、用户保存电脑、文件选择与回调代际检查保持既有边界。业务聊天界面在根目录 `web/`，不在 Swift 中另做一套。
- 新增 Swift 文件必须进入正确的 Xcode Sources；共用 Activity 类型仍分别属于主 App 和 Widget。不要把主 App 的桥接资源加入扩展目标。
- 当前仓库以 `python3 scripts/build-mobile-ios.py` 和 `make build-ios` 作为 iOS 原生回归入口；可用 `swiftc` 单独运行 `GatewayURL.swift`/`MobileRelease.swift` 的纯 Foundation 检查。源码与未签名构建不能代表真机或签名安装通过。
