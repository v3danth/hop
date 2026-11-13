#!/bin/bash
#
# Installs hop as a launchd daemon that starts at boot, listens on its own
# loopback address 127.0.0.2:80 (so links are plain yt/ while localhost:80
# stays free) and keeps /etc/hosts in sync. Root is needed for the address
# alias, port 80 and /etc/hosts, so this asks for sudo.
# Usage: ./install.sh            install or upgrade
#        ./install.sh uninstall  stop hop and remove its hosts entries

set -euo pipefail

readonly LABEL="dev.hop"
readonly BIN="/usr/local/bin/hop"
readonly PLIST="/Library/LaunchDaemons/${LABEL}.plist"
readonly LOG="/var/log/hop.log"
readonly DATA="${HOME}/.hop/registry.json"
# Keep in step with defaultAddr in main.go.
readonly IP="127.0.0.2"

err() {
  echo "install.sh: $*" >&2
}

# macOS only enables 127.0.0.1, and the alias is lost at reboot, so launchd
# adds it each time before starting hop.
write_plist() {
  sudo tee "${PLIST}" > /dev/null <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string><string>-c</string>
    <string>/sbin/ifconfig lo0 alias ${IP} up; exec ${BIN} serve -addr ${IP}:80 -data '${DATA}' -hosts /etc/hosts</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>${LOG}</string>
  <key>StandardErrorPath</key><string>${LOG}</string>
</dict>
</plist>
EOF
}

# launchd restarts a crashing daemon forever, so check hop really answers
# before reporting success.
wait_until_serving() {
  local i
  for i in {1..20}; do
    if curl -fs -o /dev/null "http://${IP}/_hop/api/services"; then
      return 0
    fi
    sleep 0.25
  done
  err "hop did not start. Last log lines:"
  sudo tail -n 5 "${LOG}" >&2
  exit 1
}

install_hop() {
  # Global, not local: the EXIT trap runs after this function has returned.
  BUILD_DIR="$(mktemp -d)"
  trap 'rm -rf "${BUILD_DIR}"' EXIT
  (cd "$(dirname "$0")" && go build -o "${BUILD_DIR}/hop" .)
  sudo mkdir -p "$(dirname "${BIN}")"
  sudo install -m 0755 "${BUILD_DIR}/hop" "${BIN}"
  # The daemon runs as root but writes files owned like this directory, so
  # create it as the user. Earlier versions left it owned by root.
  mkdir -p "$(dirname "${DATA}")"
  sudo chown -R "$(id -u):$(id -g)" "$(dirname "${DATA}")"
  sudo launchctl bootout "system/${LABEL}" 2> /dev/null || true
  write_plist
  sudo launchctl bootstrap system "${PLIST}"
  wait_until_serving
  echo "hop is running. Dashboard: http://hop/  Logs: ${LOG}"
  echo "Add it to Claude Code with: claude mcp add -s user hop -- ${BIN} mcp"
}

uninstall_hop() {
  sudo launchctl bootout "system/${LABEL}" 2> /dev/null || true
  sudo rm -f "${PLIST}" "${BIN}"
  sudo sed -i '' '/^# hop begin/,/^# hop end/d' /etc/hosts
  sudo ifconfig lo0 -alias "${IP}" 2> /dev/null || true
  sudo dscacheutil -flushcache
  sudo killall -HUP mDNSResponder || true
  echo "hop removed. Your shortcuts are kept in ${DATA}."
}

main() {
  case "${1:-install}" in
    install) install_hop ;;
    uninstall) uninstall_hop ;;
    *)
      err "unknown command: $1 (use install or uninstall)"
      exit 2
      ;;
  esac
}

main "$@"
