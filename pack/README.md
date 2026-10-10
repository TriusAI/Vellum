# Installing Vellum globally + running it as a service

Vellum ships as a **portable one-folder pack**: the `vellum` binary, the two
`llama.cpp` servers (`llm/`), the GGUF models (`models/`), `mutool` +
`tesseract` (`bin/`) and their libs (`lib/`), `tessdata/`, the chat template,
`config.yaml` and the `vellum.sh` launcher.

"Installing globally" means dropping that folder somewhere system-wide
(conventionally `/opt/vellum`) and starting it under an init system. Service
files for both are in this directory:

- `openrc/vellum` — `/etc/init.d/vellum`
- `openrc/vellum.conf` — `/etc/conf.d/vellum`
- `systemd/vellum.service` — `/etc/systemd/system/vellum.service`

The steps below are shared; jump to [OpenRC](#openrc-artixgentooalpine) or
[systemd](#systemd-debianubuntufedoranixos) at the end.

## 1. Lay down the pack

From the repo (replace `<hash>` with the current build, e.g. `ff025ec`):

```sh
sudo useradd -r -d /opt/vellum -s /sbin/nologin vellum
sudo install -d -m 0755 /opt/vellum
sudo tar -C /opt/vellum --strip-components=1 -xzf \
    pack/vellum-<hash>-linux-x86_64.tar.gz
sudo chown -R vellum:vellum /opt/vellum
```

`--strip-components=1` drops the tarball's top-level
`vellum-<hash>-linux-amd64/` directory. `/opt/vellum` is the `VELLUM_HOME`
the service files refer to.

> Shortcut for testing without copying ~3.8 GB of models: skip the `tar` and
> point `VELLUM_HOME` (conf.d / the unit's paths) at the existing stage
> directory, e.g. `.../pack/stage/vellum-<hash>-linux-amd64`.

## 2. Make the tool paths absolute in `config.yaml`

This matters for a global install. The pack's config paths are relative to
the pack root:

```yaml
tools:
    mutool: bin/mutool
    tesseract: bin/tesseract
    tessdata: tessdata
```

Vellum execs these by the literal string, so a relative path resolves against
the **current working directory** — fine for `vellum.sh` run from the pack,
broken for `vellum search` from anywhere else. Make them absolute:

```yaml
tools:
    mutool: /opt/vellum/bin/mutool
    tesseract: /opt/vellum/bin/tesseract
    tessdata: /opt/vellum/tessdata
```

(`library_dir` and `db` are resolved relative to the config file, so they
need no change.)

## 3. Make the CLI find the config

The binary resolves its config as `--config` → `$VELLUM_CONFIG` →
`./config.yaml`. Put it in the environment once:

```sh
sudo tee /etc/profile.d/vellum.sh >/dev/null <<'EOF'
export VELLUM_CONFIG=/opt/vellum/config.yaml
EOF
sudo ln -s /opt/vellum/vellum /usr/local/bin/vellum
```

Log out/in (or `source` it) and `vellum --version` works from any directory.

> `/etc/profile.d/` is only read by **login** shells. To make the config be
> found no matter how `vellum` is invoked, replace the symlink with a wrapper:
>
> ```sh
> sudo rm /usr/local/bin/vellum
> sudo tee /usr/local/bin/vellum >/dev/null <<'EOF'
> #!/bin/sh
> exec /opt/vellum/vellum --config /opt/vellum/config.yaml "$@"
> EOF
> sudo chmod +x /usr/local/bin/vellum
> ```
>
> Note: if the service runs as a dedicated user (below), the `library.db` is
> owned by that user, so `vellum` from your own shell cannot write it — run
> the CLI as the service user instead (see
> [Ingesting sources under your home](#ingesting-sources-under-your-home-permissions)).

## OpenRC (Artix/Gentoo/Alpine)

```sh
sudo install -m 0755 pack/openrc/vellum      /etc/init.d/vellum
sudo install -m 0644 pack/openrc/vellum.conf /etc/conf.d/vellum
sudo rc-update add vellum default
sudo rc-service vellum start
```

Check:

```sh
rc-service vellum status
curl -s http://127.0.0.1:8090/api/status
```

The unit starts the bundled chat server on `:8081` and the embedding server
on `:8082` (unless the config selects an external backend), waits on
`/health`, then supervises `vellum serve`; on stop it kills the servers it
started, from pidfiles. All of it is tunable in `/etc/conf.d/vellum` (user,
ports, model file, `cpu`/`vulkan`/`auto`).

## systemd (Debian/Ubuntu/Fedora/NixOS…)

All-in-one unit (uses the pack's `vellum.sh`; needs `curl`):

```sh
sudo install -m 0644 pack/systemd/vellum.service /etc/systemd/system/vellum.service
sudo systemctl daemon-reload
sudo systemctl enable --now vellum.service
systemctl status vellum
```

### Optional: split units (keep models loaded across UI restarts)

The all-in-one unit restarts the models whenever the main process restarts.
If you'd rather keep the two servers up independently — and skip the `curl`
dependency — install three units instead.

`/etc/systemd/system/vellum-llm.service`:

```ini
[Unit]
Description=Vellum chat model server (llama.cpp, port 8081)
After=network.target

[Service]
Type=simple
User=vellum
Group=vellum
WorkingDirectory=/opt/vellum
Environment=LD_LIBRARY_PATH=/opt/vellum/lib
ExecStart=/opt/vellum/llm/vulkan/llama-server \
    -m /opt/vellum/models/qwen3-4b.gguf \
    --host 127.0.0.1 --port 8081 -np 1 -c 8192 --jinja \
    --chat-template-file /opt/vellum/templates/qwen3-nothink.jinja
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

(Use `/opt/vellum/llm/cpu/llama-server` for the CPU build.)

`/etc/systemd/system/vellum-embed.service` — the embedding model must stay on
the CPU backend by design (GPU-memory pressure corrupts it):

```ini
[Unit]
Description=Vellum embedding model server (llama.cpp, port 8082)
After=network.target

[Service]
Type=simple
User=vellum
Group=vellum
WorkingDirectory=/opt/vellum
Environment=LD_LIBRARY_PATH=/opt/vellum/lib
ExecStart=/opt/vellum/llm/cpu/llama-server \
    -m /opt/vellum/models/nomic-embed-text-v1.5.gguf \
    --host 127.0.0.1 --port 8082 -np 1 --embeddings --ubatch-size 2048
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

`/etc/systemd/system/vellum.service` (replaces the all-in-one unit):

```ini
[Unit]
Description=Vellum library manager (web UI + JSON API)
After=network.target vellum-llm.service vellum-embed.service
Wants=vellum-llm.service vellum-embed.service

[Service]
Type=simple
User=vellum
Group=vellum
WorkingDirectory=/opt/vellum
Environment=LD_LIBRARY_PATH=/opt/vellum/lib
Environment=TESSDATA_PREFIX=/opt/vellum/tessdata
ExecStart=/opt/vellum/vellum --config /opt/vellum/config.yaml serve --listen 127.0.0.1:8090
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

`vellum serve` does not need the model servers to be up at boot (it fetches
on demand), so the ordering is convenience, not a hard dependency. Skip the
two server units entirely if your config uses an external backend.

## Files at runtime

| Path | What |
|------|------|
| `/opt/vellum/config.yaml` | config (paths resolve relative to this file) |
| `/opt/vellum/library.db` | the SQLite library (created on first run) |
| `/opt/vellum/library/`, `/opt/vellum/covers/` | indexed files + cover thumbnails |
| logs | OpenRC: `/var/log/vellum/*.log`; systemd: `journalctl -u vellum` |
| `/run/vellum/*.pid` | pidfiles for the supervisor + the two servers (OpenRC) |

## Behaviour notes

- The service starts the **bundled** llama.cpp servers itself unless the
  config selects an external backend (`llm.backend: ollama`,
  `embed.provider: ollama`, or `external: true`).
- If a server is **already healthy** on its port, the launcher reuses it
  instead of starting another (and therefore won't stop it). Kill stale
  servers from a previous manual run first if you're chasing a wrong-model
  bug — they bind fixed ports silently.
- **Don't point the service at a library another `vellum serve` is already
  using.** Two processes on one SQLite file mostly work under WAL but you'll
  see `database is locked` and two UIs showing diverging state. Give the
  service its own library, or stop the manual instance first.
- To move an existing library in, copy its `library.db` (and `library/`,
  `covers/`) into `/opt/vellum` and `chown -R vellum:vellum` them. A fresh
  install creates a new empty library.
- Back up before upgrading a binary: `vellum export /path/backup.zip` writes a
  consistent snapshot.

## Ingesting sources under your home (permissions)

Vellum indexes files **in place** — it stores their paths and the extracted
text; it does not copy the sources. So whatever directory you ingest or watch
must be readable by the user the service runs as (the `vellum` system user by
default). A home directory is usually `0700`, so that user cannot even
traverse into it:

```
drwx------ you you /home/you
$ curl -s "http://127.0.0.1:8090/api/fs?path=/home/you"
{"error":"open /home/you: permission denied"}
```

This is what makes the watcher fail to start, and what makes an ingest fail
with `permission denied`. Two ways to fix it (replace `you` with your login
name):

**Run the service as your own user** — simplest on a single-user machine. Set
`VELLUM_USER`/`VELLUM_GROUP` in `/etc/conf.d/vellum` (or `User=`/`Group=` in
the systemd unit) to your account, then hand it the install:

```sh
sudo rc-service vellum stop
sudo chown -R you:you /opt/vellum /var/log/vellum /run/vellum
sudo rc-service vellum start
```

**Keep the dedicated user and grant it access with ACLs** — more isolation:

```sh
# traverse-only through your home (can follow a path, cannot list it)
sudo setfacl -m u:vellum:x /home/you
# read + traverse the source tree, now and for files added later
sudo setfacl -R  -m u:vellum:rX "/home/you/library"
sudo setfacl -R -d -m u:vellum:rX "/home/you/library"
# optional: also let the UI's file browser list your home
# sudo setfacl -m u:vellum:rx /home/you
sudo rc-service vellum restart

# verify — the API runs as the service user, so this is the real test:
curl -s "http://127.0.0.1:8090/api/fs?path=/home/you/library"
```

With a dedicated service user the `library.db` is owned by that user, so the
CLI has to run as them. The pack's shared libraries must be on the path for
OCR (`tesseract`), which the service normally exports but a bare `sudo` does
not:

```sh
sudo -u vellum env \
    LD_LIBRARY_PATH=/opt/vellum/lib \
    TESSDATA_PREFIX=/opt/vellum/tessdata \
    /opt/vellum/vellum --config /opt/vellum/config.yaml \
    ingest "/home/you/library"
```

`sudo -u vellum` works even though the account's shell is `/sbin/nologin`
(that only affects login shells). You can skip both approaches when using the
UI: the Ingest dialog has a **"paste paths"** toggle, so you can paste the
folder directly, and the Watch dialog can register it once the ACLs are in
place.

## Least privilege

The simplest setup runs everything as one `vellum` user that owns
`/opt/vellum`. To keep models/tools root-owned, create the user first, then
hand only the writable bits to it:

```sh
sudo chown -R root:root /opt/vellum
sudo chown -R vellum:vellum /opt/vellum/library /opt/vellum/covers
sudo chown vellum:vellum /opt/vellum /opt/vellum/library.db* /opt/vellum/*.log
```

Or run entirely as root (OpenRC: `VELLUM_USER="root"` in
`/etc/conf.d/vellum`; systemd: set `User=root`/`Group=root`).

## Troubleshooting

- **`exec: "mutool": executable file not found in $PATH`** (or `tesseract`) —
  the config was not loaded, so the built-in defaults (the bare word `mutool`)
  were used. The resolver is `--config` → `$VELLUM_CONFIG` → `./config.yaml`;
  this happens when none is set/exported and the working directory has no
  `config.yaml`. With no config the working directory also becomes the library
  root, so a run like `vellum ingest .` creates a stray `./library.db`
  (`-wal`/`-shm`) there — delete it. Fix by passing `--config`, exporting
  `VELLUM_CONFIG`, or installing the wrapper in step 3.
- **Ingest finds nothing / the library stays empty** — the source path is not
  readable by the service user (see
  [Ingesting sources under your home](#ingesting-sources-under-your-home-permissions)).
  An inaccessible path is reported as `failed` with the OS error — the CLI
  prints `failed=N`, the UI lists `FAILED: <path>: <error>` — so an empty
  ingest means the path was unreachable. Confirm with the
  `curl .../api/fs?path=...` probe above.
- **A config edit had no effect** — only some settings hot-reload (the Watch
  toggle does). `rc-service vellum restart` (OpenRC) or
  `systemctl restart vellum` (systemd) after editing `config.yaml`.
- **A model server is "up" but answers are wrong** — a stale server from an
  earlier manual run may already hold the port; the launcher reuses a healthy
  server instead of replacing it. `ss -tlnp | grep -E '808[12]'` and stop the
  stray process, then restart the service.
