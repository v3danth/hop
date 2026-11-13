#!/bin/bash
#
# Installs hop as a system service that starts at boot: launchd on macOS,
# systemd on Linux. hop listens on its own loopback address, 127.0.0.2,
# ports 80 and 443, so links are plain yt/ and https://yt while localhost
# stays free for other apps. It keeps /etc/hosts in sync and serves HTTPS
# with its own local CA, which this script adds to the trusted certificates.
# Root is needed for all of that, so this asks for sudo.
#
# Usage: ./install.sh            install or upgrade
#        ./install.sh uninstall  remove hop, its hosts entries and its CA;
#                                your shortcuts are kept for a reinstall

set -euo pipefail

readonly BIN="/usr/local/bin/hop"
readonly IP="127.0.0.2"           # defaultIP in main.go
readonly CA_NAME="hop local CA"   # caCommonName in tls.go
readonly OLD_DATA="${HOME}/.hop"  # where hop kept its registry before November 2026

OS="$(uname -s)"
readonly OS
case "${OS}" in
  Darwin)
    readonly PLATFORM="macos"
    # Root-only, because it holds the CA key: anyone with that key can
    # impersonate any website to this machine.
    readonly STATE_DIR="/Library/Application Support/hop"
    readonly LABEL="dev.hop"
    readonly PLIST="/Library/LaunchDaemons/${LABEL}.plist"
    readonly LOG="/var/log/hop.log"
    readonly LOGS_HINT="${LOG}"
    readonly KEYCHAIN="/Library/Keychains/System.keychain"
    ;;
  Linux)
    readonly PLATFORM="linux"
    readonly STATE_DIR="/var/lib/hop"
    readonly UNIT="/etc/systemd/system/hop.service"
    readonly LOGS_HINT="journalctl -u hop"
    ;;
  *)
    echo "install.sh: hop runs on macOS and Linux, not ${OS}" >&2
    exit 1
    ;;
esac

err() {
  echo "install.sh: $*" >&2
}

require() {
  local cmd
  for cmd in "$@"; do
    if ! command -v "${cmd}" > /dev/null; then
      err "needs ${cmd}, which is not installed"
      exit 1
    fi
  done
}

have_any() {
  local cmd
  for cmd in "$@"; do
    if command -v "${cmd}" > /dev/null; then
      return 0
    fi
  done
  return 1
}

#######################################
# macOS: launchd and the System keychain
#######################################

macos_stop_service() {
  sudo launchctl bootout "system/${LABEL}" 2> /dev/null || true
}

# macOS only enables 127.0.0.1, and the alias is lost at reboot, so launchd
# adds it each time before starting hop.
macos_start_service() {
  sudo tee "${PLIST}" > /dev/null << EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>${LABEL}</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/sh</string><string>-c</string>
    <string>/sbin/ifconfig lo0 alias ${IP} up; exec ${BIN} serve -addr ${IP}:80 -hosts /etc/hosts -state '${STATE_DIR}'</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>${LOG}</string>
  <key>StandardErrorPath</key><string>${LOG}</string>
</dict>
</plist>
EOF
  sudo launchctl bootstrap system "${PLIST}"
}

macos_remove_service() {
  macos_stop_service
  sudo rm -f "${PLIST}"
  sudo ifconfig lo0 -alias "${IP}" 2> /dev/null || true
}

macos_logs() {
  sudo tail -n 5 "${LOG}" >&2
}

macos_untrust_ca() {
  while sudo security delete-certificate -c "${CA_NAME}" "${KEYCHAIN}" > /dev/null 2>&1; do :; done
}

# Trusts the CA in $1 system-wide. macOS may ask to confirm the change.
macos_trust_ca() {
  macos_untrust_ca
  sudo security add-trusted-cert -d -r trustRoot -k "${KEYCHAIN}" "$1"
}

macos_flush_dns() {
  sudo dscacheutil -flushcache
  sudo killall -HUP mDNSResponder || true
}

#######################################
# Linux: systemd, the system CA store and NSS
#######################################

linux_stop_service() {
  sudo systemctl stop hop 2> /dev/null || true
}

