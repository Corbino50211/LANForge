# Third-party sources

- AMP templates: https://github.com/CubeCoders/AMPTemplates — MIT license, bundled in `assets/amp/LICENSE`; pinned snapshot in `assets/amp-source.json`. Templates and their supplementary assets remain upstream work. LANForge is an independent limited adapter, not AMP.
- Playit daemon: https://github.com/playit-cloud/playit-agent — downloaded on demand from the pinned official release described by `assets/playit-release.json`, verified by its release SHA-256 digest. Game downloads and the daemon are not included in this archive.
- Minecraft metadata/server: https://piston-meta.mojang.com; Java runtime: https://adoptium.net. Game EULA acceptance is collected before Minecraft installation.
- Terraria: https://terraria.org; Valheim and applicable AMP games: Valve SteamCMD, https://developer.valvesoftware.com/wiki/SteamCMD.
- Cloudflare API: https://developers.cloudflare.com/api/; public IPv4 discovery: https://www.ipify.org.
- Go dependencies are pinned by `go.mod` and `go.sum`. Frontend dependencies are pinned by `ui/package-lock.json`. Redistributed dependency notices are in `licenses/`.
