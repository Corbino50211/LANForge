# LANForge 0.3.3 — local hosting preview

A local controller and host application for Windows and Linux. Run the controller on your main PC and pair your laptops. The same executable runs either role. The controller also hosts games itself.

## Start here

**Windows x64:** extract the complete download, open `windows/Start-LANForge.cmd`, and follow the browser setup. Keep the application running. An optional per-user installation is provided by `Install-LANForge.ps1`; add `-AutoStart` to start at sign-in. No administrator account is needed for LANForge itself. The executable is unsigned and has not been tested on a physical Windows machine.

**Linux x64:** open a terminal in `linux`, run `chmod +x lanforge`, then `./lanforge`. Optional: `sh install-linux.sh` installs a menu entry and user binary. Complete setup before running `sh enable-autostart-linux.sh` (requires systemd). No root is needed. SteamCMD games may require your distribution's 32-bit C runtime libraries; installation errors explain this dependency.

1. On your main PC choose **Controller**, give it a name, and set a password.
2. Open `https://localhost:8443`. LANForge uses a locally generated TLS certificate, so the browser initially warns that it is self-signed. Continue only for your own controller address. Agents verify the certificate fingerprint during pairing.
3. In **Computers**, choose **Add computer**. Keep the displayed LAN controller URL, one-time code and fingerprint handy.
4. Run LANForge on each laptop, choose **Host**, and enter those pairing details. Agents connect outward to the controller; they need no inbound management port.
5. Choose **Create server**, select a supported game and computer, set its options and choose LAN, Playit, or direct forwarding. Minecraft requires accepting its EULA in the wizard.
6. Watch installation progress in the console, then start the server. Copy its join address from the dashboard.

Windows firewall may request access: allow your private/home network. Permit TCP 8443 on the controller from your LAN only; reserve its LAN address in your router. Do not forward the dashboard to the internet. Permit each game's listening ports on its host as needed. Keep laptops awake while hosting.

## Public access

### Playit.gg

Select **Playit** during server creation. Under **Computers**, connect Playit on the computer hosting that server and approve the one-time link in your Playit account. LANForge then downloads the checksum-verified official Playit daemon and requests the game's supported tunnels. This requires internet access and a Playit account; limits and premium requirements come from Playit. If automatic creation is rejected, the UI shows the service error and lets you retry or configure the tunnel in Playit's dashboard. Playit uses a local socket or Windows named pipe, not another management TCP port.

### Direct forwarding and a domain

Forward the displayed game ports in your router to the selected host. LANForge does not modify the router. Under **Settings**, enter a Cloudflare zone ID and a narrowly scoped API token with Zone Read and DNS Edit permissions for that zone. In the server's **Connect** tab, enter a subdomain. LANForge creates DNS-only A records and Minecraft Java SRV records, and updates a discovered public IPv4 address every five minutes. You can supply a fixed public IPv4 instead. Existing unrelated DNS records are not overwritten.

DNS does not replace forwarding or bypass CGNAT. Other DNS providers, IPv6, and automatic router configuration are not implemented in this release. Most games need `hostname:port`; Minecraft Java can use its SRV record.

## What's implemented

- Password-protected local HTTPS dashboard, SQLite storage, certificate-pinned outbound host connections and expiring single-use pairing codes.
- Real process installation/start/stop/restart, console input and output, CPU/RAM/disk readings, automatic start and limited crash recovery.
- Text file editing and uploads, stopped-server backups and staged restore with the previous game directory retained.
- Official Minecraft Java, Terraria and Valheim installers. Java runtimes install privately beside each Minecraft server.
- Minecraft loaders: Vanilla, Paper, Purpur, Fabric, Quilt, Forge and NeoForge. Pick one in the wizard or in Settings, then use **Install / repair**. Paper and Purpur run Spigot/Bukkit plugins.
- Steam game icons in the library, from Steam's store.
- A Modrinth browser in the create wizard for mods, plugins and modpacks, plus a **Mods** (or **Plugins**) tab that searches Modrinth, installs only compatible versions and pulls in required dependencies. Set **Modrinth modpack** to a modpack link or slug to install a whole pack. No API keys are needed; CurseForge is not supported.
- **Custom app** servers for Node.js, Python, Go, Java, Bun and Deno. Code comes from a public GitHub repository, an uploaded zip (click it in Files to extract) or files you write in Files. LANForge downloads the runtime, installs dependencies (`npm install`, `pip install -r requirements.txt`, `go build`), sets `PORT` and runs the start command.
- AMP catalog browsing and refresh from CubeCoders' public MIT-licensed repository. The bundled snapshot contains 244 upstream templates plus LANForge's Minecraft entry.
- A generic AMP adapter covering SteamCMD, FetchURL, FetchURLFromJQ, GithubRelease, ExtractArchive, CreateDirectory/File, CopyFilePath, Executable and pre-start stages, plus INI/KVP/JSON/template configuration with AMP line formats. In the bundled snapshot 133 templates install on Windows and 100 on Linux. They are experimental and have not been tested in-game. Templates that need a Steam account login, XML configuration or git are still marked unavailable.
- Automatic updates from GitHub Releases (see below). The pairing dialog now shows the PC's real LAN address instead of VirtualBox/Hyper-V adapters.
- Playit account claim, official agent management, tunnel discovery and supported tunnel creation; Cloudflare DNS configuration and public IPv4 updates.