# hop writes only /etc/hosts and its state directory, so the unit keeps it
# out of home directories and makes /usr read-only.
linux_start_service() {
  sudo tee "${UNIT}" > /dev/null << EOF
[Unit]
Description=hop: short names for websites and local apps
Documentation=https://github.com/v3danth/hop

[Service]
ExecStart=${BIN} serve -addr ${IP}:80 -hosts /etc/hosts -state ${STATE_DIR}
StateDirectory=hop
StateDirectoryMode=0700
Restart=always
RestartSec=1
ProtectHome=yes
ProtectSystem=true
PrivateTmp=yes

[Install]
WantedBy=multi-user.target
EOF
  sudo systemctl daemon-reload
  sudo systemctl enable --quiet hop
  sudo systemctl restart hop
}

linux_remove_service() {
  sudo systemctl disable --now --quiet hop 2> /dev/null || true
  sudo rm -f "${UNIT}"
  sudo systemctl daemon-reload
}

linux_logs() {
  sudo journalctl -u hop -n 5 --no-pager >&2
}

# Distros keep extra trust anchors in different places, each with a tool
# that rebuilds the system bundle (the same list mkcert uses). Sets ANCHOR
# and REFRESH, or fails when the layout is unknown.
linux_find_trust_store() {
  if [[ -d /etc/pki/ca-trust/source/anchors ]]; then # Fedora, RHEL
    ANCHOR="/etc/pki/ca-trust/source/anchors/hop-local-ca.pem"
    REFRESH=(update-ca-trust extract)
  elif [[ -d /usr/local/share/ca-certificates ]]; then # Debian, Ubuntu
    ANCHOR="/usr/local/share/ca-certificates/hop-local-ca.crt"
    REFRESH=(update-ca-certificates)
  elif [[ -d /etc/ca-certificates/trust-source/anchors ]]; then # Arch
    ANCHOR="/etc/ca-certificates/trust-source/anchors/hop-local-ca.crt"
    REFRESH=(trust extract-compat)
  elif [[ -d /usr/share/pki/trust/anchors ]]; then # openSUSE
    ANCHOR="/usr/share/pki/trust/anchors/hop-local-ca.pem"
    REFRESH=(update-ca-certificates)
  else
    return 1
  fi
}

# Rebuilds the system bundle, showing the tool's chatter only if it fails.
linux_refresh_trust_store() {
  local out
  if ! out="$(sudo "${REFRESH[@]}" 2>&1)"; then
    err "${REFRESH[*]} failed: ${out}"
    return 1
  fi
}

