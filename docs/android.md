# Android 原生容器与局域网加速

Android App 复用现有 Mobile Bridge 网页、Cloudflare 登录和会话。普通浏览器行为不变；App 通过原生网络层在同一可信热点内加速读取，无需证书、固定 IP 或修改路由器 DNS。

## 使用

1. 电脑启动 Mobile Bridge 网关并保留局域网访问，记下已配置的 Cloudflare HTTPS 地址。需要自动发现时，在 `desktop.json` 设置 `"lanDiscovery": true`，或设置 `GATEWAY_LAN_DISCOVERY=1`；默认不广播，修改后由用户重启网关。
2. 构建并安装 `dist/mobile-bridge-android-debug.apk`。首次安装 debug APK 时，需要允许系统“安装未知应用”。
3. 打开 App，输入不带路径的 Cloudflare 地址，例如 `https://codex.example.com`，点击“连接”并正常登录。
4. 登录后，标题栏右上角的电脑图标就是网关入口：查看当前地址、修改网关地址、重新连接。地址只在首次连接或改地址时需要填写，不再藏进设置页。
5. App 保存稳定 `DeviceID` 和最多 8 个最近 LAN origin，并浏览 `_mobile-bridge._tcp`。公网域名失败时，历史地址与 DNS-SD 会并行恢复同一网关；热点阻断组播但地址未变时仍可直连，地址已变且组播也不可用时会有界失败，不扫描网段。
6. 切换热点或外出后无需重新登录。App 会重新探测当前地址，无法直连时自动回退公网。

只在自己或可信任的热点中启用。局域网直连使用 HTTP。mDNS/缓存候选先发送不含 Cookie 的 ownership proof，通过后才允许一次根 Cookie `/api/auth` 引导；后续业务请求只携带该 origin 的派生 route Cookie。HTTP 无法防止同一网络中的主动代理或窃听，不可信网络请使用浏览器公网访问，或另行配置受信任的 HTTPS 直连。

## 请求边界

- GET/HEAD、图片和 SSE 可安全重放，因此优先尝试局域网，建连失败回退 Cloudflare。
- 正常公网页面中的发送消息、审批和其他 POST/PATCH/DELETE 始终使用 Cloudflare，不在两条链路之间重放写请求。公网失败并恢复到 LAN shell 后，页面在同源 LAN origin 上工作，写请求也提交到已验证的 LAN；已发出的公网写请求不会自动重放。
- 公网 `/api/auth` 只向已登录且带原生标识的请求下发当前候选地址。App 不扫描网段，不推断其他设备。
- 公网 Cookie 只在 App 内复用；局域网响应的 Set-Cookie 不会覆盖公网 Cookie。
- 公网 TLS 始终使用系统证书校验，没有忽略证书错误。
- Android 新版本可能要求“局域网”运行时权限，请按系统提示允许；拒绝后仍可使用 Cloudflare。

## 返回手势

系统返回手势和返回键按 WebView 的历史栈处理：聊天页、团队会话和 Run 页各压一条历史，返回一次回到会话列表，列表页再返回才退出 App。网页侧因此不再只用 `replaceState` 换屏（那会让历史栈永远只有一条，`webView.canGoBack()` 恒为 false，返回手势会直接退出 App）；压栈只在手机宽度（≤720px）生效，桌面和平板的双栏布局仍按原来的方式只替换 hash。深链（通知或转发链接落在某个聊天、`#team=`、`#run=`）没有可退的列表条目，此时页内返回按钮就地关闭面板，系统返回直接退出。

## 构建

需要 JDK 17、Android SDK Platform 36 和对应构建工具。设置 `ANDROID_HOME` 后，在仓库根目录运行：

```bash
make android-build
```

该命令执行 JVM 测试与 debug APK 构建，输出：

```text
dist/mobile-bridge-android-debug.apk
```

也可单独运行：

```bash
cd android
./gradlew :app:testDebugUnitTest :app:assembleDebug
```

debug APK 使用 Android 默认调试签名，适合自有设备测试；正式发布前应另行配置发布签名、版本号和发布流水线。
