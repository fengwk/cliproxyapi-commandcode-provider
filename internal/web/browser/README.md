# Browser verification

Run the production assets and CSP against fake loopback management endpoints.
Playwright is an external test tool, not a plugin dependency:

```bash
NODE_PATH=/path/to/external/node_modules CHROMIUM_PATH=/usr/bin/chromium \
  node internal/web/browser/polish.cjs /tmp/commandcode-ui-evidence
```

Checks cover light/white/dark at 1280/768/390px, compact 34/28px controls,
44px coarse-pointer targets, real dropdown keyboard/pointer operation,
label focus, parent-visible iframe positioning, repeated credential reload,
mode PATCH followed by GET confirmation, failed-write draft preservation,
inert labels, production CSP, and absence of credentials in URLs or storage.
Screenshots and results go only to the supplied directory outside the repository.
