#!/bin/bash
#
# Installs hop as a launchd daemon that starts at boot, listens on its own
# loopback address 127.0.0.2, ports 80 and 443 (so links are plain yt/ and
# https://yt while localhost stays free), and keeps /etc/hosts in sync. For
# HTTPS it trusts hop's own local CA in the System keychain. Root is needed
# for all of that, so this asks for sudo.
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
# Root-only: anyone holding a trusted CA key can impersonate any website.
readonly CA_DIR="/Library/Application Support/hop"
readonly CA_NAME="hop local CA" # caCommonName in tls.go
readonly KEYCHAIN="/Library/Keychains/System.keychain"

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
    <string>/sbin/ifconfig lo0 alias ${IP} up; exec ${BIN} serve -addr ${IP}:80 -data '${DATA}' -hosts /etc/hosts -ca-dir '${CA_DIR}'</string>
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

remove_trusted_ca() {
  while sudo security delete-certificate -c "${CA_NAME}" "${KEYCHAIN}" > /dev/null 2>&1; do :; done
}

# hop creates its CA on first start; trust exactly that one, replacing any
# CA left by an earlier install. macOS may ask to confirm the trust change.
trust_ca() {
  remove_trusted_ca
  sudo security add-trusted-cert -d -r trustRoot -k "${KEYCHAIN}" "${CA_DIR}/ca.pem"
}

# Checks the HTTPS chain against hop's CA directly, so the result does not
# depend on which trust store this curl uses.
check_https() {
  sudo cat "${CA_DIR}/ca.pem" > "${BUILD_DIR}/ca.pem"
  if ! curl -fs -o /dev/null --cacert "${BUILD_DIR}/ca.pem" --resolve "hop:443:${IP}" "https://hop/_hop/api/services"; then
    err "http://hop works, but https://hop does not. Last log lines:"
    sudo tail -n 5 "${LOG}" >&2
    exit 1
  fi
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
  trust_ca
  check_https
  echo "hop is running. Dashboard: http://hop/ (https://hop/ works too)  Logs: ${LOG}"
  echo "Add it to Claude Code with: claude mcp add -s user hop -- ${BIN} mcp"
}

uninstall_hop() {
  sudo launchctl bootout "system/${LABEL}" 2> /dev/null || true
  sudo rm -f "${PLIST}" "${BIN}"
  sudo sed -i '' '/^# hop begin/,/^# hop end/d' /etc/hosts
  sudo ifconfig lo0 -alias "${IP}" 2> /dev/null || true
  remove_trusted_ca
  sudo rm -rf "${CA_DIR}"
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
