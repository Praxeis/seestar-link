# seestar-link

Remote access to a ZWO **Seestar** telescope over the internet, reaching a scope behind
NAT with no LAN access. It reimplements ThroughTek's **Kalay** P2P stack in Go and runs
the Seestar's own services over that tunnel, so tools like NINA can talk to a scope that
is miles away as if it were on your desk.

This is a fork of [zrbecker/seestar-remote](https://github.com/zrbecker/seestar-remote),
which did the reverse engineering. This fork adds an easier setup:

- **One `seestar` command** with prebuilt downloads for Windows, macOS and Linux
- **Saved login**, kept in Windows DPAPI, the macOS Keychain, or an owner-only file on Linux
- **Profiles**, so several scopes can be named, switched between, and proxied at once
- **Auto-reconnect**: the proxy redials on its own when the link drops
- **Background service**: a Windows service, macOS LaunchAgent, or systemd user unit
- **Status page** showing whether the tunnel is up

## Install

Download the archive for your system from the
[Releases](https://github.com/Praxeis/seestar-link/releases) page and unpack it:

| system | file |
|---|---|
| Windows | `seestar-<version>-windows-amd64.zip` |
| Mac with Apple Silicon (M1 and later) | `seestar-<version>-darwin-arm64.tar.gz` |
| Mac with Intel | `seestar-<version>-darwin-amd64.tar.gz` |
| Linux PC | `seestar-<version>-linux-amd64.tar.gz` |
| Raspberry Pi (64-bit OS) | `seestar-<version>-linux-arm64.tar.gz` |

Put `seestar` (or `seestar.exe`) somewhere on your PATH. On a Mac, the first run of a
downloaded binary may be blocked by Gatekeeper; clear it with
`xattr -d com.apple.quarantine seestar`.

Or build it yourself with Go 1.26:

```
go install github.com/Praxeis/seestar-link/cmd/seestar@latest
```

## Quick start

```
seestar login                 # save your ZWO account login
seestar profile add s30       # pick a scope from your account and name it
seestar proxy                 # serve it on 127.0.0.1:4700
```

Then, in another terminal:

```
printf '{"id":1,"method":"get_device_state"}\r\n' | nc 127.0.0.1 4700
```

To keep it running in the background, starting at boot (Windows) or login (macOS/Linux):

```
seestar service install
seestar status
```

## Profiles

A profile names one scope: the ZWO account it belongs to, its serial, and the local
ports and status address its proxy uses.

```
seestar profile add s30                              # first scope, ports 4700:4700
seestar profile add s50                              # second scope, ports 4710:4700
seestar profile set s30 -p 4700:4700 -p 32323:32323  # also expose Alpaca
seestar profile list
seestar profile use s50                              # make s50 the default
seestar proxy -profile s30                           # any command takes -profile
```

Each new profile gets local ports and a status address that do not clash with the
existing ones, so two scopes can be proxied at the same time. `-email` on `profile add`
uses a different ZWO account; scopes shared with you by another account are listed too.

Profiles live in `config.json` in your user config folder (`%AppData%\seestar-link` on
Windows, `~/Library/Application Support/seestar-link` on macOS, `~/.config/seestar-link`
on Linux). `-config-dir` or `SEESTAR_CONFIG_DIR` points elsewhere.

## Commands

| command | what it does |
|---|---|
| `login` | checks your ZWO login and saves the password |
| `logout` | removes a saved password |
| `devices` | lists the Seestars on your account |
| `profile` | adds, lists, picks and edits scope profiles |
| `proxy` | serves the scope's ports on this computer |
| `status` | shows whether a proxy's tunnel is up |
| `service` | installs, starts, stops and removes the background service |
| `download` | copies the scope's image archive to this computer |
| `upload` | copies files onto the scope's image share |

`seestar <command> -h` shows each command's flags. Every command accepts `-profile`,
`-config-dir`, and `-log FILE` (append output to a file).

The original separate `proxy`, `download` and `upload` programs still build from
`cmd/proxy`, `cmd/download` and `cmd/upload`, and the environment variables below still
work, so existing scripts keep running.

## proxy

Maps device ports to local ones, Docker style, `-p local:device`:

```
seestar proxy -p 4700:4700 -p 32323:32323 -p 8080:80
curl http://127.0.0.1:32323/management/v1/configureddevices
```

Without `-p`, the profile's ports are used, else `4700:4700`.

| device port | service |
|---|---|
| `4700` | JSON-RPC control API (newline-delimited JSON) |
| `32323` | ASCOM Alpaca REST. The scope serves this itself, so NINA and ConformU can be pointed straight at it |
| `80` | HTTP image server (a fresh channel per client; map another port as well) |

Any tool speaking the Seestar LAN protocols works unchanged, because a mapped port looks
like the device on localhost. All mappings share one tunnel, since the device rejects a
second concurrent Kalay session. While the proxy runs it holds the device's single remote
session, so the Seestar app cannot connect remotely at the same time; stop the proxy (or
the service) to use the app. One client at a time per mapped port; a second is refused
with an error. Port 80 is the exception: the scope's image server closes its socket after
each response, so every client gets its own fresh channel, and several can be served at
once. A client that stops reading for 10 seconds is disconnected, so it cannot stall the
link for everyone else.

**Reconnect.** If the tunnel drops, the proxy closes any connected clients, waits (5s,
10s, 20s, 40s, then every 60s; back to 5s after a session that lasted two minutes),
logs in again if needed, and redials. The listeners stay up the whole time, and a client that
connects while the link is down is held for up to 30 seconds instead of being refused.
`-no-reconnect` restores the old behaviour of exiting.

**Status page.** The proxy serves its state as JSON on `127.0.0.1:4799` (or the
profile's address). `seestar status` reads it; `/healthz` returns 200 while connected
and 503 otherwise, for monitoring tools. `-status off` disables it.

`-listen` (or `LISTEN_HOST`) sets the bind address, default `127.0.0.1`. Binding to
`0.0.0.0` exposes the scope's control ports to your whole network with no
authentication, so only do that on a network you trust.

## service

```
seestar service install [-profile NAME]
seestar service status | start | stop | uninstall
```

Run `seestar login` and `seestar profile add` first: the service uses the saved login and
the profile's ports, and never prompts. Each profile gets its own service.

- **Windows**: a real Windows service named "Seestar Link (NAME)". Install from an
  Administrator terminal. It starts at boot, before anyone logs in, and restarts on
  failure. The program is copied to `C:\Program Files\seestar-link`, and the profile and
  login to `C:\ProgramData\seestar-link`, readable only by SYSTEM and Administrators,
  with the password under machine-scope DPAPI. Log: `C:\ProgramData\seestar-link\logs`.
- **macOS**: a LaunchAgent for your user, running while you are logged in. Log:
  `~/Library/Logs/seestar-link/NAME.log`.
- **Linux**: a systemd user unit, `seestar-link-NAME`. Log:
  `journalctl --user -u seestar-link-NAME`. Run `loginctl enable-linger $USER` so it
  keeps running while you are logged out and starts at boot. Suited to a Raspberry Pi.

Reinstalling replaces the service and picks up a new binary or changed profile.

## download

```
seestar download
OUTBASE=/path/to/archive seestar download
```

Walks the device SMB tree and fetches each file, skipping any already present at
matching size, then re-walks the device and diffs. It prints `VERIFIED COMPLETE` only
when every device file is present locally at the exact size. A manifest of the device
tree is written to `$OUTBASE/_device_manifest.tsv`.

Env: `OUTBASE` (default `./seestar-archive`), `SHARE` (default `EMMC Images`), `ROOT`
(default `MyWorks`), `SMB_PARALLEL` (default 4), `SMB_USER` (default `Guest`),
`MIN_FETCH_BPS` (throughput floor for timeouts, default 400000), `LIST_ONLY` (write the
manifest and stop).

Scope is narrowed with `TARGETS` (comma-separated top-level directories under `ROOT`)
and `EXCLUDE_EXT` (comma-separated extensions to skip); empty means no restriction.

```
TARGETS="M 24,Mizar" EXCLUDE_EXT=".mp4,.avi" seestar download
```

Directories outside `TARGETS` are never opened. Matching is on whole path segments, so
`M 24` does not pull in `M 24_sub`. The same filter applies to the download walk, the
manifest and the verify pass, so `VERIFIED COMPLETE` describes the filtered scope.

Each file is fetched by `SMB_PARALLEL` concurrent range reads streamed into a `.part`
file, renamed only once every byte is on disk, so memory stays flat regardless of file
size. A file that fails four times is left for the verify pass to report as missing.

The download uses the device's single remote session, so stop a running proxy or
service for that scope first.

## upload

```
seestar upload <local_path> <device_rel_path>
seestar upload ./finals M31/finals          # a directory
seestar upload ./M31.fit finals/M31.fit     # a single file
```

Writes into the device's SMB image share, creating missing parent directories and
reading the size back to confirm. A directory uploads every file beneath it in one
session, which avoids the cooldown the device imposes after each disconnect.

Env: `SHARE`, `ROOT`, `SMB_USER` as above; `UPLOAD_CHUNK` (SMB write size, default
8192), `UPLOAD_WORKERS` (concurrent writers, default 16). Outbound throughput is bounded
by the device's small receive buffer, a few hundred KB/s on a typical remote path.

## Environment

Settings are taken from the first place that has them: the environment, then the
profile and saved login, then a prompt.

| env | meaning |
|---|---|
| `SEESTAR_PROFILE` | profile to use, same as `-profile` |
| `SEESTAR_EMAIL`, `SEESTAR_PASSWORD` | ZWO account, overriding the profile and saved login |
| `SEESTAR_SN`, `SEESTAR_MODEL` | device, overriding the profile |
| `SEESTAR_MASTER` | Kalay rendezvous master, default `119.45.181.137:3478` |
| `SEESTAR_CONFIG_DIR` | configuration folder, same as `-config-dir` |
| `SEESTAR_NO_PROMPT` | fail instead of prompting, same as `-no-prompt` |
| `KALAY_TUN_DEBUG` | tunnel and RDT tracing on stderr |
| `KALAY_LIVENESS_TIMEOUT` | seconds of device silence before the tunnel is declared dead (default 15, 0 disables) |

## How it works

```
ZWO cloud auth (login + online-link)           seestar/cloud.go
  └─ IOTC P2P rendezvous + NAT hole-punch      kalay/session.go
       └─ DTLS 1.2 ECDHE_PSK_CHACHA20_POLY1305 kalay/dtls.go
            └─ RDT reliable transport          kalay/rdt.go, kalay/tunnel.go
                 └─ P2PTunnel channels         kalay/tunnelconn.go
                      └─ device :4700 JSON-RPC, :445 SMB
```

`online-link` authorizes a fresh connection window per session. The device accepts
**one** remote session at a time.

## Sharp edges

- **Narrow, moving target.** The transport was verified against a Seestar S30 Pro on one
  firmware. ZWO's cloud API is private and unversioned, and ports and behaviour move
  across models and firmware, so any of this can break without notice.
- **One session.** A running proxy, service, download or upload occupies the scope's
  single remote session. The phone app and the other tools cannot connect meanwhile.
- **Reconnect is not instant.** A dead link is noticed after `KALAY_LIVENESS_TIMEOUT`
  (15s) of silence, then the backoff applies. A client mid-request sees its connection
  close and must reconnect.
- **The proxy is a byte pipe.** It does not parse the tunnelled protocols, and a client
  pushing large writes must chunk them, since a single write larger than the tunnel's
  send window stalls.
- **`download` verifies by size, not content.** `VERIFIED COMPLETE` means every device
  file is present locally at the exact byte length; it does not hash.

## Layout

- `cmd/seestar` - the `seestar` command
- `cmd/proxy`, `cmd/download`, `cmd/upload` - the original standalone programs
- `internal/cmd/` - each command's implementation
- `internal/config` - profiles; `internal/secret` - saved passwords
- `kalay/` - the reversed transport: control, session, dtls, rdt, tunnel
- `seestar/` - ZWO cloud auth and the tunnel dial
- `internal/tnauth/` - the `51cc` relay authorization and coordination channel

## Releasing

Push a tag such as `v0.1.0`, or on GitHub choose Releases, Draft a new release, type a
new tag such as `v0.1.0` and publish. Either way the Release workflow builds every
platform and attaches the archives and `SHA256SUMS.txt` to that release within a few
minutes.

## License

MIT, as the original. See [LICENSE](LICENSE).
