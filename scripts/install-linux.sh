#!/usr/bin/env sh
set -eu
cd -- "$(dirname -- "$0")"
mkdir -p "$HOME/.local/bin" "$HOME/.local/share/applications"
install -m 755 ./lanforge "$HOME/.local/bin/lanforge"
cat > "$HOME/.local/share/applications/lanforge.desktop" <<DESKTOP
[Desktop Entry]
Type=Application
Name=LANForge
Comment=Local game server controller and host
Exec="$HOME/.local/bin/lanforge"
Terminal=false
Categories=Game;Network;
DESKTOP
printf 'Installed. Run %s/.local/bin/lanforge to complete setup.\n' "$HOME"
