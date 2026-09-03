# KB embed Python environment

`embed.in` contains the direct runtime roots. Regenerate and install the resolved
set with the same Python minor version used by the services:

```bash
uv pip compile requirements/embed.in -o requirements/embed.lock --python-version 3.13
uv venv --python /usr/bin/python3 /opt/kb/venv-embed
uv pip sync --python /opt/kb/venv-embed/bin/python requirements/embed.lock
```

The venv must have `include-system-site-packages = false`. Verify imports with
`python -s`; no path may resolve under `~/.local`.
