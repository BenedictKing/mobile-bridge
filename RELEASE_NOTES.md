## v0.1.0 · Mobile / Bridge 新起点

- **品牌与发布迁移**：项目由 Codex Mobile Bridge 更名为 **Mobile / Bridge**，发布仓库迁移至 [BenedictKing/mobile-bridge](https://github.com/BenedictKing/mobile-bridge)。安装包更名为 `Mobile-Bridge-*`，桌面应用名称为 **Mobile Bridge**，版本号自 0.1.0 重新起步。
- **功能完整继承 v1.4.0**：内置 cloudflared 的临时/固定 HTTPS、自有服务器 SSH 与 NAS 反代连接；账号密码与登录设备管理；多 Agent 接入与模型、Skill 目录；手机网页实时聊天、附件与通知；应用内签名更新。
- **更新签名密钥换代**：新版本线使用新的更新签名密钥。旧版（1.x）客户端无法跨仓库自动更新，请手动下载本版安装，数据目录保持兼容。
- **修复**：Windows 环境下 dsh Agent 可用性探测不再因无法确定用户目录而报错；相关测试断言改为跨平台路径形式。

**升级**：旧版用户请先停止网关并退出 App，再安装对应平台的新包（包名已改为 `Mobile-Bridge-0.1.0-*`），保留原数据目录；安装后重新加载手机页面。临时 HTTPS 地址可能因重启变化，建议开启入口变化通知。

**English**

**v0.1.0 is the first release of Mobile / Bridge.** The project is renamed from Codex Mobile Bridge and now publishes at [BenedictKing/mobile-bridge](https://github.com/BenedictKing/mobile-bridge). Packages are named `Mobile-Bridge-*`, the desktop app is **Mobile Bridge**, and the version line restarts at 0.1.0.

All capabilities from v1.4.0 carry over: bundled cloudflared for quick/fixed HTTPS, own-server SSH and NAS reverse-proxy entries, login and device management, multi-agent access with model and skill catalogs, real-time mobile chat with attachments and notifications, and signed in-app updates. The update signing key rotates with the new line: existing 1.x clients must install this release manually (data directories remain compatible); in-app updates then resume from 0.1.0.

Fixes: dsh agent availability probing no longer fails on Windows when the user home directory cannot be resolved, and the related test assertion is now path-separator agnostic. Mac packages are ad-hoc signed and not notarized; Windows packages have no certificate signature. Linux remains experimental.