# Chrome and Firefox keep their own NSS certificate databases on most
# distros and ignore the system store. Prints each database found.
linux_nss_dbs() {
  local db
  for db in "${HOME}/.pki/nssdb" "${HOME}/snap/chromium/current/.pki/nssdb" \
    "${HOME}"/.mozilla/firefox/*/ "${HOME}"/snap/firefox/common/.mozilla/firefox/*/ \
    "${HOME}"/.var/app/*/.pki/nssdb "${HOME}"/.var/app/*/.mozilla/firefox/*/; do
    if [[ -f "${db%/}/cert9.db" ]]; then
      printf '%s\n' "${db%/}"
    fi
  done
}

linux_untrust_ca() {
  local db
  if linux_find_trust_store && [[ -f "${ANCHOR}" ]]; then
    sudo rm -f "${ANCHOR}"
    linux_refresh_trust_store
  fi
  if command -v certutil > /dev/null; then
    while IFS= read -r db; do
      while certutil -D -d "sql:${db}" -n "${CA_NAME}" > /dev/null 2>&1; do :; done
    done < <(linux_nss_dbs)
  fi
}

# Trusts the CA in $1 system-wide and in each browser database found.
linux_trust_ca() {
  local db
  linux_untrust_ca
  if linux_find_trust_store; then
    sudo install -m 0644 "$1" "${ANCHOR}"
    linux_refresh_trust_store
  else
    err "unknown certificate store layout; trust ${STATE_DIR}/ca.pem yourself for https://name"
  fi
  if ! command -v certutil > /dev/null; then
    echo "Note: Chrome and Firefox keep their own certificate lists. For https://name in"
    echo "them, install certutil (Debian/Ubuntu: libnss3-tools, Fedora: nss-tools, Arch:"
    echo "nss) and run ./install.sh again."
    return
  fi
  # Chrome creates its database on first start; create it now if Chrome is
  # installed, so https://name works in Chrome from the start.
  if [[ ! -f "${HOME}/.pki/nssdb/cert9.db" ]] && have_any google-chrome google-chrome-stable chromium chromium-browser; then
    mkdir -p "${HOME}/.pki/nssdb"
    certutil -N -d "sql:${HOME}/.pki/nssdb" --empty-password
  fi
  while IFS= read -r db; do
    certutil -A -d "sql:${db}" -t "C,," -n "${CA_NAME}" -i "$1"
  done < <(linux_nss_dbs)
}

linux_flush_dns() {
  : # glibc and systemd-resolved re-read /etc/hosts by themselves
}

#######################################
# Both
#######################################

# Earlier releases kept the registry in ~/.hop. The daemon now keeps all its
# state in a root-only directory and never writes into a home directory, so
# move the registry over once, after the old daemon stopped and saved it.
migrate_registry() {
  local old="${OLD_DATA}/registry.json"
  if [[ -f "${old}" ]] && ! sudo test -e "${STATE_DIR}/registry.json"; then
    sudo install -m 0600 "${old}" "${STATE_DIR}/registry.json"
    sudo rm -f "${old}"
    rmdir "${OLD_DATA}" 2> /dev/null || true
    echo "Moved your shortcuts from ${old} to ${STATE_DIR}."
  fi
}

# A service manager restarts a crashing daemon, so check that hop really
# answers before reporting success.
wait_until_serving() {
  for _ in {1..40}; do
    if curl -fs -o /dev/null "http://${IP}/_hop/api/services"; then
      return 0
    fi
    sleep 0.25
  done
  err "hop did not start. Last log lines:"
  "${PLATFORM}_logs"
  exit 1
}

# Checks the HTTPS chain against hop's CA in $1 directly, so the result
# does not depend on which trust store this curl uses.
check_https() {
  if ! curl -fs -o /dev/null --cacert "$1" --resolve "hop:443:${IP}" "https://hop/_hop/api/services"; then
    err "http://hop works, but https://hop does not. Last log lines:"
    "${PLATFORM}_logs"
    exit 1
  fi
}

install_hop() {
  require go curl sudo
  if [[ "${PLATFORM}" == linux && ! -d /run/systemd/system ]]; then
    err "on Linux hop runs as a systemd service, and systemd is not running here"
    exit 1
  fi
  # Global, not local: the EXIT trap runs after this function has returned.
  WORK_DIR="$(mktemp -d)"
  trap 'rm -rf "${WORK_DIR}"' EXIT
  (cd "$(dirname "$0")" && go build -o "${WORK_DIR}/hop" .)
  sudo install -d -m 0755 "$(dirname "${BIN}")"
  sudo install -m 0755 "${WORK_DIR}/hop" "${BIN}"

  "${PLATFORM}_stop_service"
  sudo install -d -m 0700 "${STATE_DIR}"
  migrate_registry
  "${PLATFORM}_start_service"
  wait_until_serving

  # hop creates its CA on first start. Reading it needs root, but the copy
  # belongs to the user, for certutil and for checking HTTPS.
  # shellcheck disable=SC2024
  sudo cat "${STATE_DIR}/ca.pem" > "${WORK_DIR}/ca.pem"
  "${PLATFORM}_trust_ca" "${WORK_DIR}/ca.pem"
  check_https "${WORK_DIR}/ca.pem"
  echo "hop is running. Dashboard: http://hop/ (https://hop/ works too). Logs: ${LOGS_HINT}"
  echo "Add it to Claude Code with: claude mcp add -s user hop -- ${BIN} mcp"
}

uninstall_hop() {
  "${PLATFORM}_remove_service"
  sudo rm -f "${BIN}"
  # An attached -i suffix works with both GNU and BSD sed.
  sudo sed -i.hop-bak '/^# hop begin/,/^# hop end/d' /etc/hosts
  sudo rm -f /etc/hosts.hop-bak
  "${PLATFORM}_flush_dns"
  "${PLATFORM}_untrust_ca"
  sudo rm -f "${STATE_DIR}/ca.pem" "${STATE_DIR}/ca-key.pem"
  echo "hop removed. Your shortcuts are kept in ${STATE_DIR}/registry.json for a reinstall."
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
