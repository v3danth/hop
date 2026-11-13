# hop

Short names for websites and local apps: `yt/`, `talk-to-db/`, `go/anything`.
One Go binary, no dependencies.

## Install

```bash
./install.sh
```

Builds hop, installs it to `/usr/local/bin/hop`, and runs it as a launchd daemon that
keeps a managed block of `/etc/hosts` in sync. It listens on ports 80 and 443 of its own
loopback address, `127.0.0.2`, so links need no port while `localhost` stays free for
your other apps. Needs sudo. `./install.sh uninstall` removes it again.

Dashboard: http://hop/

## Use

```bash
hop add yt youtube.com                                 # website shortcut
hop add ys 'youtube.com/results?search_query=%s'      # ys/lofi -> search
hop add api 8080                                       # app on a fixed port
hop run face-glitch-mob -- npm run dev                 # free port as $PORT, auto-removed on exit
hop ls
hop rm yt
```

Every name works as `name/`, `https://name/` and `go/name/...`. Type the `/` (or `http://`) the first
time, so the browser does not search for the word.

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
| POST | `/_hop/api/services` | `{"name","url"}` or `{"name","port","pid"?}` |
| DELETE | `/_hop/api/services/{name}` | remove |
| GET | `/_hop/api/free-port` | `{"port": N}` |

## Claude Code

```bash
claude mcp add -s user hop -- /usr/local/bin/hop mcp
```

Tools: `list_services`, `register_service`, `remove_service`, `free_port`.

## HTTPS

`https://yt` needs a certificate for `yt`, which no public CA issues, so hop runs its
own local CA, the way `mkcert` does. On first start it creates the CA in
`/Library/Application Support/hop` (root-only, key `0600`), and `install.sh` trusts it
in the System keychain. hop then signs a one-year certificate per name on first use,
and only for names it serves, so the CA never signs for a real website.

Anyone who can read the CA key could impersonate any site to this Mac, which is why it
is root-only. `./install.sh uninstall` removes the trust and deletes the CA. Safari and
Chrome use the System keychain; Firefox does too unless
`security.enterprise_roots.enabled` is turned off.

## Notes

- The API only answers local host names, rejects cross-site `Origin`s and needs a JSON
  content type, so web pages cannot change your shortcuts.
- Local apps are proxied with `Host: localhost:<port>`, so Vite/webpack host checks pass.
- `HOP_URL` points the CLI and MCP server at a non-default server, e.g. `http://127.0.0.1:8787`.
- Chrome resolves `name.localhost` to 127.0.0.1 itself, ignoring `/etc/hosts`, so use
  `name/` there; `name.localhost` only reaches hop in browsers that read the hosts file.
