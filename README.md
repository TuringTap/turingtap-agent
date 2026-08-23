# turingtap-agent

Local companion binary for [TuringTap](https://turingtap.ai). Runs as a user
service and provides:

- **Reverse-SOCKS tunnel** — outbound WSS to `relay.turingtap.ai`, yamux-multiplexed.
  The cloud proxy dials RFC1918 / loopback destinations *through* this agent so
  your AI can inspect LAN-internal APIs. Destinations are gated by
  `lan_allow_cidrs`; public IPs are always refused. **No inbound ports.**
- **Managed Chromium** — a single persistent [go-rod] instance launched with
  `--proxy-server=proxy.turingtap.ai:8443` and
  `--ignore-certificate-errors-spki-list=<CA SPKI>`. The cloud MCP tools
  `goto` / `act` drive it headless; `ask_human` flips it headed with an overlay
  banner and screencasts it to your phone.
- **Local MCP server** — SSE on `http://127.0.0.1:7847/sse`. Forwards to the
  cloud `mcp-gateway` and adds two local tools: `agent_status`,
  `agent_lan_test(host, port)`.
- **Tray icon** — online/offline indicator, *Open browser* (freeform recorder).
- **Cred-rotate toast** — after a session that observed credentials, an OS
  notification reminds you to rotate them.

The agent contains **no** exit-node / relay-for-others code.

## Config

`~/.turingtap/agent.toml`:

```toml
api_key         = "ttk_live_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
relay_url       = "wss://relay.turingtap.ai/agent"
proxy_url       = "proxy.turingtap.ai:8443"
ca_spki         = "BASE64_SPKI_HASH_OF_TURINGTAP_CA"
mcp_gateway_url = "https://mcp.turingtap.ai"
lan_allow_cidrs = ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "127.0.0.0/8"]
```

`lan_allow_cidrs` defaults to RFC1918 + loopback if omitted.

## Build

```sh
go build ./...
go vet ./...
```

Requires Go 1.22+. go-rod downloads a Chromium binary on **first run**, not at
build time.

### System tray

`getlantern/systray` needs CGO (GTK3 dev headers on Linux, AppKit on macOS).
The tray is therefore behind a build tag:

```sh
# Linux prerequisites: libayatana-appindicator3-dev (or libappindicator3-dev), gcc
CGO_ENABLED=1 go build -tags tray ./...
```

Without `-tags tray` the agent builds CGO-free and runs without a tray icon
(suitable for CI and headless servers). Release builds via `goreleaser` are
currently tray-less (`CGO_ENABLED=0`); per-platform CGO builds land with
code-signing in a later pass.

## Release

```sh
goreleaser release --clean
```

Produces darwin/linux/windows archives under `dist/`. On tagged releases the
`sign` job in `.github/workflows/release.yml` then re-signs the darwin
tarballs with a Developer ID Application certificate (hardened runtime),
notarizes them with Apple, regenerates `checksums.txt`, and GPG-signs it as
`checksums.txt.asc`. See `scripts/sign-macos.sh`.

Notarization tickets for bare Mach-O binaries cannot be stapled (stapling
applies to bundles, dmgs, and pkgs); Gatekeeper fetches the ticket from
Apple online on first run.

## Verifying a release

Release checksums are signed with the TuringTap release GPG key, whose
public half is committed as [`RELEASES.pub.asc`](RELEASES.pub.asc)
(uid `TuringTap Releases <github@turingtap.ai>`).

```sh
# one-time: import the release key
curl -fsSL https://raw.githubusercontent.com/turingtap/turingtap-agent/main/RELEASES.pub.asc | gpg --import

# per release: verify the checksum file, then the artifact
gpg --verify checksums.txt.asc checksums.txt
sha256sum -c --ignore-missing checksums.txt   # shasum -a 256 on macOS
```

`https://turingtap.ai/install.sh` performs both checks automatically when
`gpg` is installed (and warns instead of failing when it is not).

[go-rod]: https://github.com/go-rod/rod
