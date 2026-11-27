# Android 原生容器与局域网加速

Android App 复用现有 Mobile Bridge 网页、Cloudflare 登录和会话。普通浏览器行为不变；App 通过原生网络层在同一可信热点内加速读取，无需证书、固定 IP 或修改路由器 DNS。

## 安装

- 直接使用已发布的安装包：在 [Releases](https://github.com/BenedictKing/mobile-bridge/releases) 下载与网关同版本的 `mobile-bridge-android-debug.apk`（例如 [v0.2.0](https://github.com/BenedictKing/mobile-bridge/releases/download/v0.2.0/mobile-bridge-android-debug.apk)），校验值见同一 Release 的 `SHA256SUMS-mobile.txt`。这是 debug 签名包，仅用于自托管体验，不要分发来源不明的副本。
- 或从源码构建：在仓库根目录执行 `make android-build`（需要 Android SDK 与 JDK），产物复制到 `dist/mobile-bridge-android-debug.apk`。
- 与网关版本配套使用；覆盖安装要求签名一致，签名不同的旧包需先卸载（会清除 App 内保存的电脑与登录状态）。

## 使用

1. 电脑启动 Mobile Bridge 网关并保留局域网访问，记下已配置的 Cloudflare HTTPS 地址。需要自动发现时，在 `desktop.json` 设置 `"lanDiscovery": true`，或设置 `GATEWAY_LAN_DISCOVERY=1`；默认不广播，修改后由用户重启网关。
2. 安装 `mobile-bridge-android-debug.apk`（Release 下载或自行构建，见上节）。首次安装 debug APK 时，需要允许系统“安装未知应用”。
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

## 本设备系统通知

- 在网页“外观设置 → 通知与结果”中开启“本设备系统通知”。Android 13 及以上首次开启时需要允许通知权限；请求待确认和轮次结束沿用现有全局及单会话通知策略，不要求配置 Bark 或 ntfy。
- App 保持运行且页面处于后台时，通过 Android 原生通知提醒；点击通知打开对应聊天。冷启动仍未登录时会先保留目标聊天，登录后再打开。
- 首次轮询只建立状态基线，不补发历史；同一请求阶段和同一轮次终态不重复提醒。关闭开关后停止后续提醒并清除本应用已展示的通知。
- 该能力不依赖 FCM，但只在 App 进程保持运行时有效。系统冻结后台进程、用户强行停止或关闭 App 后不保证轮询和提醒；这些场景仍应配置 Bark、ntfy 等远程通道。本设备通知与远程通道独立，同时开启时可能分别提醒。

## 附件下载

- 下载使用 Android 系统下载器。入队时复用当前已验证 LAN 路由的地址和派生 Cookie；无可用路由时使用公网。
- LAN 路由只有在实际派生 Cookie 的 `/api/auth` 响应包含 `authenticated: true` 且网关身份一致后才可用。HTTP 200 本身不代表登录成功。地址与 Cookie 作为同一个快照发布。
- 同域可能存在旧版及多个网关命名空间的 Cookie；统一按 `/api/auth.cookieName` 选择当前网关身份，不能取第一枚。离线恢复记录仅保存 Cookie 名称，下载回退还校验原登录会话的摘要。
- LAN 任务因鉴权或传输错误最终失败后，后台完成广播触发一次公网重试。重试读取原网关的当前 Cookie，并校验登录会话未变；持久记录不保存原始凭据。
- 文件不存在、存储错误、空间不足和用户取消不触发公网重试。公网重试不会再次创建回退任务。
- 系统下载器负责同一 URL 的重试和续传。跨路由回退从头下载；排队、暂停或正在传输的任务不会立即切换 URL。同一身份刷新候选时继续使用已有验证路由；首次发现、网络或身份变化期间尚无可用路由时选择公网。

2026-11-14 验证：Pixel_9 系统下载器收到受控 401 后，自动创建回退任务并下载真实 Fridge Master APK；86,360,851 字节，SHA-256 为 `201d045b7e8c045b2edd481ea50bbdfa34eba9f6cc4a8ca2208741c657ecb3d0`，与源文件一致。

这轮回退验证使用隔离 HTTP 端点，两个任务均因模拟器网络约束而手动触发 JobScheduler。

随后完成真实网关验收：用户登录后，在正式 APK 内自动发现 `172.27.70.98:8787`，打开 Fridge Master 原聊天并点击 82.4 MB 附件。系统任务 17 的实际 URI 为该 LAN origin 下的 `/api/sessions/session_535c5599-66d0-4b5b-b0a2-0397f4af3f15/files/4cda2c195d771ec093f51c8d1092f90b22bf793075a7314bc83e89ab1a91d81a?host=local`，状态 SUCCESSFUL，字节数和 SHA-256 与上述源文件一致；无路由注入、伪造附件响应或网关重启。

设备诊断证实：原逻辑选择同域残留的旧 Cookie，根登录请求成功但派生凭据未认证；选择网关声明的命名空间后派生鉴权成功。模拟器 Wi-Fi/蜂窝均被系统标为 PARTIAL_CONNECTIVITY，真实任务仍需手动触发 JobScheduler；真实设备的自动调度、网络中断与跨路由续传不在这次通过结论内。

网络切换复查还暴露出网关 SSE 登录复查不一致：真实服务接受路由 Cookie 后返回 `ready`，随即因只检查根 Cookie 而返回 `logout`。聊天快照流、增量流和团队事件流现统一使用路由会话解析；回归测试验证有效路由先收到数据，撤销父登录后仍正确退出。此后端修复必须由用户重启网关加载，Android 更新本身不能替换运行中的网关。

用户首次重启后，真实 LAN 增量流连续 16 秒收到 `ready`、`reset` 和心跳，未再收到 `logout`。继续检查 LAN 恢复页面发现第二处凭据生命周期问题：`/api/auth` 对有效路由 Cookie 使用根 Cookie 续期器，误发 `Max-Age=0` 并将浏览器 Cookie 删除。现路由认证不再重写 Cookie，根登录仍正常续期；恢复页面下载仅允许公网 origin 或已验证 LAN origin 的附件路径，并继续携带该路由凭据。修复后需再次加载后端并复验恢复页面。

用户第二次重启后确认：路由 `/api/auth` 返回已认证且不再含删除 Cookie 的响应头；APK 自动恢复到真实 LAN 页面，多次认证和聊天订阅后 Cookie 保留、页面保持登录。恢复页点击同一真实 APK 产生任务 18，仍使用 `172.27.70.98:8787`，约 11 秒完成 86,360,851 字节，SHA-256 与源文件一致。已修正旧传输器回调覆盖 LAN 状态标签的问题。Android 33 项测试、构建、lint 通过；HTTP 层全量竞态回归及后续 Cookie 生命周期定向竞态回归通过。两次真实下载均需绕过模拟器 PARTIAL_CONNECTIVITY 的调度限制，不将其表述为实机自动调度验收。

独立测试 APK 中的 `DownloadVerification` 可接受 `url`、`initial`、`sha256` 参数复验；不包含在交付 APK 中。先构建 `:app:assembleAndroidTest` 并安装应用和测试 APK，再通过 `adb shell am instrument -w ... com.mobilebridge.app.test/com.mobilebridge.app.DownloadVerification` 运行。测试只使用隔离端点的测试凭据。

下载来源显示复验：用户反馈系统下载通知/记录显示公网域名。在上述真实聊天重测，旧包任务 19 的实际 URI 为 LAN `172.27.70.98:8787`，但 description 固定为“来自 默认网关 · bridge.lightllm.online”。现在 description 按实际目的地址显示“局域网下载 · 地址”或“公网下载 · 地址”，自动回退显示“公网重试 · 地址（局域网下载失败）”。新包任务 20 的 URI 和 description 均为 LAN，约 11 秒完整下载同一文件，字节数及 SHA-256 一致。旧下载记录不会被覆盖，需新建下载观察；手机实际线路仍须以新任务为准。模拟器网络调度限制与前述相同。

真实链路可用只读工具：`RouteDiagnostic` 接受 `origin`、`local` 参数，输出 Cookie 名称与鉴权结果（不输出凭据）；`DownloadInspection` 接受 `id`，输出任务实际 URI、状态、字节数及已完成文件的 SHA-256。

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
