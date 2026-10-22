<p align="center">
  <img src="site/assets/icon.png" alt="Mobile / Bridge" width="112" height="112">
</p>

# Mobile / Bridge

[简体中文](README.md) · [English](README_EN.md)

**[Product tour and walkthrough ↗](https://BenedictKing.github.io/mobile-bridge/?lang=en)** · **[Download the desktop App](https://github.com/BenedictKing/mobile-bridge/releases)**

[![Desktop App: connections and status (demonstration data)](site/assets/desktop-overview.png)](https://BenedictKing.github.io/mobile-bridge/?lang=en)

Continue **existing chats in the desktop Codex App** from your phone browser, or create an empty chat in a saved project.

The phone and desktop use the same chat. Read replies, send messages, choose models and Skills, and respond to pending confirmations. Local tasks continue on the original computer; SSH tasks continue on the original server. Each chat keeps its model provider and authentication settings.

**Your phone does not need to sign in to the same OpenAI account as the desktop.** The gateway has its own username/password login and an explicit passwordless option. Connect over LAN, a temporary HTTPS tunnel, or your own reverse proxy.

> Community project, not affiliated with OpenAI. Supports macOS and Windows and depends on internal Codex App IPC. See [verification records](VERIFICATION.md) for what was actually tested. App updates may require compatibility changes.

Experimental Linux x64 / ARM64 integration, Ubuntu 22.04 builds, and VMware networking are covered in the [Linux guide](docs/linux.md). Both .deb and AppImage packages are available. Native CI checks installation, startup and gateway behavior; IPC compatibility with a particular Codex desktop version still requires real-device testing.

## Edit, branch and copy messages

- Copy user messages from their action row. Choose **Edit and resend** on the latest user message, or **Edit in new branch** on an older message to keep the original conversation.
- Choose **Branch from here** under a completed answer. The new chat keeps history through that turn and waits for your next message. Chat details link back to the source.
- Copy complete replies as Markdown or copy an individual code block. Long replies load their full text. When clipboard access is unavailable, selectable text supports the system Copy action.
- In-place editing requires the turn to be stopped and generates a new answer. Editing and branching do not undo file changes; both chats share the working directory.
- Branching requires a Codex runtime with turn-specific forking and deferred goal continuation; older versions show an update prompt. Copying and latest-message editing do not require those fork capabilities. Creating a branch does not send a model request.
- If delivery is uncertain, check the chat list first; repeating the same request does not replay it. If a branch was created but could not connect, the replacement remains an edit draft for that branch.

## Download and quick start (recommended)

Download the desktop App for everyday use. **No deployment Agent, Python, Node.js or terminal is required.** The current release is **v0.1.0 (stable)**.

| Platform | Download | Open |
| --- | --- | --- |
| Windows x64 (recommended installer) | [Download Setup.exe](https://github.com/BenedictKing/mobile-bridge/releases/download/v0.1.0/Mobile-Bridge-0.1.0-Windows-x64-Setup.exe) | Run the installer and launch from the shortcut |
| Windows x64 (no installation) | [Download full ZIP](https://github.com/BenedictKing/mobile-bridge/releases/download/v0.1.0/Mobile-Bridge-0.1.0-Windows-x64.zip) | Extract the entire ZIP and run `Mobile Bridge.exe`; do not move just the exe |
| macOS Apple Silicon (M series) | [Download arm64 DMG](https://github.com/BenedictKing/mobile-bridge/releases/download/v0.1.0/Mobile-Bridge-0.1.0-macOS-arm64.dmg) | Open the DMG and drag the App to Applications |
| macOS Intel | [Download x64 DMG](https://github.com/BenedictKing/mobile-bridge/releases/download/v0.1.0/Mobile-Bridge-0.1.0-macOS-x64.dmg) | Open the DMG and drag the App to Applications |
| Ubuntu x64 (experimental) | [Download .deb](https://github.com/BenedictKing/mobile-bridge/releases/download/v0.1.0/Mobile-Bridge-0.1.0-Linux-amd64.deb) | Ubuntu 22.04; launch as a regular user |
| Ubuntu ARM64 (experimental) | [Download .deb](https://github.com/BenedictKing/mobile-bridge/releases/download/v0.1.0/Mobile-Bridge-0.1.0-Linux-arm64.deb) | Ubuntu 22.04 ARM64; no 32-bit ARM build |

[All releases and release notes](https://github.com/BenedictKing/mobile-bridge/releases) · [Download SHA256 checksums](https://github.com/BenedictKing/mobile-bridge/releases/download/v0.1.0/SHA256SUMS.txt). Both Mac architectures also include ZIPs for in-app updates or manual replacement. Windows ARM packages are not available. The Mac build is ad-hoc signed but not notarized and may require manual approval; Windows builds have no certificate signature. See the macOS first-launch instructions below.

1. Open the original **Codex App** on the computer, then open **Mobile Bridge**.
2. Keep LAN access enabled under **Network and login**, save, and click **Start gateway**. Preserve the existing port if you already have a configuration.
3. Connect the phone to the same LAN. Expand **Scan to sign in** below its address in the App and scan with the phone camera to sign in without typing a password. Alternatively, open the address manually and use the login credentials provided by the App.
4. Select an existing chat or create a new one in the phone browser. Keep the computer awake with Codex App and the gateway running.

For access outside your LAN, add temporary HTTPS, an own-server connection or a NAS connection in the App. Desktop builds bundle `cloudflared`. Server profiles connect over SSH after you manually prepare the server; a fixed Cloudflare tunnel also supports private domains without a server. See [Parallel connections](#parallel-connections). For source deployment, custom networking or help from an Agent, see the optional [Deployment Agent instructions](#deployment-agent-instructions). The gateway keeps probing Cloudflare tunnel health and rebuilds a connector left half-dead by sleep/wake or a network change; a rebuilt Quick Tunnel gets a new URL and sends an entry-change notification.

### In-app updates

**Releases from the old Codex Mobile Bridge repository (1.x / beta) cannot update in-app to this repository's releases** (the release repository and signing key have changed). Stop the gateway and quit the App, then manually install a package from this page to the same location, preserving the data directory. In-app updates work from this version onward.

Starting with `v0.1.0`, use **App updates** to check for a release, read its notes, and select **Update and restart**. The app verifies the signed manifest and package before replacing itself, preserves login, network, notification and watched-chat settings, and restores a previously running gateway. Installation failures attempt to restore the previous app. Temporary HTTPS addresses change when the gateway restarts.

**Windows users of old versions:** the old updater may roll back with `WinError 32` because its working directory is locked. Choose **Stop gateway and quit** in the tray, then install this page's Setup.exe to the same location. Do not uninstall or delete your data. The current updater has fixed this working-directory issue.

### Phone reading and display settings

- Compact chat navigation leaves more room for replies. Tap the title for the full name, project, device and model details.
- Use the arrow beside Model, Notifications and Skill to hide or show the composer and send/stop controls. Drafts are retained; a successful send does not collapse the composer automatically, and a failed send reopens it to show the error.
- Open **“··· → Display settings”** to independently toggle **Reasoning summaries** and **Execution activity**. Activity includes commands, tools, file changes and intermediate progress updates. Turn both off to focus on replies; user messages, errors and pending requests remain visible. Older replies without phase metadata are also retained.
- Choose light, dark or system theme, an accent color, text and code sizes, and reading spacing. Settings apply immediately and are saved in the current browser; they also work on desktop browsers.

### Multiple accounts and API connections

v1.3.0 lets you add official accounts and custom APIs or import local configurations in the Bridge desktop app, then switch saved connections on desktop or Web. Switching restarts the official Codex desktop app while keeping the Bridge gateway running. The active connection stays first; official accounts show quota and reset credits, while API chats list models from their upstream. See [usage and recovery](docs/account-switching.md).

The desktop app can scan the current or a specified Codex data directory and import saved official credentials, API providers and profiles. API forms can fetch the provider's model list for selection, with manual model entry still available.

### Native account limits and usage resets

After starting the updated gateway, **Account and usage** appears in the desktop panel and phone chat list only for native ChatGPT sign-in on this computer. For API keys and custom providers, the phone shows non-clickable “API connection” text; signed-out accounts have a separate status. The page reads this status automatically on entry and retries temporary failures. These are this computer's account limits, independent of the selected SSH chat.

View remaining percentages, reset times and available usage resets. Missing information is shown as unavailable. Using a reset requires the Codex desktop usage-reset permission and confirmation for each operation. Interrupted requests retain their original redemption ID for retry. The bundled runtime manages account credentials; they are never sent to the phone.

### Login validity and device access

- Web sign-in defaults to **Remember me (7 days)**. It keeps a login cookie, not the plaintext password. Uncheck it to use a browser-session cookie.
- Five consecutive password failures automatically block the source IP. The page shows remaining attempts; counts and blocks survive restarts. Successful authentication resets the count. Unblock and reset attempts only under **Login devices → Automatically blocked IPs** in the desktop app.
- **Phone notifications → Login security notifications** is enabled by default and uses enabled Bark and ntfy channels. Disabling alerts does not disable blocking.
- Set standard login validity under **Network and login → Login validity (hours)** to an integer from 0 to 87600, then restart the gateway. The default is 12 hours; 0 sets no server expiry. A finite deadline starts at login and does not slide with activity.
- Normal gateway restarts and App updates preserve unexpired logins. Changing the account, password, login mode or validity invalidates earlier logins on restart. Logout, clearing browser cookies or changing hostnames requires signing in again; browsers may also remove long-unused cookies.
- **Signed-in devices** shows browser login records, IPs, browser identifiers, login/expiry times and last activity (updated at most once a minute). **Revoke login** removes one login; **Block this IP** revokes every login at that IP and prevents new logins.
- The optional allowlist accepts only listed IPs; the blocklist takes priority. Enter exact IPv4 or IPv6 addresses, one per line. Rules apply immediately and can always be changed in the local desktop App.
- These are browser logins, not immutable hardware identities. Web pages cannot read phone MAC addresses, and phones may use rotating private MAC addresses. IPs can change, and devices sharing a public IP share its access rules.
- LAN connections use the peer IP. Local HTTPS tunnels are trusted automatically. For NAS or other reverse proxies, configure **Trusted proxy IPs**, and have each proxy append or overwrite `X-Forwarded-For`. Headers from untrusted peers are ignored. Missing forwarded addresses are labelled as proxy IPs; blocking a proxy IP affects all its clients.
- `auth-sessions.json` contains private session records (only hashes of bearer tokens) and IP rules. Keep it private and preserve it during updates.

### First launch on macOS: “damaged” or unidentified developer

The Mac build has an **ad-hoc integrity signature**, but **no Apple Developer ID signature or notarization**. macOS can still block the first launch.

1. Download the DMG or ZIP for your Mac's chip and `SHA256SUMS.txt` from this repository's Release. Calculate the downloaded file's hash and compare it with the matching filename in the checksum file. If they differ, download again instead of allowing the app. The example below uses the 0.1.0 Apple Silicon DMG; replace the filename for other downloads.

   ```sh
   shasum -a 256 "$HOME/Downloads/Mobile-Bridge-0.1.0-macOS-arm64.dmg"
   ```

2. Open the DMG, drag `Mobile Bridge.app` to Applications, then eject the disk image. For a ZIP, extract it and move the App instead. Try opening it from Applications, then go to **System Settings → Privacy & Security → Open Anyway** and confirm.
3. If Open Anyway is unavailable, or macOS still reports the app as damaged, only after verifying the source and hash, remove the download quarantine attribute from this specific app and open it again:

   ```sh
   xattr -dr com.apple.quarantine "/Applications/Mobile Bridge.app"
   ```

   Substitute the actual path if installed elsewhere. This does not confer Apple trust or notarization. Do not disable Gatekeeper, SIP or global security checks.

An ad-hoc signature checks bundle integrity; it does not identify the publisher or mean Apple has checked the app. Only allow downloads you trust. See [Apple: Safely open apps on your Mac](https://support.apple.com/102445).

## Desktop App guide

`main` includes the integrated macOS and Windows version. Use a Release package above for everyday use. The desktop launcher manages the gateway; the original Codex App still owns execution and model authentication.

### Install and use the App

Download links are in the quick start above. Development artifacts are available from successful **GitHub Actions → Desktop builds** runs, or you can build locally as described below.

On Windows, closing the window hides it to the tray; launching again restores it. The tray offers separate actions to stop the gateway and quit, or quit only the controller. Stop the gateway and exit before manually upgrading or moving the app. Select Chinese or English at the top right; existing Windows language preferences are retained, and tray labels follow the selection. Phone language is independent. Uninstalling does not automatically remove gateway settings or credentials.

The App provides:

- **Overview:** start/stop the gateway, copy/open phone URLs, expand QR sign-in, and locate initial login credentials.
- **Network and login:** the LAN port, independently enabled connection profiles, extra HTTPS addresses, username/password or explicit passwordless access.
- **Runtime settings:** automatic gateway startup, gateway data directory and Cloudflare component status. Custom paths, IPC overrides and repair installation are collapsed under advanced settings.
- **Phone notifications:** independent Bark and ntfy settings, test buttons and delivery status, with a shared click URL and title privacy option.
- **App updates:** automatic release checks, signed downloads, update and restart, and recovery status.
- **Runtime logs:** newest records first within each source; stack traces within one error keep their original order. Refresh returns to the newest records.

If you already use the command-line gateway, choose its existing `.local` directory under Runtime settings. Stop the gateway before changing network settings or executable paths. The listening port never changes automatically. Closing the launcher leaves the gateway running; **Stop** ends phone access. Login changes apply on the next gateway start; notification changes are read while it runs.

Unsaved changes appear as red dots in the affected sidebar section and save bar. Switching pages or languages preserves edits. Saving successfully, or reverting to the original values, clears the indicators.

Desktop builds bundle a pinned cloudflared binary for the target OS and architecture. No first-run download is needed. Runtime settings keep custom executables and source-run installation under **Advanced: custom program and repair**.

The desktop sidebar links to this project's GitHub home, issues, and pull requests for source code, feedback, and contributions.

### QR sign-in

1. Start the gateway and find a reachable phone address on the overview page.
2. Expand **Scan to sign in** below that address. Each LAN, ready temporary HTTPS or configured fixed HTTPS entry has its own collapsible area; multiple areas can be expanded together. Loopback addresses such as `127.0.0.1` have no phone QR code.
3. Scan with the phone camera and open the link in a browser to sign in without typing a password. The App checks that the entry points to this running gateway before generating a code.
4. Each QR code expires after **5 minutes** and works **once**. Collapsing or refreshing revokes it; restarting the gateway invalidates all codes. Browser logins use the configured validity (12 hours by default; 0 disables automatic expiry), independently of QR expiry, and survive gateway restarts.

Treat the QR code as a short-lived login credential and keep screenshots private. It does not contain your username/password, disable password protection or create a network tunnel. Images are generated locally. Opening the ordinary URL still uses the configured login method. LAN reachability or a working public HTTPS entry is required; verify camera scanning and connectivity on your own phone.

### Parallel connections

Under **Network and login → Additional connections**, choose a type and click **Add connection**. Name, enable, disable or delete each profile independently. LAN access works independently of these profiles.

| Connection | When to use it | Configuration |
| --- | --- | --- |
| LAN | Phone and computer share a reachable network | Enable LAN access and keep the existing port |
| Temporary Cloudflare HTTPS | No domain or existing public entry | Add a connection, save, and start using the bundled program |
| Fixed Cloudflare tunnel | Private domain, no server | Configure Cloudflare DNS and a public hostname; enter the domain and tunnel token |
| Own server + SSH | You have a Linux public server and domain; the computer is behind NAT or on a campus network | Manually prepare HTTPS and forwarding permissions, then enter a regular SSH account, check sign-in and connect |
| NAS / existing proxy / Docker | The NAS can reach the computer and already has an HTTPS reverse proxy | Enter the fixed HTTPS URL and the computer's HTTP address as reachable from the NAS |

LAN, a temporary Cloudflare tunnel and multiple fixed entries can run together. Only one temporary tunnel is needed per gateway. Profiles targeting the same SSH server need different loopback ports. Different SSH aliases pointing to the same server can still conflict; choose their ports accordingly.

All entries reach the same gateway and share the same gateway login. Saved single-entry settings from earlier versions are converted when read; existing files are not rewritten until you save.

Server profiles offer **Export manual setup reference**, **Copy manual setup instructions**, and **Check fixed entry**. NAS profiles retain their deployment ZIP and Agent instructions. The ZIP contains concrete configuration and Chinese/English deployment instructions, without passwords, notification tokens, Codex data or SSH private keys. Clipboard instructions use the current UI language.

The username `try2love` and URL `https://codex.try2love.com` are configuration examples, not a live demo site. Replace them with your own settings.

#### Private domain without a server

Choose **Fixed domain · Cloudflare Tunnel**. Use Cloudflare DNS for the domain (keep your registrar), create a tunnel, and publish a hostname such as `codex.try2love.com` pointing to `http://localhost:8787`. Save the hostname and tunnel token in the App, start the gateway, then check the fixed entry. The token runs the existing tunnel; it does not create DNS records. No public server or inbound computer port is needed. Use a dedicated hostname, not a `/codex/` path. Keep the computer, Codex App and gateway online.

See the [complete fixed-domain walkthrough (Chinese)](docs/fixed-domain.md), or expand the first-time setup guide in the Cloudflare connection card. The guide covers nameservers, the tunnel token, the hostname route and verification over phone mobile data.

#### Own server

```text
Phone → server HTTPS → server loopback port → SSH → computer gateway → original Codex App
```

The computer initiates SSH, so the server does not need direct access to the computer's private IP. SSH forwarding starts/stops with the gateway and retries after disconnection. New profiles support password, private key with optional passphrase, SSH agent, and existing SSH configuration. Confirm the host fingerprint in the App; changed keys are rejected. Legacy OpenSSH aliases remain supported. Passwords and tunnel tokens can be stored in the native OS credential store or held only for this app session, never in ordinary configuration or exported packages.

The server must allow remote forwarding and keep the listening socket on loopback (`GatewayPorts no` or `clientspecified`, not `yes`). For the generated Caddy configuration, DNS must point to the server and public TCP 80/443 must be reachable and free. Caddy obtains and renews certificates. Its generated Docker service uses host networking on Linux Docker Engine, on the same host as SSH; it is not a Mac/Windows Docker Desktop deployment.

Everyday SSH forwarding can use a regular user such as `try2love`, without root or sudo. Follow the [server SSH walkthrough (Chinese)](docs/server-ssh.md): manually prepare DNS, HTTPS and forwarding permissions; save the app settings and check SSH sign-in; start the connection and check the fixed entry, then verify over phone mobile data.

Server software installation, firewall rules and SSH permissions must be configured manually by you or your administrator. The app does not accept sudo passwords or run remote installation commands. Preserve existing sites and use their reverse proxy. SSH sign-in success does not verify forwarding or public HTTPS access. Export or copy setup references for manual use; any administrative commands must be run by the user.

If an existing proxy already owns 80/443, keep it. Use the upstream printed in the deployment instructions and preserve the public Host header instead of starting another Caddy instance.

#### NAS and Docker

Docker hosts the access entry. The original computer still needs to stay awake with Codex App and the gateway running. If the NAS already has an HTTPS proxy that can reach the computer, direct forwarding is usually enough. The optional Nginx container is an HTTP intermediary behind the NAS's existing HTTPS termination.

A domain alone does not make separate private networks reachable. A home NAS cannot directly reach an isolated campus computer; use a working VPN route or the own-server SSH option. See [NAS deployment instructions](deploy/nas/README.md) and the English instructions in an exported ZIP.

#### Verification

**Check fixed entry** verifies that the HTTPS endpoint returns the identity of this exact running gateway. It checks from the computer only: also disable phone Wi-Fi and test login and chat over mobile data.

The overview lists all enabled fixed URLs. With the notification click URL empty, notifications prefer the first enabled fixed URL, then another HTTPS entry, then LAN. Enter an explicit notification URL if you want a specific entry. Domain/server costs are determined by the services you choose; this project does not purchase or provision resources.

### Choose which adapters allow access

In the desktop App, open **Network & login**, choose **Selected IPv4 addresses only**, and select the addresses you want to use. Exclude unwanted WSL or VMware adapters. Stop the gateway, save, and restart; unselected addresses do not listen on the gateway port. Selections bind to current IPs: reselect after a DHCP address change. The gateway does not fall back to opening all adapters. **All IPv4 addresses** retains the previous behavior.

**Allow local browser access** independently controls `127.0.0.1 / localhost`. Turning it off hides local URLs and blocks local browser/API access while keeping a private loopback path for desktop status checks and HTTPS tunnels. Turning off LAN access does not disable configured external entries.

### What are “Additional HTTPS addresses”?

This is the gateway's **address allowlist**. Add a URL only when its HTTPS reverse proxy or tunnel is already configured elsewhere and you need another domain to reach the gateway. Enter one origin per line, such as `https://codex.try2love.com`, without a path.

Adding a URL does **not** create a tunnel, configure DNS or obtain a certificate. The proxy must reach the gateway and preserve the public Host header. Fixed URLs in connection profiles are added automatically. Leave this field empty when using only LAN or temporary Cloudflare.

### Chinese and English UI

The desktop App and phone website each have a **Language / 语言** selector and remember their own choice. Switching language does not restart the gateway or translate chat content, commands, model/Skill descriptions, user input or raw logs. Unsaved settings, message drafts and pending-confirmation form input are preserved. Language switching is disabled while a confirmation is being submitted.

### Phone notifications: Bark and ntfy

1. **Bark for iPhone:** install Bark and allow notifications. In the desktop App, enter the server URL and Device Key from the phone app. For `https://api.day.app/YOUR_KEY`, use `https://api.day.app` as the server and only `YOUR_KEY` as the key. Self-hosted servers are supported; register your phone with that server first. Saved keys are hidden, blank keeps the key, and changing the server requires re-entering it. See the [Bark guide](https://bark.day.app/#/tutorial).
2. **ntfy for Android:** install ntfy and allow notifications, lock-screen display and background activity. For an initial test, use `https://ntfy.sh`. Generate a random topic in the desktop App and subscribe to exactly that server and topic on the phone. Public anonymous topics need no token and are created automatically. Anyone who knows such a topic can read and publish; use a long random name and leave chat titles hidden. Consider a protected topic for regular use.
3. Enable the channels you need, save, then click **Send Bark test notification** or **Send ntfy test notification**. Either channel or both can receive the same watched-chat notifications. Confirm actual receipt on the phone. This test does not require the gateway to run; server acceptance alone is not proof of phone delivery.
4. Start the gateway, then open **Settings → Notifications for all chats** on the phone website and choose the request-handling and run-completion defaults. Fresh configurations default to request notifications on and completion notifications off; at least one notification channel must be configured and enabled first.
5. New command, file, permission or question requests trigger a notification. Clicking it opens that chat using the normal web login and confirmation flow.
6. In each chat's **Reminders**, both notification types can independently **follow the global setting, turn on, or turn off**; explicit choices on watched chats survive upgrades. Completion notifications start from the boundary of the first live connection: failed, stopped and already completed historical runs do not trigger it.
7. In the desktop App, **Phone notifications → Watched chats** shows each chat's name, device, directory and ID. Toggle completion notifications or **Remove watch**; changes save immediately. Removing a watch stops that chat's reminders without deleting the chat. Add new watches from the phone. Saved watches remain manageable when the gateway is stopped or notification channels are disabled. Remote names use the last available title; the device and chat ID remain visible when the name is unavailable.

Watched chats remain monitored after the phone page closes, provided the computer, gateway, original App and relevant SSH connections stay online. Delivery and retries are tracked separately per channel, destination, chat and event, including after restart. Failure in one channel does not affect the other. New channels or destinations do not replay completed history; runs observed in progress on their first live connection and future completions can notify. Existing ntfy configuration and delivery records are preserved. Failed delivery retries with backoff and rechecks whether the request is still pending. Completion retries continue while that chat has the option enabled and stop when it is disabled. Completed history from before the first live connection is not replayed. Strict exactly-once delivery is not guaranteed during network failures.

An explicitly configured notification click URL takes priority over automatic selection. Old notifications retain their old URLs after a temporary domain changes. Self-hosted ntfy needs an APNs upstream for instant iPhone delivery; Android delivery also depends on background/battery permissions. See the official [phone subscription guide](https://docs.ntfy.sh/subscribe/phone/) and [iOS instant notification setup](https://docs.ntfy.sh/config/#ios-instant-notifications).

### Gateway startup and entry notifications

In the desktop App, open **Phone notifications → Gateway startup and entry notifications** and enable sending addresses on every gateway start and entry change. Optionally name the gateway. Enable at least one Bark or ntfy channel, save, start the gateway and send a current-entry test. Confirm receipt on your phone.

New configurations enable address notifications by default; saved opt-outs survive upgrades. Configure at least one notification channel for delivery. Every gateway start sends the enabled addresses, even when unchanged: LAN, NAS / existing reverse-proxy domains, personal servers once SSH forwarding connects, and temporary HTTPS once ready. Later-ready entries and address changes trigger an update. Disabled interfaces, disabled entries and loopback addresses are excluded. LAN links require the same network; fixed domains require completed deployment.

The switch is independent of chat notifications. Messages use current entries rather than the chat click URL, contain no passwords or login tokens, and retry per channel. Stopping the gateway or disabling the switch stops new sends. Configure and test after enabling before relying on notifications for subsequent restarts.

## Features

| Feature | Details |
| --- | --- |
| Existing App chats | Read history, live replies and tool output |
| New chats | Create an empty chat in a saved local/SSH project, then hand it to the desktop App; creation itself does not call a model |
| Markdown and math | Headings, lists, tables, code and LaTeX math; assets and fonts are served locally |
| Progressive history | Show the latest 20 records first, silently fill to 100, then load 100 more near the top; expand large tool content on demand |
| Original execution | Send, steer, queue; disable queueing per chat, send queued messages now, edit or delete them; stop and respond to supported confirmation cards |
| Local and SSH | Execute on the chat's original computer or server |
| Chat navigation | Sort by recent activity or group by expandable projects |
| Model and Skills | Choose model/reasoning effort, toggle Fast for eligible official accounts, or send Skills: Codex reads installed skills from the chat's host (up to 8 per message); Kimi lists the skills the agent advertises, one per message |
| ACP account pools | Per-agent tabs for Kimi / ZCode / MiniMax / WorkBuddy: add or re-login accounts in the panel (device flow / QR / OAuth), capture the current local login, delete snapshots, view window quotas and switch manually; when a window limit (such as Kimi's 5-hour window) runs out, switch to an available account and resume the interrupted session (ZCode spends a reset card first); can be disabled per agent; Codex is shown read-only |
| Plan and Goal modes | Start a plan or native goal from the composer; implement or revise a completed plan in the browser |
| Independent authentication | Username/password or explicit passwordless mode |
| Parallel access | LAN, temporary HTTPS and multiple fixed entries |
| File previews | Local chat references inside the working directory, up to 50 MiB per file |
| Bilingual UI | Chinese / English on the desktop and phone |

Large history pages also have a byte budget, so a large reply may require multiple pages. Incremental SSE updates changed records on LAN entries without retransmitting the entire chat; entries that cannot carry SSE (such as temporary Cloudflare tunnels) fall back to incremental long polling. Stale cursors after a gateway restart or replaced desktop snapshot trigger a fresh synchronization. Full model context stays on the computer.

## Requirements

- macOS or Windows 10/11 (Linux x64 / ARM64 is experimental, see [docs/linux.md](docs/linux.md)) with the original Codex App running.
- Release packages include the gateway runtime; no Python or Node.js installation is needed.
- For command-line deployment: Go (single Go binary built by `make build-gateway-package`); the ACP multi-agent npx fallback needs Node.js 22+.
- Desktop App development and packaging additionally require Node.js and the build dependencies below.
- Keep the computer awake, network reachable and gateway running.
- For SSH chats: the host is configured and connected in the desktop App; the gateway reaches remote chats through the App's own connection and does not run scripts on the server. Model/Skill discovery also needs the remote Codex runtime. SSH entry profiles (own-server forwarding) need OpenSSH `ssh` on PATH (`ssh.exe` on Windows).
- Desktop builds include cloudflared; source runs install it separately.

## Command-line LAN setup (advanced)

Skip this section if you downloaded the desktop App. These commands are for running the gateway from source.

### Windows PowerShell

```powershell
git clone https://github.com/BenedictKing/mobile-bridge.git
cd mobile-bridge
go build -trimpath -ldflags="-s -w" -o dist\gateway\codex-mobile-gateway.exe .\cmd\gateway
.\dist\gateway\codex-mobile-gateway.exe serve --data-dir ".local"
```

You can also double-click `start.cmd` (it runs `dist\gateway\codex-mobile-gateway.exe` and prompts to build when missing). Keep the terminal open. Stop with `Ctrl+C` or `stop.cmd`.

The gateway reads `%USERPROFILE%\.codex` or `CODEX_HOME` and uses the local `\\.\pipe\codex-ipc` named pipe. Run it as the same Windows user as the App. `--codex-home` changes the data directory, not the pipe name. `--ipc-path` is a local endpoint override, not a way to connect to another computer.

Runtime discovery includes `%LOCALAPPDATA%\OpenAI\Codex\bin`, common installation directories, the current user's MSIX package and PATH. For custom installations:

```powershell
$env:GATEWAY_CODEX_BIN = 'C:\path\to\codex.exe' before serve
```

If Windows Firewall prompts, allow only the network scope you need. Files use UTF-8. Restrict access with Windows directory ACLs; POSIX chmod does not replace them.

### macOS

```sh
git clone https://github.com/BenedictKing/mobile-bridge.git
cd mobile-bridge
make serve          # foreground (builds dist/gateway/codex-mobile-gateway)
```

For an always-on production setup use launchd:

```sh
make dev-cert       # one-time local signing cert (stable TCC identity across rebuilds)
make start          # build, install to ~/.local/bin/, sign, run gateway + ACP Agent Host under launchd
make status         # service state + health probe
make logs           # follow ~/Library/Logs/mobile-bridge-gateway.log
make stop           # stop and unload the gateway and Agent Host
```

Open the printed LAN URL on a phone on the same network, for example `http://192.168.1.10:8787`. The initial username is `admin`; the generated password is in `.local/首次登录.txt`. Select a chat and wait for **Connected** before sending.

By default the gateway listens on `127.0.0.1` plus every non-link-local address of each active interface. There is no `--lan` flag: the listen scope comes from `lan` (`false` = `127.0.0.1` only), `lanAddresses` (listen only on these) and `localAccess` (`false` = no loopback) in the data directory's `desktop.json`, which the desktop App's network settings edit. `serve` accepts only `--data-dir`, `--port`, `--codex-home`, `--ipc-path` and `--codex-bin`; any other flag exits with an error. `make serve` installs no system service; only `make start` registers launchd jobs. Normal gateway restarts preserve unexpired logins. If a chat shows saved history only, open it in the desktop App and reconnect on the phone; the gateway does not create a replacement execution owner for an unloaded chat.

### Multiple agents (source runs)

The default `auto` probes for locally installed agents (Codex is always enabled; Kimi Code CLI, MiniMax mcode, Claude Code, DeepSeek Harness dsh, ZCode, OpenCode and WorkBuddy codebuddy are enabled when a local install is detected — an npx download fallback alone does not count). You can also declare them explicitly with the `GATEWAY_AGENTS` environment variable, comma-separated:

```sh
GATEWAY_AGENTS=codex,kimi,mcode,claude,dsh,zcode,opencode,workbuddy make serve
```

The corresponding CLI (`kimi` / `mcode` / `npx`; Claude is reached through the `@agentclientprotocol/claude-agent-acp` wrapper, overridable via the `CLAUDE_ACP_BIN` environment variable; DeepSeek through the `@openma/deepseek-harness-acp` wrapper, overridable via `DSH_ACP_BIN`; ZCode through the `zcode-acp-server` wrapper, overridable via `ZCODE_ACP_BIN`, credentials read from `~/.zcode`, Node.js 22+ required; OpenCode ships ACP natively and is reached through its `opencode acp` subcommand, overridable via `OPENCODE_BIN`, sign in first with `opencode auth login`; WorkBuddy through `codebuddy --acp`, overridable via `CODEBUDDY_BIN`) must be installed on PATH and signed in (for dsh use `dsh-acp login` or the `DEEPSEEK_API_KEY` environment variable; for ZCode and WorkBuddy sign in via the desktop app or CLI). The gateway spawns these agents as child processes; the phone chat list labels each session with its agent, and listing, history, sending, approvals, interrupt, queueing and notifications behave the same across agents. Unsupported capabilities (goals, steer, skills, Fast mode, …) are hidden per agent; see the capability matrix in the "多 Agent 适配" section of `ARCHITECTURE.md`. When creating a chat, the agent and the project are picked independently; projects pointing at the same directory are deduplicated across agents, and a local project directory known to one agent can be handed to another agent for the new session.

Kimi, ZCode, MiniMax and WorkBuddy also have account pools, shown as per-agent tabs in the web account dialog (only the active tab is loaded):

- **Add account / re-login** inside the panel (device flow for Kimi/MiniMax, QR for WorkBuddy, web OAuth for ZCode); zcode-switcher is no longer required.
- **Capture current login** reads the local CLI's signed-in state: new accounts are imported, existing ones get refreshed credentials. A WorkBuddy desktop slot in encrypted-envelope state prompts a QR re-login to obtain plaintext credentials.
- **Delete** removes only the pooled snapshot, never the local login.
- **Quotas**: the active account is polled every 5 minutes (`GATEWAY_POOL_POLL_SECONDS`) and the whole pool is swept hourly (`GATEWAY_POOL_POLL_ALL_SECONDS`); negative values disable either.
- **Auto-switch**: when a window limit runs out, the gateway switches to an available pooled account and resumes the interrupted session (can be disabled per agent). ZCode first redeems a reset card if one is available and only switches when none is. Auxiliary quotas (such as ZCode MCP usage) are ignored. Kimi refreshes the token before switching and refuses dead credential chains, marking them for re-login.

Codex accounts are shown read-only; switching them still uses the existing account panel. The old "Import from zcode-switcher" button is gone; `/api/pool/{kind}/import` remains for scripts (source directory from `GATEWAY_ZCAS_DIR`).

Note: Kimi/MiniMax/Claude/DeepSeek/ZCode/OpenCode/WorkBuddy runs execute inside the gateway process by default, so stopping the gateway interrupts their in-flight turns for good (history is preserved). After a gateway restart, sessions with notification watches re-attach automatically in the background; unwatched sessions reconnect when you open the chat. Codex runs are owned by the desktop App and survive gateway restarts.

**Agent Host (optional, macOS):** `make start` also runs a standalone ACP Agent Host under launchd (Kimi by default; use `ACP_HOST_KIND=<kind> make start-agent-host` for another agent), reached over a Unix socket (`GATEWAY_ACP_HOST_SOCKETS`). The Host owns the agent child processes, so gateway restarts or crashes no longer interrupt in-flight turns, and its stable signing identity keeps macOS folder permissions across gateway rebuilds. Agents without a Host are still spawned inside the gateway; both modes can be mixed. See `ARCHITECTURE.md`.

The chat details dialog can migrate the current session to another enabled agent: a new session is created on the same directory, with the title and a transcript of recent messages injected as its first message; the source session stays unchanged. If a local source directory is missing from the target agent, the gateway creates the target session directly from that directory. A remote source directory cannot be created on the local target, so choose an explicit target project.

The packaged desktop App launches the same Go gateway, so without an environment variable it also probes installed agents (`auto`). To restrict the set, inject `GATEWAY_AGENTS` (or `MOBILE_BRIDGE_AGENTS`; the former wins) before launching it: on macOS run `launchctl setenv MOBILE_BRIDGE_AGENTS codex,kimi` and then open the App, or start it from a terminal with `MOBILE_BRIDGE_AGENTS=codex,kimi open -a …`. The gateway has no `--agents` flag.

## Temporary HTTPS and existing proxies

**Desktop App:** add a Temporary HTTPS · Cloudflare connection, save and start. The matching program is bundled. For source runs or repair, expand **Runtime settings → Cloudflare component → Advanced: custom program and repair** to find Download and install. The App downloads the matching official GitHub Release, verifies its SHA-256 digest, installs it in the gateway data directory and checks `--version`. It needs no admin privileges, does not change system PATH and does not start a tunnel during installation.

The detected path is filled in as an unsaved change. Save settings, then start the gateway from the overview. Temporary HTTPS has a separate status; its URL and login QR code appear when connected. An existing executable can be selected and checked instead. Supported installer targets: macOS arm64 / x64 and Windows x64 / x86. Other architectures use the linked official guide. Downloads time out after 3 minutes; failed verification, missing digests or network failures leave existing binaries and settings unchanged. The App includes manual steps and an official download link. If a running gateway’s tunnel fails, LAN access remains available; see the Cloudflare section in Runtime logs.

**Source / CLI:** Install cloudflared from the [official download instructions](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/downloads/). Then, using the same configuration and port as your existing gateway:

```sh
enable the tunnel connection in `.local/config.json` (or the desktop App), then `make serve`
```

On Windows, put `cloudflared.exe` in `.local\bin\` or PATH. Quick Tunnel URLs are temporary, require no purchased domain and may change after restart. The computer must be able to make outbound connections to the tunnel service. The initial tunnel handshake runs in the background so LAN access can become ready independently.

For an already configured HTTPS reverse proxy:

```sh
add the origin to `.local/config.json`'s `origins` array, then `make serve`
```

Preserve the external Host header, Origin, cookies and CSRF header; disable proxy caching/buffering and allow long requests. Use a dedicated hostname root, not `/codex/`. The proxy must reach the computer. TLS termination and DNS are configured on your own infrastructure. The App's profile export is the guided route for own-server SSH and NAS deployments.

## Phone operations

- **New chat:** choose a saved desktop project and name. Creation stores an empty chat through the official runtime, closes the helper and opens it in the original App. Sending waits for desktop ownership. Creation may change the desktop's visible chat. Saved request IDs prevent blind duplicate creation after an uncertain result. For a local Git project, check **Run in a new worktree** to create the chat in an isolated worktree under `<project>/.agents/worktrees/` with a new branch: `fresh` bases on the latest remote default branch, `head` on the current local HEAD (including unpushed commits).
- **Model/Skills:** settings follow the current chat's host and project. Custom model availability depends on the provider. Changing a model does not change authentication or permission policy. Codex chats accept up to eight Skills sent as native skill input with the next message; Kimi chats list the skills the agent advertises, one per message, sent as a slash command.
- **Fast mode:** the model panel shows a toggle when the chat's host uses official ChatGPT login and its model and workspace allow Fast. Toggle it and select **Apply to this chat**; changes take effect from the next turn. Turning it off explicitly selects standard speed. The panel reads desktop settings and retains them after a refresh. Leaving the toggle untouched preserves the existing speed tier, including when changing model or reasoning effort. API/custom providers and unsupported models hide it; SSH chats use their remote account and workspace. Fast uses more allowance; see the [official speed guide](https://learn.chatgpt.com/docs/agent-configuration/speed).
- **Long model names:** the toolbar truncates the name while keeping reasoning effort visible. Open the model panel to read the full ID, or hover over the button on a computer.
- **Work mode:** choose Default, Plan or Goal between the send-type selector and Send. Plan mode discusses the task first; its completion card lets you view the plan, implement it in Default mode, or send requested changes in Plan mode. Entering revision feedback emphasizes further planning and disables immediate implementation, so feedback cannot be silently ignored.
- **Goal mode:** enter an objective of up to 4,000 characters after the current turn ends. The bridge sets the goal through the local native Goal API, then starts execution through the original desktop chat owner. The browser displays the confirmed objective, status and token usage; uncertain operations keep their request identity and are not automatically replayed. An unfinished goal or unresolved goal request prevents another start.
- **Goal panel:** use × to hide it while the goal keeps running. Select **Goal progress** beside Send to restore it; narrow screens use a target icon. The same goal stays hidden after refreshing the current browser tab, and new goals are shown automatically.
- **Attachments:** select the paperclip to the left of the send-type selector to add multiple files or images. Up to 10 files per message, 1 byte–20 MiB each and 100 MiB total. Uploads can be removed from the draft or retried individually; drafts stay with their chat and host. Send, queue and steer retain attachments, including after a failed send. Default messages can contain attachments without text. PNG, JPEG, GIF and WebP use native image inputs; other files supply original file paths, subject to model/tool capabilities and chat permissions. Remote chats upload through their configured SSH connection. Uploading does not start a model turn.
- **Chat indicators:** green means running, blue means completed and unread, amber means failed or stopped. Entering the chat clears its completion marker; a running marker stays. A previously running chat with unavailable live state turns gray. Disable markers under **Display settings → Chat list**. Preferences and read state are stored in this browser. Existing completed history is not marked unread on first use; loaded list entries refresh about every five seconds while the page is visible. Phone push notifications use their separate watch settings.
- **Send modes:** new message, queue after the active task, or steer the current task. Queued messages can be sent immediately when idle, edited, or deleted before submission.
- **Confirmations:** respond to supported command, file, permission and question requests. Unsupported requests should be handled in the desktop App.
- **Unknown result:** inspect chat history before trying again. The gateway does not automatically replay an uncertain submission.

Work mode follows the chat unless manually selected. Default and Plan messages can be queued; steering retains the running turn's mode. Modes preserve the original host, model, provider and permissions. After a goal request is accepted, subsequent messages return to ordinary input without cancelling the goal. Local chats support pausing, resuming, editing and closing goals from the browser; these controls are not yet supported for SSH chats. Pause before editing. Saving replaces the objective and resets usage while retaining the token budget and paused state; resume to continue. Pausing or closing a goal does not interrupt an active reply; use Stop for that. Manage budgets with existing Codex controls. These features require desktop runtime support; see [verification scope](VERIFICATION.md).

### Chat notifications and renaming

- **Settings → Notifications for all chats** sets the request-handling and run-completion defaults; each chat's **Reminders** can override them independently. Notifications continue after the page is closed while the gateway keeps running. Bark and ntfy can be used together.
- Click a chat title, choose **Rename chat**, and save a name of up to 120 characters. The name is saved to Codex on the chat's execution host, including SSH hosts, and the web list and heading update.

## Login settings

The default is independent username/password authentication. To change the password from the CLI:

```sh
stop the gateway, write a new PBKDF2-SHA256 `salt`/`hash` into `.local/config.json`'s `auth` (or use the desktop App)
```

Passwords must contain at least 12 characters. Restart the gateway for login changes to take effect. Passwordless access is explicit through the App or `auth.mode: "none"` in `.local/config.json`; anyone who can reach that entry can then operate connected chats. Keep authentication for public access.

## Architecture and stored data

The bridge reads desktop chat metadata/history and forwards actions through the App's original IPC owner. It does not need a matching mobile OpenAI login. Explicit account management stores and switches credentials locally; it never sends them to the phone. Empty-chat creation uses a short-lived official app-server helper and then hands ownership to the App. See [architecture](ARCHITECTURE.md).

Gateway data stays in its configured directory, normally `.local`. It includes login hashes, desktop preferences, notification settings/tokens, watched chats, submission/creation deduplication records, caches, process control records and logs. Each SSH connection has separate status/log files. Do not publish this directory or `.tmp`. Keep backups private and use the App's directory selector to adopt an existing installation.

Uploaded files are retained in `uploads/` or `hosts/<host hash>/uploads/` inside gateway data, separated by chat. Remote copies are stored under that host's `$CODEX_HOME/mobile-bridge/uploads/` (default `~/.codex/mobile-bridge/uploads/`). Removing a draft attachment does not delete uploaded files. Local uploads older than seven days that are no longer referenced by any send record are cleaned up automatically; remote copies are not cleaned up. Manually deleting a file can break historical or queued references to it.

The Go desktop management interface uses Wails v3 bindings and the private gateway action protocol. Gateway start/stop and network configuration are not exposed through the phone HTTP API. Notification settings are managed in the Wails app. Public HTTP access remains constrained by authentication, allowed Host/Origin checks and CSRF validation.

## Known limits

- Internal Codex App IPC is version-sensitive. Revalidate after App updates.
- The computer, original App and relevant SSH owner must remain available. A NAS container alone cannot replace the desktop App.
- macOS and Windows have automated checks; real GUI, phone delivery and server deployments have separate verification scopes.
- File preview is limited to supported local chat references. Complex approval types may need the desktop.
- Fixed-domain HTTPS requires working DNS, server/NAS routing and TLS configuration. Exported configuration is not proof of a deployed public service.
- Never treat a model/Skill list lookup, server acceptance of ntfy, or a successful package build as proof of end-to-end operation.

## Deployment Agent instructions

An Agent is optional for everyday App use. For source deployment, custom networking or Agent-assisted troubleshooting, see [docs/deployment-agent.md](docs/deployment-agent.md): it contains a copy-pasteable instruction for an Agent on the computer, plus environment inspection, startup, acceptance, diagnosis and handoff templates (full version in Chinese, with an English summary).

## Development and packaging

The product icon source is `assets/icon.png`. The Go/Wails desktop build uses the icon assets directly.

The product website lives in `site/` and is hosted on GitHub Pages. Pushing changes under `site/` to `main` automatically publishes them through the **Product website** workflow; no separate build step is required.

This section requires Go and the native WebView dependencies used by Wails on the target OS. Release users do not need Python or Node.js.

On macOS, build the Go gateway and Wails shell with `make build-gateway-package` and `make build-wails` for the current architecture. Cross-compile gateway binaries with `make build-matrix`; Wails packaging must run on the target OS because it links to the native WebView.

```sh
git switch main
make build-gateway-package
make build-wails
```

```sh
go test ./...
go test -race ./internal/...
make conformance
```

Report the OS, App/runtime version, branch/commit and redacted reproduction steps when opening an issue. Include logs without credentials or private chat content. Current verification boundaries are recorded in [VERIFICATION.md](VERIFICATION.md).

## Acknowledgements

Thanks to the [LINUX DO](https://linux.do/) community and its members for their support.

Thanks to [@qybgh (Luoran Yau)](https://github.com/qybgh) for contributing mobile Plan and Goal support, attachment previews and UI improvements in [PR #8](https://github.com/try2love/codex-mobile-bridge/pull/8). Version 1.3.1 builds on that contribution with Goal controls, notification preferences and interaction refinements.

## License and references

[MIT License](LICENSE).

- [OpenAI Codex](https://github.com/openai/codex)
- [Cloudflare Tunnel documentation](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/)
- [ntfy documentation](https://docs.ntfy.sh/)
- [Docker Compose documentation](https://docs.docker.com/compose/)
- [Caddy documentation](https://caddyserver.com/docs/)
- [Nginx proxy module](https://nginx.org/en/docs/http/ngx_http_proxy_module.html)

