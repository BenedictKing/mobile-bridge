# Linux Desktop Support (Experimental)

v1.3.0 adds experimental Linux integration and native x64/ARM64 packages.
Native CI validates package installation, GUI startup and isolated gateway behavior.
Compatibility with a particular Codex desktop version still requires real-device IPC testing.

## Scope

| Target | Build baseline | Packages | Current validation |
| --- | --- | --- | --- |
| Linux x64 / amd64 | Ubuntu 22.04 x64 | amd64.deb, x86_64.AppImage | Native CI installation and isolated GUI checks |
| Linux ARM64 / aarch64 | Ubuntu 22.04 ARM64 | .deb, AppImage | Native CI installation and isolated GUI checks; hardware IPC testing pending |

ARM means ARM64 here, not 32-bit ARM. Windows ARM64 is outside this change.
Other Linux distributions may run the AppImage, but are not yet validated.
The desktop App itself must work on the target OS and expose compatible IPC;
packaging the bridge does not establish compatibility with every App version.

User-supplied diagnostics from an Ubuntu 22.04 x64 VM confirmed this layout:

```text
Installed package: chatgpt 26.930.31730
/usr/bin/chatgpt -> /usr/lib/chatgpt/codex-launcher
Bundled runtime: /usr/lib/chatgpt/resources/codex
codex:// handler: chatgpt.desktop
```

Runtime discovery resolves desktop launchers, checks their bundled resources,
then common installation directories and finally a PATH CLI. Nonstandard
installations can set the runtime path explicitly in Advanced settings or with
`--codex-bin`. Discovery does not launch the desktop or send model requests.
Linux uses `xdg-open` for desktop links and `ip -j -4 address show up` for IPv4
enumeration; the existing hostname fallback remains available if `ip` fails.

## Build on the Target Architecture

Use Ubuntu 22.04 or a compatible Go/Wails build environment and build the
gateway and Wails shell for the same architecture. Wails links to the native
WebKit/GTK libraries on the target system.

```sh
make build-gateway-package
make build-wails
```

On an x64 host:

```sh
GOOS=linux GOARCH=amd64 make build-gateway-package
```

On an ARM64 host:

```sh
GOOS=linux GOARCH=arm64 make build-gateway-package
```

The gateway artifact is `dist/gateway/codex-mobile-gateway`; the Wails shell is
`dist/desktop/mobile-bridge-wails`. Package these two files with the native
distribution tooling for the target Linux architecture.

## Install and Launch

After obtaining a verified package for your architecture, compare its SHA-256
with the release checksums. Run the bridge as the same ordinary desktop user
that runs ChatGPT/Codex, never with `sudo`.

For a .deb, replace the example filename with the actual downloaded package:

```sh
sudo apt install ./Mobile-Bridge-VERSION-Linux-amd64.deb
mobile-bridge
```

The .deb declares its desktop libraries, `xdg-utils`, and `iproute2`. For
AppImage, grant execute permission to that file and run it as your user. If
the environment cannot mount AppImages, try its `--appimage-extract-and-run`
option or prefer the .deb. Do not disable Chromium's sandbox or run the GUI
as root to work around startup failures.

Keep the Codex desktop App running. The bridge retains the shared Chinese and
English interface and stores preferences under the Wails data directory, not
under `/opt`. Closing the control window leaves the gateway running. Reopen the
Wails app to manage it.

In-app updates are disabled for Linux. Stop the gateway and exit the controller
before installing a newer .deb or replacing the AppImage. Keep the user data
directory. Do not use the macOS/Windows directory-swap updater for Linux.

## Source Mode and Desktop IPC

Source mode uses the Go gateway:

```sh
./dist/gateway/codex-mobile-gateway
```

Run this in the logged-in desktop session. A headless SSH session may lack the
display/session bus needed by `xdg-open`. The default Unix socket is
`${CODEX_HOME:-$HOME/.codex}/ipc/ipc.sock`; set `--ipc-path` or the Advanced
setting if the installed App uses another endpoint. Verify the socket instead
of assuming its location:

```sh
ls -l "${CODEX_HOME:-$HOME/.codex}/ipc/"
xdg-mime query default x-scheme-handler/codex
/usr/lib/chatgpt/resources/codex --version
```

Do not expose the desktop Unix socket over TCP. The HTTP gateway provides its
own authentication; LAN and tunnel options do not disable login protection.

## VMware and Phone Access

`127.0.0.1` always means the device opening the URL, not the VM. Select a
reachable VM interface in Network settings and save before starting the
gateway. Keep authentication enabled.

- Bridged networking can give the VM a LAN address reachable by the phone.
- VMware NAT commonly gives the VM a private VMnet address reachable by the
  host but not directly by other LAN devices. It may need explicitly configured
  forwarding, bridged networking, or a tunnel.
- The desktop can install the official `cloudflared-linux-amd64` or
  `cloudflared-linux-arm64` binary. HTTPS source validation and SHA-256 checking
  occur before execution; the binary stays in the gateway's private data area.
- Check Ubuntu and host firewall rules for the chosen path. Do not disable
  firewalls or expose ports automatically. A reachable interface address alone
  does not prove that a phone can connect.

## Validation

The Linux jobs in `.github/workflows/desktop.yml` use independent native x64
and ARM64 runners on Ubuntu 22.04. They build both runtimes, install the .deb,
verify architecture/version metadata, launch the real packaged GUI under a
session bus and Xvfb, and exercise authentication, notifications, QR login,
TLS, language persistence, and closing/reopening the controller. The AppImage
payload is extracted and launched separately; this does not verify a real
desktop's FUSE mounting or Wayland behavior. Screenshots are retained as CI
artifacts. These jobs passed on the Ubuntu integration branch; each release reruns the same gates.

For a local installed package, use Node.js 24 in the source checkout:

```sh
node scripts/smoke-linux.cjs /usr/bin/mobile-bridge
```

The smoke check uses a private temporary data directory and a nonexistent
desktop socket, so it never opens or sends messages to real chats. It does not
prove compatibility with a particular desktop App's internal IPC.

Before promoting Linux support, validate actual IPC connection, read-only
session listing, desktop links, and an explicitly approved test conversation
on the Ubuntu VM and on ARM64. Also check native Wayland/XWayland, restart,
package upgrades, and phone access using the chosen VMware networking mode.
