# Browser verification

Run the real production assets and exact CSP against synthetic loopback endpoints.
Python Playwright is an external test tool, not a plugin runtime dependency:

```bash
python3 internal/web/browser/session-layout.py --report-dir /tmp/commandcode-ui-evidence
```

Checks cover independent login, exactly one remembered-session validation,
successful import, explicit mode PATCH followed by GET confirmation, quota and
logout. Warm/white/dark at 1440/768/390px check the floating-toolbar safety line,
scroll clipping, modal controls and no page-level horizontal overflow.
`ui.test.cjs` preserves failed-write draft, nullable quota, model and select
regressions and loads `login.test.cjs` for namespace/fallback/clear checks.

If Playwright is not already installed, keep its environment outside the repo:

```bash
uv venv /tmp/provider-ui-browser-env
uv pip install --python /tmp/provider-ui-browser-env/bin/python playwright
/tmp/provider-ui-browser-env/bin/python internal/web/browser/session-layout.py \
  --report-dir /tmp/commandcode-ui-evidence
```

Chromium is expected at `/usr/bin/chromium`. No live CPA, real credential or
production configuration is read. Screenshots/results go only to the supplied
directory. The old `polish.cjs` describes the retired v0.2 single-page DOM and is
not the current test entry.
