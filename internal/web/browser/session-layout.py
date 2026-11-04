#!/usr/bin/env python3
"""Production-asset login/layout regression; synthetic credentials, loopback only."""

import argparse
import json
import re
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from playwright.sync_api import expect, sync_playwright


WEB = Path(__file__).resolve().parent.parent
SOURCE = (WEB / "ui.js").read_text()
PLUGIN = re.search(r'var PLUGIN_ID = "([^"]+)"', SOURCE).group(1)
HAS_MODE = 'id="import-mode"' in (WEB / "ui.html").read_text()
CSP = re.search(r'"(default-src [^"\n]+)"', (WEB.parent / "provider/management.go").read_text()).group(1)
BASE = "/v0/management/plugins/" + PLUGIN
RESOURCE = "/v0/resource/plugins/" + PLUGIN
KEY = "synthetic-browser-management-key"
WINDOWS = re.findall(r'window: "([^"]+)"', SOURCE.split("var QUOTA_WINDOWS = [")[1].split("];")[0])
STATE = {"reads": 0, "models": [], "mode": "cli", "auth_status": 200}


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def reply(self, body, mime="application/json", status=200, csp=False):
        data = json.dumps(body).encode() if mime == "application/json" else body
        self.send_response(status)
        self.send_header("Content-Type", mime)
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        if csp:
            self.send_header("Content-Security-Policy", CSP)
        self.end_headers()
        self.wfile.write(data)

    def handle_api(self):
        assert KEY not in self.path
        if self.path == "/fixture":
            html = """<!doctype html><html><head><meta name="viewport" content="width=device-width,initial-scale=1">
            <style>html,body{margin:0;height:100%;--header-height:64px}iframe{width:100%;height:100dvh;border:0}
            .floating-actions{position:fixed;z-index:9999;top:24px;right:24px;width:178px;height:52px;
            border-radius:16px;background:#333;color:white;display:flex;align-items:center;justify-content:space-around}</style>
            </head><body><iframe src="__RESOURCE__/ui"></iframe><div class="floating-actions" aria-label="CPA原生悬浮工具">
            <button>↻</button><button>◎</button><button>☼</button><button>↪</button></div></body></html>""".replace("__RESOURCE__", RESOURCE)
            return self.reply(html.encode(), "text/html")
        assets = {RESOURCE + "/ui": ("ui.html", "text/html"),
                  RESOURCE + "/ui.js": ("ui.js", "text/javascript"),
                  RESOURCE + "/ui.css": ("ui.css", "text/css")}
        if self.path in assets:
            filename, mime = assets[self.path]
            return self.reply((WEB / filename).read_bytes(), mime, csp=True)
        if self.path == "/favicon.ico":
            return self.reply(b"", "image/x-icon")
        if self.headers.get("Authorization") != "Bearer " + KEY:
            return self.reply({}, status=401)
        payload = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or b"{}")
        if self.path == BASE + "/keys":
            if self.command == "GET":
                STATE["reads"] += 1
                return self.reply({"files": [{"name": "synthetic-fixture.json", "label": "合成测试凭据",
                    "auth_index": "fixture-1", "mode": STATE["mode"], "mode_source": "auth",
                    "status": "active", "success": 8, "failed": 1}]}, status=STATE["auth_status"])
            if self.command == "PATCH":
                STATE["mode"] = payload["mode"]
                return self.reply({"auth_index": "fixture-1", "mode": STATE["mode"], "mode_source": "auth"})
            return self.reply({"imported": len(payload["keys"]), "skipped": 0, "failed": 0})
        if self.path == BASE + "/settings":
            return self.reply({"mode": "cli", "default_mode": "cli", "cli_version": "1.79.2",
                               "catalog": {"count": 87, "stale": False}, "manual-models": STATE["models"]})
        if self.path == BASE + "/config":
            if self.command == "PATCH":
                STATE["models"] = payload["manual-models"]
            return self.reply({"manual-models": STATE["models"]})
        if self.path == BASE + "/validate":
            return self.reply({"valid": True})
        if self.path == BASE + "/quota":
            return self.reply({"subscription": {"plan": "Synthetic"}, "summary": {"total_credits": 100},
                "groups": [{"buckets": [{"window": window, "remainingFraction": 0.75,
                    "resetTime": "2026-12-01T00:00:00Z", "description": "Synthetic API window"} for window in WINDOWS]}]})
        return self.reply({}, status=404)

    do_GET = do_POST = do_PATCH = handle_api


def ready(frame):
    expect(frame.locator("#app-shell" if HAS_MODE else "#app-view")).to_be_visible()
    expect(frame.locator("#reload-btn" if HAS_MODE else "#refresh-btn")).to_be_enabled()


