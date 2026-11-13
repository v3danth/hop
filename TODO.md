# TODO

## Upgrade the macOS install

The Mac still runs the build from before the Linux release. Run `./install.sh`
in a terminal (it needs sudo). It stops the old service, moves
`~/.hop/registry.json` to `/Library/Application Support/hop/registry.json`,
keeps the existing CA and trusts it again (macOS may ask to confirm).

Then check:

- [ ] `hop ls` shows `yt`, `gh`, `orbit-dev` and `orbit-prod` with their visit counts
- [ ] `~/.hop` is gone
- [ ] `curl -I yt/` redirects and `curl https://yt/` works without `--cacert`
- [ ] `sudo launchctl print system/dev.hop` shows the service running

## Not tested yet

- [ ] The macOS install path of `install.sh` after the Linux changes
- [ ] CA trust on Fedora/RHEL (`update-ca-trust`), Arch (`trust extract-compat`)
      and openSUSE; only Debian 13 was tested
- [ ] Chrome and Firefox on Linux in a real browser; only the NSS database entry
      was checked
- [ ] The SELinux `restorecon` step for `/etc/hosts` (Fedora/RHEL)

## Maybe

- [ ] Uninstall leaves a blank line at the end of `/etc/hosts`
- [ ] Commit `fc1c075`'s message says it adds the no-overwrite change, which was
      in the commit before it; fixing it would need a force-push
