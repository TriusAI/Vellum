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