## Preview limitations

This is an early working implementation, not full AMP compatibility. Arbitrary AMP scripts, proprietary AMP runtime modules, XML config mapping, GitRepo installers and Windows-only builds on Linux (Wine) are marked unsupported. Catalog visibility does not mean a game can be installed. Scheduled backups, game player counts and multi-user roles are not implemented. Change game ports or connection mode by creating a new server; do not overwrite existing worlds.

Settings edits require a stopped server. Generic AMP configuration is edited in Files. File uploads are limited to 2 GiB each and text editing to 2 MiB. Backups include the game directory and need enough space for an additional copy; take backups before upgrading. Repair may update official game files.

The controller can restart without intentionally stopping its detached local host. Hosts reconnect after network interruption. After an entire host-agent process crashes, running game processes are recovered where their PID and creation time match; restart a recovered game to restore console input. Graceful shutdown of a recovered process is platform-dependent. Windows autostart runs at user sign-in, not as a Windows service before login. Linux user services also normally start at login; unattended boot setup is an OS administration task.

Playit and Cloudflare code is implemented but has not been exercised with a real account in this build. Browser interaction and Windows runtime behavior remain unverified. See `docs/TESTING.md` for the exact validation performed.

## Automatic updates

LANForge checks the latest GitHub release of `Corbino50211/LANForge` (set by `UpdateRepo` in `updater.go`; the repository must be public) one minute after start and every six hours. A release tagged `vX.Y.Z` must contain `lanforge-windows-amd64.exe`, `lanforge-linux-amd64` and `SHA256SUMS.txt` (the build scripts produce all three in `dist/`). The download is verified against the checksum, swapped in, and the app restarts itself. Hosts wait until no game is running, installing or being backed up before restarting. The controller restarts right away because games run on its separate host process. **Settings → LANForge updates** checks right away and offers **Update to vX.Y.Z**; each computer card has **Update now**. Restarted processes log to `lanforge.log` in the data folder.

## Steam sign-in

Some games download only with a Steam account that owns them. Open **Computers**, choose **Steam sign-in** on the computer that will host the game, and enter your username, password and (if asked) the Steam Guard code, or leave the code blank and approve the sign-in in the Steam app. The password is used once and is not stored. SteamCMD keeps its own login token in that computer's data folder (`tools/steamcmd`). Sign in on each host separately. If installs later fail with a login error, sign in again.

## Your data

Default data location: `%APPDATA%\LANForge` on Windows; `$XDG_CONFIG_HOME/LANForge` or `~/.config/LANForge` on Linux. The controller's own game host uses its `local-host` subfolder. Remote hosts keep their games locally. Use `--data /absolute/path` to select another data folder. Do not share the data folder: it contains agent credentials, TLS keys and account secrets. Cloudflare tokens are encrypted with a key stored locally; filesystem account permissions still matter.

Stopping the controller does not stop games. Stop games in the dashboard before shutting down a host or removing the app. Disable Windows Startup shortcuts or run `systemctl --user disable --now lanforge` to disable autostart. Keep data directories when replacing binaries; stop app and host processes before replacement.

## Build from source

Install Go 1.24+ and Node.js 20.19+ with npm. Run `sh scripts/build.sh`, or `powershell -File scripts/build.ps1` on Windows. Prebuilt frontend assets are included, so `go build -buildvcs=false .` is sufficient if you do not change the UI. All third-party downloads require internet access. The dashboard, state, server files and control traffic stay local; external services are contacted for downloads, selected public access and DNS updates.

CLI: `lanforge run [--data PATH] [--no-browser]`, `lanforge host --data PATH`, `lanforge version`. For headless controller initialization, set `LANFORGE_PASSWORD` and run `lanforge init --name "Main PC" --port 8443`, then unset the variable and run `lanforge run --no-browser`.

LANForge is independent of CubeCoders, Playit, Mojang, Re-Logic, Iron Gate and Cloudflare. Game and service terms apply separately. See `THIRD-PARTY.md` and the included licenses.
