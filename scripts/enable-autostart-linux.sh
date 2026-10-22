#!/usr/bin/env sh
set -eu
if [ ! -x "$HOME/.local/bin/lanforge" ]; then printf 'Run install-linux.sh first.\n'; exit 1; fi
mkdir -p "$HOME/.config/systemd/user"
cat > "$HOME/.config/systemd/user/lanforge.service" <<SERVICE
[Unit]
Description=LANForge local game hosting
After=network-online.target
[Service]
ExecStart="$HOME/.local/bin/lanforge" run --no-browser
Restart=on-failure
RestartSec=5
KillMode=process
[Install]
WantedBy=default.target
SERVICE
systemctl --user daemon-reload
systemctl --user enable --now lanforge.service
printf 'Enabled for this user. Open https://localhost:8443 on the controller.\n'
