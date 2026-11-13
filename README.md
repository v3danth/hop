# hop

Short names for websites and local apps on macOS and Linux: `yt/`, `talk-to-db/`,
`go/anything`. One Go binary, no dependencies.

## Install

Needs Go, curl and sudo; on Linux, also systemd.

```bash
./install.sh
```

The script builds hop, installs it to `/usr/local/bin/hop` and runs it as a system
service that starts at boot (launchd on macOS, systemd on Linux). hop listens on ports
80 and 443 of its own loopback address, `127.0.0.2`, so links need no port while
`localhost` stays free for your other apps, and it keeps a managed block of `/etc/hosts`
in sync. `./install.sh uninstall` removes it again and keeps your shortcuts.

Dashboard: http://hop/ · Logs: `/var/log/hop.log` on macOS, `journalctl -u hop` on Linux.
You can also `go install github.com/v3danth/hop@latest` and run `hop serve` yourself.

## Use

```bash
hop add yt youtube.com                             # website shortcut
hop add ys 'youtube.com/results?search_query=%s'   # ys/lofi -> search
hop add api 8080                                   # app on a fixed port (or localhost:8080)
hop run web -- npm run dev                         # free port as $PORT, removed on exit
hop ls
hop rm yt
```

Every name works as `name/`, `https://name/` and `go/name/...`. Type the `/` (or
`http://`) the first time, so the browser does not search for the word. curl works too;
add `-L` to follow a website shortcut's redirect: `curl -L yt/`.

`hop add` will not replace an existing name unless you pass `-f`. `hop run` takes over a
name held by an earlier run of an app, but not a permanent shortcut unless you pass `-f`.

Local apps get each request as if the browser had opened `localhost:<port>` itself: hop
rewrites `Host`, the `Origin` of the app's own pages, and redirects back to
`localhost:<port>`, so dev servers such as Vite, webpack, Next and Rails accept it. Apps
listening on IPv4 or IPv6 localhost both work.

## From any language

Read the port from `$PORT`, then register once at startup. Pass your PID so the name is
removed when the process exits:

```bash
curl -X POST http://127.0.0.2/_hop/api/services \
  -H 'Content-Type: application/json' \
  -d '{"name":"talk-to-db","port":8080,"pid":12345}'
```

| Method | Path | |
|---|---|---|
| GET | `/_hop/api/services` | list, with live status |
| POST | `/_hop/api/services` | `{"name","url"}` or `{"name","port","pid"?}`; add `"replace":false` to refuse a taken name |
| DELETE | `/_hop/api/services/{name}` | remove; with `?pid=N`, only while that process holds the name |
| GET | `/_hop/api/free-port` | `{"port": N}` |

## Claude Code

```bash
claude mcp add -s user hop -- /usr/local/bin/hop mcp
```

Tools: `list_services`, `register_service`, `remove_service`, `free_port`.

## HTTPS

`https://yt` needs a certificate for `yt`, which no public CA issues, so hop runs its
own local CA, the way `mkcert` does. It signs a one-year certificate per name on first
use, and only for names it serves.

The install adds one root certificate, `hop local CA` (ECDSA P-256, valid 10 years):

- macOS: the System keychain (Keychain Access → System → Certificates). Safari, Chrome
  and curl use it; Firefox does too unless `security.enterprise_roots.enabled` is off.
- Linux: the system store (`update-ca-certificates` or `update-ca-trust`), which curl and
  most tools use, plus the NSS databases of Chrome and Firefox when `certutil` is
  installed (`libnss3-tools` on Debian and Ubuntu, `nss-tools` on Fedora, `nss` on Arch).

Some names, such as `app`, `dev` and `page`, are top-level domains that browsers always
open over HTTPS. They work as long as the browser trusts hop's CA.

## Where hop keeps its state

The registry and the CA live in a root-only directory: `/Library/Application Support/hop`
on macOS, `/var/lib/hop` on Linux. Anyone who can read the CA key could impersonate any
website to this machine, which is why it is root-only, and why hop never writes into a
home directory. `./install.sh uninstall` deletes the CA and its trust, and keeps
`registry.json`.

## Notes

- The API only answers hop's own names, rejects cross-site `Origin`s and needs a JSON
  content type, so web pages cannot change your shortcuts.
- `HOP_URL` points the CLI and MCP server at a non-default server, e.g. `http://127.0.0.1:8787`.
- Chrome resolves `name.localhost` to 127.0.0.1 itself, ignoring `/etc/hosts`, so use
  `name/` there; `name.localhost` only reaches hop in browsers that read the hosts file.
- On Linux, hop cannot share port 80 or 443 with a server listening on all addresses
  (`0.0.0.0:80`, such as nginx or `docker run -p 80:80`); bind that server to a specific
  address instead.
- Names work only on this machine: not from other devices, and not inside containers,
  which have their own `/etc/hosts`.
