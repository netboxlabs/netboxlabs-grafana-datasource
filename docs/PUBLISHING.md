# Publishing & Grafana Cloud readiness

How this plugin gets onto the Grafana plugin catalog and Grafana Cloud, and the checklist
it must pass. Sources are linked inline.

## The one hard rule for Grafana Cloud

**Grafana Cloud only runs plugins that are published to the Grafana plugin catalog and signed
by Grafana.** There is no sideload/upload path, and private/unsigned plugins cannot run on
Cloud. (Self-hosted OSS/Enterprise Grafana _can_ run a privately-signed or
`allow_loading_unsigned_plugins` build; that's how the demo runs today.)
See [Find and use plugins](https://grafana.com/docs/grafana-cloud/introduction/find-and-use-plugins/).

So Cloud support = **a catalog submission**. Because NetBox Labs is a commercial entity, this
is normally the **Commercial** signature level (requires a Commercial Plugin Subscription
arrangement with Grafana); a **Community** (free, fully-OSS) submission is also possible.
See [plugin policy](https://grafana.com/legal/plugins/) and
[publish a plugin](https://grafana.com/developers/plugin-tools/publish-a-plugin/publish-a-plugin).

## Reaching a customer's NetBox from Cloud

- **Public NetBox URL** → the Cloud backend calls it directly.
- **Private NetBox (VPC/on-prem)** → the customer enables
  **[Private Data Source Connect (PDC)](https://grafana.com/docs/grafana-cloud/connect-externally-hosted/private-data-source-connect/)**.
  PDC only supports _backend_ datasource plugins (this one qualifies). The plugin builds its
  HTTP client from the Grafana SDK (`backend/httpclient`) using the datasource instance
  settings, so the PDC tunnel, proxy and TLS options are applied automatically.

This matters because most NetBox installs are private. PDC is the expected path for them.

## Signing

```bash
export GRAFANA_ACCESS_POLICY_TOKEN=<token>   # Grafana Cloud → My Account → Security →
                                             # Access Policies, scope plugins:write
npm run sign                                  # wraps @grafana/sign-plugin
```

Signing writes `MANIFEST.txt` (SHA-256 of every file + signature) into `dist/`. For a public
(catalog) plugin you do **not** pass `--rootUrls`; Grafana signs after review. Re-sign after
any change to `dist/`. See [sign a plugin](https://grafana.com/developers/plugin-tools/publish-a-plugin/sign-a-plugin).

## Build artifacts in the packaged zip

The zip's top-level dir must be the plugin id (`netboxlabs-datasource/`) and contain:

```
plugin.json  module.js  module.js.map  README.md  CHANGELOG.md  LICENSE
MANIFEST.txt (after signing)  img/ (logos + screenshots)
gpx_netbox_linux_amd64      gpx_netbox_linux_arm64     gpx_netbox_linux_arm
gpx_netbox_darwin_amd64     gpx_netbox_darwin_arm64    gpx_netbox_windows_amd64.exe
```

`linux/amd64` + `linux/arm64` are what Cloud runs; the rest are for self-hosted catalog users.
All binaries `0755`. Build with `mage` (BuildAll) or the per-target cross-compile in
[demo/](../demo)/CI; frontend with `npm run build`.

## The plugin validator (gate for catalog + Cloud)

Run before every submission; the catalog runs it automatically.

```bash
npx -y @grafana/plugin-validator@latest \
  -sourceCodeUri https://github.com/<org>/<repo>/tree/<tag> \
  ./netboxlabs-datasource-<version>.zip
```

It checks archive structure, `plugin.json`, `module.js`, **backend binary presence/consistency**,
README, **CHANGELOG (required)**, LICENSE (must be an OSI license, Apache-2.0 ✓), **real
(non-placeholder) logos**, screenshots, keywords, broken links, and security scanners
(gosec/govulncheck/osv-scanner/semgrep/virus). **Zero `error`-severity findings** is required.

## Checklist status for this plugin

| Requirement                                                 | Status                                                                                             |
| ----------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| Plugin id matches `<org>-[<name>-]datasource`               | ✅ `netboxlabs-datasource`                                                                          |
| `backend: true` + `executable: gpx_netbox`                  | ✅                                                                                                 |
| `secureJsonData` for the API token (no secrets in jsonData) | ✅                                                                                                 |
| SDK HTTP client (PDC-compatible)                            | ✅ (uses `backend/httpclient`)                                                                     |
| Multi-arch binaries (amd64+arm64 + full matrix)             | ✅ via `mage`/CI                                                                                   |
| Real SVG logo (not the scaffold placeholder)                | ✅ official NetBox icon, vector-extracted from `netboxlabs_brand_guidelines_v3.pdf` (Visuals page) |
| Screenshots in `plugin.json`                                | ✅                                                                                                 |
| README + CHANGELOG + Apache-2.0 LICENSE                     | ✅                                                                                                 |
| `grafanaDependency` realistic minimum                       | ✅ `>=12.3.0`                                                                                      |
| No telemetry / tracking scripts                             | ✅                                                                                                 |
| Signed by Grafana                                           | ⏳ at submission (needs access-policy token)                                                       |
| Catalog submission (Community/Commercial)                   | ⏳ business decision (Commercial likely)                                                           |
| README images/links absolute for the catalog                | ⏳ at submission, currently repo-relative (see step 4)                                             |

## Remaining business/process steps (not code)

1. Decide Community vs Commercial level (coordinate a Commercial Plugin Subscription with
   Grafana if commercial).
2. Public source repo + matching release tag for `-sourceCodeUri`.
3. Generate the access-policy token, sign, and submit at grafana.com (Org Settings → My
   Plugins → Submit New Plugin).
4. **Convert README images and links from repo-relative to absolute.** The catalog page
   renders the packaged README on grafana.com, where relative paths don't resolve, and the
   validator's `brokenlinks` analyzer treats relative links as **errors**. While the repo
   is private we keep them relative so they render on github.com (absolute
   `raw.githubusercontent.com` URLs 404 for a private repo). Once the repo is public,
   rewrite image refs to
   `https://raw.githubusercontent.com/netboxlabs/netboxlabs-grafana-datasource/main/<path>`
   and doc links to `https://github.com/.../blob/main/<path>`, then re-run the validator.
