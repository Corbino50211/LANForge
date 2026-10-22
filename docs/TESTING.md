# Validation for 0.1.0

Completed in the Linux build environment:

- Go automated tests with the race detector: path confinement, malicious ZIP/tar entry rejection, internal archive link handling, SQLite persistence and encrypted secrets, all catalog parsing, supported plan compilation, configuration preservation, password/session access, cross-origin write rejection, TLS fingerprint mismatch, pairing-code replay rejection, real TLS/WebSocket host reconnection and command transport, actual subprocess console/start/stop lifecycle, duplicate start handling, backup and staged restore, occupied-port detection, settings preserving installed executable paths, AMP configuration mapping and argument parsing.
- React/TypeScript production build (`npm run build`).
- Windows x64 cross-compilation and Linux x64 native compilation.
- Packaged Linux executable smoke test: actual HTTPS dashboard HTML and JavaScript/CSS assets, password login, the full catalog response and the detached local host heartbeat. Run `python3 tests/binary-smoke.py /path/to/lanforge` to repeat it on Linux.
- Live official Terraria download, extraction, world generation, readiness detection, TCP connection to its game port and console shutdown. This was a transport test, not a game-client multiplayer session.
- Live Temurin Java 21 download, checksum verification, safe extraction of internal license-file links and execution of `java -version`.

Not verified: actual Windows execution/console signaling/autostart, graphical browser interactions, a complete Minecraft or Valheim gameplay session, generic AMP game installation, a real multi-device home LAN, Playit account/tunnel creation, Cloudflare DNS changes, external player reachability, router/firewall configuration or operating-system suspend/resume. These need validation on the user's machines and accounts. No private credentials or worlds are included in this package.