def check_layout(page, frame):
    floating = page.locator(".floating-actions").bounding_box()
    pane = "#main-content" if HAS_MODE else "#content"
    viewport = frame.locator(pane).bounding_box()
    assert viewport["y"] >= floating["y"] + floating["height"], (viewport, floating)
    frame_box = page.locator("iframe").bounding_box()
    controls = frame.locator("html").evaluate("""(html,pane) => {
      const viewport=document.querySelector(pane).getBoundingClientRect();
      return Array.from(document.querySelectorAll('button,input,textarea,select'))
        .filter(el=>!el.disabled&&!el.closest('[inert]')&&el.getClientRects().length&&getComputedStyle(el).visibility!=='hidden')
        .map(el=>{const r=el.getBoundingClientRect(),inside=!!el.closest(pane);
          const y=Math.max(r.top,inside?viewport.top:0), bottom=Math.min(r.bottom,inside?viewport.bottom:innerHeight);
          return {id:el.id,x:r.x,y,width:r.width,height:bottom-y};}).filter(r=>r.width>0&&r.height>0);
    }""", pane)
    for box in controls:
        box["x"] += frame_box["x"]
        box["y"] += frame_box["y"]
        assert not (box["x"] < floating["x"] + floating["width"] and box["x"] + box["width"] > floating["x"]
                    and box["y"] < floating["y"] + floating["height"] and box["y"] + box["height"] > floating["y"]), box
    assert frame.locator("html").evaluate("el=>el.scrollWidth<=innerWidth")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--report-dir", default=None)
    args = parser.parse_args()
    report = Path(args.report_dir or tempfile.mkdtemp(prefix=PLUGIN + "-browser-"))
    report.mkdir(parents=True, exist_ok=True)
    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    origin = f"http://127.0.0.1:{server.server_port}"
    try:
        with sync_playwright() as pw:
            browser = pw.chromium.launch(executable_path="/usr/bin/chromium", headless=True)
            page = browser.new_page(viewport={"width": 1440, "height": 1000}, reduced_motion="reduce")
            errors = []
            page.on("pageerror", lambda error: errors.append(str(error)))
            page.on("console", lambda message: errors.append(message.text) if message.type == "error" and "security" in message.text.lower() else None)
            page.goto(origin + RESOURCE + "/ui")
            page.locator("#mgmt-key" if HAS_MODE else "#login-key").fill(KEY)
            page.locator("#login-submit" if HAS_MODE else "#login-btn").click()
            ready(page)
            reads = STATE["reads"]
            page.reload()
            ready(page)
            assert STATE["reads"] == reads + 1, "remembered login verifies exactly once"
            page.locator("#keys-import" if HAS_MODE else "#import-open").click()
            page.locator("#keys").fill("synthetic-upstream-key")
            page.locator("#import-btn").click()
            ready(page)
            assert page.locator("#keys").input_value() == ""
            if not HAS_MODE:
                page.locator("#import-cancel").click()
            if HAS_MODE:
                page.locator("select[data-role='mode-select']").select_option("provider", force=True)
                page.locator("[data-action='save-mode']").click()
                ready(page)
                expect(page.locator("#status")).to_contain_text("确认生效")
            page.goto(origin + "/fixture")
            frame = page.frame_locator("iframe")
            ready(frame)
            for width, height in [(1440, 1000), (768, 1024), (390, 844)]:
                page.set_viewport_size({"width": width, "height": height})
                for theme in ["light", "white", "dark"]:
                    page.evaluate("t=>document.documentElement.setAttribute('data-theme',t)", theme)
                    frame.locator("[data-tab='keys']").click()
                    ready(frame)
                    check_layout(page, frame)
                    pane = frame.locator("#main-content" if HAS_MODE else "#content")
                    pane.evaluate("el=>el.scrollTop=el.scrollHeight")
                    check_layout(page, frame)
                    pane.evaluate("el=>el.scrollTop=0")
                    frame.locator("#keys-import" if HAS_MODE else "#import-open").click()
                    check_layout(page, frame)
                    frame.locator("#import-cancel").click()
                    frame.locator("[data-action='quota']").click()
                    expect(frame.locator("#quota-panel")).to_be_visible()
                    assert frame.locator("#quota-body tr").count() == len(WINDOWS)
                    check_layout(page, frame)
                    frame.locator("#quota-close").click()
                    frame.locator(".table-wrap").evaluate_all("els=>els.forEach(el=>el.scrollLeft=0)")
                    page.screenshot(path=str(report / f"{width}-{theme}.png"))
            frame.locator("[data-tab='settings']").click()
            ready(frame)
            frame.locator("#logout-btn").click()
            expect(frame.locator("#login-view")).to_be_visible()
            assert not page.evaluate("Object.keys(localStorage).some(k=>k.includes('management-key'))")
            assert not errors, errors
            browser.close()
    finally:
        server.shutdown()
        server.server_close()
    (report / "result.json").write_text(json.dumps({"plugin": PLUGIN, "passed": True, "viewports": [1440, 768, 390]}))
    print(f"PASS {PLUGIN}: login/restore/import/quota/logout and 3 themes × 3 embedded viewports; {report}")


if __name__ == "__main__":
    main()
