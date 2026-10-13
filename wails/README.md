# Go/Wails 桌面壳

Go 平台桌面入口使用 Wails v3，参考 ccx 项目的 `application.New`、
`application.NewService`、`embed.FS` 和窗口创建方式。

构建：

```sh
make build-wails
```

结果位于 `dist/desktop/mobile-bridge-wails`。通过 `MOBILE_BRIDGE_GATEWAY`
和 `MOBILE_BRIDGE_DATA_DIR` 可以指定网关二进制与数据目录；默认使用
`dist/gateway/codex-mobile-gateway` 和 `.local`。
