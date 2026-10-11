"use strict";

/*
 * Node built-in tests for CommandCode dependency-free UI helpers and flows.
 * Run with: node --test internal/web/ui.test.cjs
 *
 * ui.js is loaded as a CommonJS module (no DOM in Node), and DOM flows
 * are tested via isolated vm contexts against minimal mock DOM and fetch.
 */

const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

const ui = require("./ui.js");

const DIR = __dirname;
const readAsset = (name) => fs.readFileSync(path.join(DIR, name), "utf8");

/* ---------------------------------------------------------------- pure helpers */

test("deriveEndpoints keeps a reverse-proxy prefix before /v0", () => {
  const proxied = ui.deriveEndpoints({
    pathname: "/gateway/cliproxy/v0/resource/plugins/cliproxyapi-commandcode-provider/ui"
  });
  assert.equal(proxied.prefix, "/gateway/cliproxy");
  assert.equal(
    proxied.keysUrl,
    "/gateway/cliproxy/v0/management/plugins/cliproxyapi-commandcode-provider/keys"
  );
  assert.equal(
    proxied.settingsUrl,
    "/gateway/cliproxy/v0/management/plugins/cliproxyapi-commandcode-provider/settings"
  );
  assert.equal(
    proxied.validateUrl,
    "/gateway/cliproxy/v0/management/plugins/cliproxyapi-commandcode-provider/validate"
  );
  assert.equal(
    proxied.configUrl,
    "/gateway/cliproxy/v0/management/plugins/cliproxyapi-commandcode-provider/config"
  );
  assert.equal(
    proxied.quotaUrl,
    "/gateway/cliproxy/v0/management/plugins/cliproxyapi-commandcode-provider/quota"
  );
  assert.equal(proxied.credentialsUrl, "/gateway/cliproxy/v8/management/credentials");
});

test("deriveEndpoints works at the origin root", () => {
  const root = ui.deriveEndpoints({
    pathname: "/v0/resource/plugins/cliproxyapi-commandcode-provider/ui"
  });
  assert.equal(root.prefix, "");
  assert.equal(root.keysUrl, "/v0/management/plugins/cliproxyapi-commandcode-provider/keys");
  assert.equal(root.settingsUrl, "/v0/management/plugins/cliproxyapi-commandcode-provider/settings");
  assert.equal(root.validateUrl, "/v0/management/plugins/cliproxyapi-commandcode-provider/validate");
  assert.equal(root.configUrl, "/v0/management/plugins/cliproxyapi-commandcode-provider/config");
  assert.equal(root.quotaUrl, "/v0/management/plugins/cliproxyapi-commandcode-provider/quota");
  assert.equal(root.credentialsUrl, "/v8/management/credentials");
});

test("deriveEndpoints falls back to origin-relative paths for unknown pages", () => {
  const fallback = ui.deriveEndpoints({ pathname: "/some/stub/ui.html" });
  assert.equal(fallback.keysUrl, "/v0/management/plugins/cliproxyapi-commandcode-provider/keys");
  assert.equal(fallback.credentialsUrl, "/v8/management/credentials");
});

test("plaintext HTTP is only allowed on loopback", () => {
  assert.equal(ui.isSecureUiContext({ protocol: "https:", hostname: "cpa.example.com" }), true);
  assert.equal(ui.isSecureUiContext({ protocol: "http:", hostname: "localhost" }), true);
  assert.equal(ui.isSecureUiContext({ protocol: "http:", hostname: "127.0.0.1" }), true);
  assert.equal(ui.isSecureUiContext({ protocol: "http:", hostname: "[::1]" }), true);
  assert.equal(ui.isSecureUiContext({ protocol: "http:", hostname: "::1" }), true);
  assert.equal(ui.isSecureUiContext({ protocol: "http:", hostname: "cpa.example.com" }), false);
  assert.equal(ui.isSecureUiContext({ protocol: "http:", hostname: "192.168.1.10" }), false);
  assert.equal(ui.isSecureUiContext({ protocol: "file:", hostname: "" }), false);
});

test("parseKeys trims, drops blanks, dedupes and reports counts", () => {
  const parsed = ui.parseKeys("  key-one  \n\nkey-two\r\nkey-one\n   \nkey-two\r");
  assert.deepEqual(parsed.keys, ["key-one", "key-two"]);
  assert.equal(parsed.error, null);
  assert.deepEqual(parsed.counts, {
    lines: 7,
    empty: 3,
    duplicates: 2,
    invalid: 0,
    valid: 2
  });
});

test("parseKeys rejects interior whitespace, control characters and over-long keys", () => {
  const parsed = ui.parseKeys("good-key\nbad key\ntab\tkey\n" + "x".repeat(4097) + "\nok-key");
  assert.deepEqual(parsed.keys, ["good-key", "ok-key"]);
  assert.deepEqual(parsed.invalidLines, [
    { line: 2, reason: "whitespace" },
    { line: 3, reason: "control" },
    { line: 4, reason: "too-long" }
  ]);
  assert.equal(parsed.counts.invalid, 3);
});

test("parseKeys enforces the 100-key ceiling without returning partial input", () => {
  const lines = [];
  for (let i = 0; i < 101; i++) {
    lines.push("key-" + i);
  }
  const parsed = ui.parseKeys(lines.join("\n"));
  assert.equal(parsed.error.code, "too-many");
  assert.deepEqual(parsed.keys, []);
  assert.equal(parsed.counts.valid, 101);
});

test("validateLabel keeps text but rejects control characters and over-long labels", () => {
  assert.deepEqual(ui.validateLabel("  batch 1  "), { ok: true, value: "batch 1", reason: "" });
  assert.equal(ui.validateLabel("line\nbreak").ok, false);
  assert.equal(ui.validateLabel("x".repeat(129)).reason, "too-long");
  assert.deepEqual(ui.validateLabel(undefined), { ok: true, value: "", reason: "" });
});

test("buildImportPayload measures the UTF-8 body against the 64 KiB budget", () => {
  const small = ui.buildImportPayload(["key-one"], "label");
  assert.equal(small.ok, true);
  assert.deepEqual(JSON.parse(small.body), { keys: ["key-one"], label: "label" });

  const multibyte = ui.utf8ByteLength("密钥");
  assert.equal(multibyte, 6);
  assert.equal(ui.utf8ByteLength("abc"), 3);

  const huge = ui.buildImportPayload(
    ["k".repeat(4096)].concat(Array.from({ length: 17 }, (_, i) => "j" + i + "k".repeat(4096)))
  );
  assert.equal(huge.ok, false);
  assert.ok(huge.bytes > ui.MAX_BODY_BYTES);
});

test("normalizeFileEntries renders hostile content as inert text and flags unsafe names", () => {
  const rows = ui.normalizeFileEntries([
    {
      name: 'evil\">\u0007<img src=x onerror=alert(1)>.json',
      label: "<script>alert(1)</script>",
      status: "active",
      disabled: false,
      unavailable: true,
      success: "3",
      failed: -2
    },
    { name: "../escape.json", label: "traversal" },
    { name: "sub/dir.json" },
    { name: "commandcode-1.json", label: "batch", success: 4, failed: 1 },
    "not-an-object",
    null
  ]);

  assert.equal(rows.length, 4);
  assert.ok(!rows[0].name.includes("\u0007"));
  assert.ok(rows[0].name.includes("<img src=x onerror=alert(1)>"));
  assert.ok(rows[0].label.includes("<script>"));
  assert.equal(rows[0].unavailable, true);
  assert.equal(rows[0].success, 3);
  assert.equal(rows[0].failed, 0);
  assert.equal(rows[0].deletable, false);

  assert.equal(rows[1].deletable, false);
  assert.equal(rows[2].deletable, false);
  assert.equal(rows[3].deletable, true);
  assert.equal(rows[3].success, 4);
  assert.equal(rows[3].failed, 1);
});

test("isSafeFileName and toCellText guard delete input", () => {
  assert.equal(ui.isSafeFileName("commandcode-1.json"), true);
  assert.equal(ui.isSafeFileName("  spaced name.json  "), true);
  assert.equal(ui.isSafeFileName("../escape.json"), false);
  assert.equal(ui.isSafeFileName("a/b.json"), false);
  assert.equal(ui.isSafeFileName("a\\b.json"), false);
  assert.equal(ui.isSafeFileName("bad\u0000name"), false);
  assert.equal(ui.isSafeFileName(""), false);
  assert.equal(ui.isSafeFileName(".."), false);
  assert.equal(ui.isSafeFileName(42), false);

  assert.equal(ui.toCellText("a\u0000b\nc"), "abc");
  assert.equal(ui.toCellText({ html: "<b>x</b>" }), "");
  assert.equal(ui.toCellText("x".repeat(500)).length, 201);
});

test("formatImportResult reports success and partial imports without server strings", () => {
  const ok = ui.formatImportResult(200, { imported: 2, skipped: 1, failed: 0 }, 3);
  assert.equal(ok.ok, true);
  assert.equal(ok.level, "ok");
  assert.match(ok.summary, /成功 2/);

  const partial = ui.formatImportResult(207, { imported: 1, skipped: 0, failed: 1 }, 2);
  assert.equal(partial.ok, true);
  assert.equal(partial.level, "warn");
  assert.match(partial.summary, /部分导入完成/);

  const failed = ui.formatImportResult(500, { error: "<img src=x onerror=alert(1)>" });
  assert.equal(failed.ok, false);
  assert.equal(failed.level, "error");
  assert.ok(!failed.summary.includes("img"));
  assert.ok(!failed.summary.includes("alert"));
});

test("formatImportResult accepts only trusted counts summing to the submitted total", () => {
  const check = (body, expected) => ui.formatImportResult(200, body, expected);

  assert.equal(check({}, 0).ok, false);
  assert.equal(check(null, 0).ok, false);
  assert.equal(check({ imported: 1, skipped: 0 }, 1).ok, false);
  assert.equal(check({ imported: "1", skipped: 0, failed: 0 }, 1).ok, false);
  assert.equal(check({ imported: -1, skipped: 2, failed: 0 }, 1).ok, false);
  assert.equal(check({ imported: 1.5, skipped: 0, failed: 0 }, 1).ok, false);
  assert.equal(check({ imported: NaN, skipped: 0, failed: 0 }, 1).ok, false);
  assert.equal(check({ imported: 1, skipped: 0, failed: 0 }, 3).ok, false);
  assert.match(check({}, 0).summary, /无法确认导入结果/);

  const confirmed = check({ imported: 2, skipped: 1, failed: 0 }, 3);
  assert.equal(confirmed.ok, true);
  assert.equal(confirmed.level, "ok");
  assert.equal(confirmed.failed, 0);
  assert.match(confirmed.summary, /成功 2/);
});

test("describeHttpFailure returns fixed safe messages", () => {
  assert.match(ui.describeHttpFailure(401), /401/);
  assert.match(ui.describeHttpFailure(403), /远程管理/);
  assert.match(ui.describeHttpFailure(404), /版本/);
  assert.match(ui.describeHttpFailure(500), /服务端错误/);
  assert.ok(!ui.describeHttpFailure(418).includes("<"));
});

test("buildManagementRequest keeps the key in the Authorization header only", () => {
  const get = ui.buildManagementRequest("mgmt-secret", { method: "GET" });
  assert.deepEqual(get.headers, { Accept: "application/json", Authorization: "Bearer mgmt-secret" });
  assert.equal(get.credentials, "omit");
  assert.equal(get.cache, "no-store");
  assert.equal(get.body, undefined);

  const post = ui.buildManagementRequest("mgmt-secret", { method: "POST", body: '{"keys":["upstream"]}' });
  assert.equal(post.headers["Content-Type"], "application/json");
  assert.ok(!post.body.includes("mgmt-secret"));

  const anon = ui.buildManagementRequest("", { method: "GET" });
  assert.equal(Object.prototype.hasOwnProperty.call(anon.headers, "Authorization"), false);
});

/* ---------------------------------------------------------------- quota helpers */

test("isSafeAuthIndex and buildQuotaPayload bound the credential index", () => {
  assert.equal(ui.isSafeAuthIndex("idx-1"), true);
  assert.equal(ui.isSafeAuthIndex("  idx-1  "), true);
  assert.equal(ui.isSafeAuthIndex(""), false);
  assert.equal(ui.isSafeAuthIndex("   "), false);
  assert.equal(ui.isSafeAuthIndex("a b"), false);
  assert.equal(ui.isSafeAuthIndex("bad\u0000index"), false);
  assert.equal(ui.isSafeAuthIndex("x".repeat(ui.MAX_AUTH_INDEX_LENGTH + 1)), false);
  assert.equal(ui.isSafeAuthIndex(42), false);

  const body = ui.buildQuotaPayload("idx-1");
  assert.equal(body.ok, true);
  assert.deepEqual(JSON.parse(body.body), { auth_index: "idx-1" });

  const empty = ui.buildQuotaPayload("a b");
  assert.equal(empty.ok, false);
  assert.equal(empty.body, "");
});

test("isValidIsoTimestamp and formatQuotaReset accept only strict RFC3339", () => {
  assert.equal(ui.isValidIsoTimestamp("2026-10-12T00:00:00Z"), true);
  assert.equal(ui.isValidIsoTimestamp("2026-10-12T00:00:00.500+08:00"), true);
  assert.equal(ui.isValidIsoTimestamp("2026-10-12 00:00:00"), false);
  assert.equal(ui.isValidIsoTimestamp("not-a-date"), false);
  assert.equal(ui.isValidIsoTimestamp("2026-13-99T00:00:00Z"), false);
  assert.equal(ui.isValidIsoTimestamp(1234), false);

  assert.equal(ui.formatQuotaReset("2026-10-12T00:00:00Z"), "2026-10-12 00:00 UTC");
  assert.equal(ui.formatQuotaReset("2026-10-12T09:05:00Z"), "2026-10-12 09:05 UTC");
  assert.equal(ui.formatQuotaReset("not-a-date"), "");
});

test("parseQuotaResponse parses CommandCode five-hour and weekly windows and credits summary", () => {
  const parsed = ui.parseQuotaResponse({
    subscription: { plan: "CommandCode Pro", status: "active", period: "monthly" },
    summary: {
      monthly_credits: 100,
      purchased_credits: 50,
      free_credits: 0,
      total_credits: 150
    },
    groups: [
      {
        displayName: "CommandCode Rate Limits",
        buckets: [
          { window: "five-hour", remainingFraction: 0.85, resetTime: "2026-10-12T15:00:00Z" },
          { window: "weekly", remainingFraction: 0.2, resetTime: "2026-10-19T00:00:00Z" }
        ]
      },
      // Unknown window is ignored safely
      { buckets: [{ window: "bonus", remainingFraction: 0.5, resetTime: "2026-10-20T00:00:00Z" }] }
    ]
  });

  assert.equal(parsed.ok, true);
  assert.equal(parsed.subscription.plan, "CommandCode Pro");
  assert.equal(parsed.subscription.status, "active");
  assert.deepEqual(parsed.summary, {
    totalCredits: 150,
    monthlyCredits: 100,
    purchasedCredits: 50,
    freeCredits: 0
  });
  assert.equal(parsed.buckets.length, 2);
  assert.deepEqual(
    parsed.buckets.map((b) => [b.window, b.label, b.remainingPercent, b.usedPercent]),
    [
      ["five-hour", "5小时窗口", 85, 15],
      ["weekly", "每周窗口", 20, 80]
    ]
  );
  assert.equal(parsed.buckets[0].resetTime, "2026-10-12 15:00 UTC");
});

test("parseQuotaResponse does NOT force 3 windows: single window is accepted without fabricating missing as 0", () => {
  const single = ui.parseQuotaResponse({
    subscription: { plan: "CommandCode Standard" },
    groups: [
      {
        buckets: [
          { window: "five-hour", remainingFraction: 0.75, resetTime: "2026-10-12T15:00:00Z" }
        ]
      }
    ]
  });
  assert.equal(single.ok, true);
  assert.equal(single.buckets.length, 1);
  assert.equal(single.buckets[0].window, "five-hour");
  assert.equal(single.buckets[0].remainingPercent, 75);
  // Missing weekly window is NOT fabricated as 0
  assert.equal(single.buckets.some((b) => b.window === "weekly"), false);
});

test("parseQuotaResponse accepts credits summary when groups buckets are empty", () => {
  const summaryOnly = ui.parseQuotaResponse({
    subscription: { plan: "CommandCode Free" },
    summary: { total_credits: 20, free_credits: 20 },
    groups: []
  });
  assert.equal(summaryOnly.ok, true);
  assert.equal(summaryOnly.summary.totalCredits, 20);
  assert.equal(summaryOnly.summary.freeCredits, 20);
  assert.equal(summaryOnly.buckets.length, 0);
});

test("parseQuotaResponse rejects malformed or duplicate data", () => {
  assert.equal(ui.parseQuotaResponse(null).ok, false);
  assert.equal(ui.parseQuotaResponse({}).ok, false);

  // Duplicate window is ambiguous
  const duplicate = {
    subscription: { plan: "Pro" },
    groups: [
      {
        buckets: [
          { window: "five-hour", remainingFraction: 0.5, resetTime: "2026-10-12T15:00:00Z" },
          { window: "five-hour", remainingFraction: 0.4, resetTime: "2026-10-12T15:00:00Z" }
        ]
      }
    ]
  };
  assert.equal(ui.parseQuotaResponse(duplicate).ok, false);

  // Remaining fraction out of bounds
  const badFraction = {
    subscription: { plan: "Pro" },
    groups: [
      {
        buckets: [
          { window: "five-hour", remainingFraction: 1.5, resetTime: "2026-10-12T15:00:00Z" }
        ]
      }
    ]
  };
  assert.equal(ui.parseQuotaResponse(badFraction).ok, false);

  // Invalid reset time
  const badReset = {
    subscription: { plan: "Pro" },
    groups: [
      {
        buckets: [
          { window: "five-hour", remainingFraction: 0.5, resetTime: "tomorrow" }
        ]
      }
    ]
  };
  assert.equal(ui.parseQuotaResponse(badReset).ok, false);
});

test("describeQuotaFailure returns fixed messages without echoing server text", () => {
  assert.match(ui.describeQuotaFailure(400), /400/);
  assert.match(ui.describeQuotaFailure(404), /版本/);
  assert.match(ui.describeQuotaFailure(501), /不支持/);
  assert.match(ui.describeQuotaFailure(502), /服务端错误/);
  assert.ok(!ui.describeQuotaFailure(418).includes("<"));
});

/* ---------------------------------------------------------------- manual models */

test("manual models normalize namespace, strip commandcode/ prefix and validate protocols", () => {
  assert.deepEqual(
    ui.normalizeManualModels([
      { id: " commandcode/claude-3-7-sonnet " },
      { id: "custom-chat", protocol: "openai" }
    ]),
    [{ id: "claude-3-7-sonnet" }, { id: "custom-chat", protocol: "openai" }]
  );
  assert.deepEqual(ui.normalizeManualModels([]), []);
});

test("manual models reject duplicate ids, invalid protocols, whitespace and overlong ids", () => {
  for (const models of [
    null,
    [{ id: "model with spaces" }],
    [{ id: "model\u0085control" }],
    [{ id: "model-1" }, { id: "commandcode/model-1" }],
    [{ id: "commandcode/commandcode/model-1" }],
    [{ id: "novel", protocol: "unsupported" }],
    [{ id: "x".repeat(201) }],
    Array.from({ length: 101 }, (_, i) => ({ id: "model-" + i }))
  ]) {
    assert.throws(() => ui.normalizeManualModels(models));
  }
});

/* ---------------------------------------------------------------- security & CSP checks */

test("ui.js contains no storage, HTML-injection, eval or external-request primitives", () => {
  const source = readAsset("ui.js");
  for (const forbidden of [
    "localStorage",
    "sessionStorage",
    "document.cookie",
    "indexedDB",
    "innerHTML",
    "outerHTML",
    "insertAdjacentHTML",
    "document.write",
    "eval(",
    "new Function",
    "console."
  ]) {
    assert.equal(source.includes(forbidden), false, "ui.js must not reference " + forbidden);
  }
  assert.equal(/https?:\/\//.test(source), false, "ui.js must not contain absolute URLs");
  assert.equal(source.includes('credentials: "omit"'), true);
  assert.equal(source.includes('cache: "no-store"'), true);
});

test("ui.html is CSP-safe and wires the embedded assets", () => {
  const html = readAsset("ui.html");
  assert.equal(/<style[\s>]/i.test(html), false, "no inline <style> block");
  assert.equal(/\sstyle\s*=/i.test(html), false, "no inline style attribute");
  assert.equal(/\son[a-z]+\s*=/i.test(html), false, "no inline event handler");
  assert.equal(html.includes('href="./ui.css"'), true);
  assert.equal(html.includes('src="./ui.js" defer'), true);
  assert.equal(/<script(?![^>]*\ssrc=)/i.test(html), false, "scripts must have a src (no inline JS)");
  assert.match(html, /type="password"[^>]*autocomplete="off"/);
  assert.match(html, /rel="noopener noreferrer"/);
  assert.ok(html.includes("https://github.com/fengwk/cliproxyapi-commandcode-provider"));
  assert.ok(html.includes('aria-live="polite"'));
  assert.ok(html.includes("routing.session-affinity: true"));
});

test("ui.html excludes sensitive controls from native form serialization", () => {
  const html = readAsset("ui.html");
  for (const id of ["mgmt-key", "keys"]) {
    const controls = html.match(new RegExp(`<(?:input|textarea)\\b[^>]*\\bid=["']${id}["'][^>]*>`, "gi"));
    assert.equal(controls && controls.length, 1, `expected one sensitive control: ${id}`);
    assert.doesNotMatch(controls[0], /\sname(?:\s|=|\/?>)/i, `${id} must not have a name attribute`);
  }
});

test("ui.html shows CommandCode Provider display name and PluginID, without OpenCode leftovers", () => {
  const html = readAsset("ui.html");
  assert.ok(html.includes("<title>CommandCode Provider · 密钥与配额管理</title>"));
  assert.ok(html.includes("<h1>CommandCode Provider</h1>"));
  assert.ok(html.includes("<code>cliproxyapi-commandcode-provider</code>"));
  assert.ok(html.includes("导入 CommandCode 密钥"));

  // Check no leftover OpenCode tokens
  assert.equal(html.includes("cliproxyapi-opencode-provider"), false);
  assert.equal(html.includes("OpenCode"), false);
  assert.equal(html.includes("opencode-go"), false);
});

test("all buttons use the shared .btn contract instead of type/table-specific styling", () => {
  const css = readAsset("ui.css");
  const html = readAsset("ui.html");
  const js = readAsset("ui.js");

  const rule = (selector) => {
    const pattern = new RegExp(
      "(?:^|\\n)[ \\t]*" + selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + "\\s*\\{([^}]*)\\}"
    );
    const match = css.match(pattern);
    assert.ok(match, "missing CSS rule: " + selector);
    return match[1];
  };

  const base = rule(".btn");
  for (const declaration of [
    "display: inline-flex",
    "gap: 8px",
    "padding: 10px 14px",
    "border: 1px solid transparent",
    "background-color: var(--bg-secondary)",
    "font-size: 16px",
    "font-weight: 600",
    "line-height: 1.5",
    "white-space: nowrap"
  ]) {
    assert.ok(base.includes(declaration), "`.btn` must declare " + declaration);
  }

  assert.match(rule(".btn-primary"), /background-color:\s*var\(--primary-color\)/);
  assert.match(rule(".btn-secondary"), /background-color:\s*var\(--bg-tertiary\)/);
  assert.match(rule(".btn-ghost"), /background-color:\s*transparent/);
  assert.match(rule(".btn-danger"), /background-color:\s*var\(--error-color\)/);
  assert.match(rule(".btn-sm"), /padding:\s*8px 10px/);
  assert.match(rule(".btn-sm"), /font-size:\s*14px/);
  assert.match(rule(".btn:disabled"), /opacity:\s*0\.6/);
  assert.match(rule(".btn:focus-visible"), /outline:\s*2px solid var\(--text-primary\)/);

  assert.doesNotMatch(css, /button\[type="submit"\]/);
  assert.doesNotMatch(css, /button\.danger/);
  assert.doesNotMatch(css, /\.manual-model-table td button/);

  assert.match(
    rule("a:focus-visible,\ninput:focus-visible,\ntextarea:focus-visible"),
    /outline:\s*2px solid var\(--primary-color\)/
  );

  const semantics = ["btn-primary", "btn-secondary", "btn-danger", "btn-ghost"];
  const staticButtons = html.match(/<button\b[^>]*>/g) || [];
  assert.equal(staticButtons.length, 6);
  for (const tag of staticButtons) {
    assert.match(tag, /class="btn [^"]+"/, "static button must carry .btn: " + tag);
    assert.equal(semantics.filter((variant) => tag.includes(variant)).length, 1, "one variant: " + tag);
  }

  // Dynamic buttons in JS wire the shared classes
  assert.match(js, /quotaButton\.className = "btn btn-secondary btn-sm";/);
  assert.equal((js.match(/className = "btn btn-danger btn-sm";/g) || []).length, 2);
});

/* ---------------------------------------------------------------- DOM Flows */

function matchesSelector(element, selector) {
  const spec = /^([a-zA-Z]+)?(?:\[([^\]=]+)(?:=['"]?([^'"\]]*)['"]?)?\])?$/.exec(selector.trim());
  if (!spec) return false;
  const tag = spec[1];
  const attr = spec[2];
  const value = spec[3];
  if (tag && element.tagName !== tag.toUpperCase()) return false;
  if (attr) {
    if (!Object.prototype.hasOwnProperty.call(element.attributes, attr)) return false;
    if (value !== undefined && element.attributes[attr] !== value) return false;
  }
  return true;
}

function createMockDom() {
  const registry = new Map();

  function makeElement(tag) {
    const el = {
      tagName: String(tag).toUpperCase(),
      children: [],
      attributes: {},
      handlers: {},
      textContent: "",
      className: "",
      value: "",
      disabled: false,
      hidden: false,
      max: 0,
      type: "",
      focus() {},
      appendChild(child) {
        this.children.push(child);
        child.parentNode = this;
        return child;
      },
      removeChild(child) {
        const index = this.children.indexOf(child);
        if (index >= 0) this.children.splice(index, 1);
        child.parentNode = null;
        return child;
      },
      setAttribute(name, value) {
        this.attributes[name] = String(value);
      },
      getAttribute(name) {
        return Object.prototype.hasOwnProperty.call(this.attributes, name) ? this.attributes[name] : null;
      },
      addEventListener(name, handler) {
        this.handlers[name] = handler;
      },
      querySelectorAll(selector) {
        const found = [];
        const walk = (node) => {
          for (const child of node.children) {
            if (matchesSelector(child, selector)) found.push(child);
            walk(child);
          }
        };
        walk(this);
        return found;
      },
      get firstChild() {
        return this.children.length ? this.children[0] : null;
      }
    };
    el.classList = {
      toggle(name, force) {
        const parts = el.className ? el.className.split(/\s+/).filter(Boolean) : [];
        const has = parts.indexOf(name) >= 0;
        const on = force === undefined ? !has : Boolean(force);
        if (on && !has) parts.push(name);
        if (!on && has) parts.splice(parts.indexOf(name), 1);
        el.className = parts.join(" ");
        return on;
      }
    };
    return el;
  }

  return {
    document: {
      readyState: "complete",
      addEventListener() {},
      createElement: makeElement,
      getElementById(id) {
        if (!registry.has(id)) {
          registry.set(id, makeElement("div"));
        }
        return registry.get(id);
      }
    }
  };
}

// Mount full UI against mock DOM and scripted fetch
async function mountUi(options) {
  const opts = options || {};
  const dom = createMockDom();
  const requests = [];
  const fetchImpl = async (url, init) => {
    requests.push({ url, init });
    if (opts.networkFailure) throw new Error("network failure");
    if (url.includes("/quota")) {
      const resp = opts.quotaResponse || { status: 200, body: {} };
      return { status: resp.status, ok: resp.status >= 200 && resp.status < 300, text: async () => JSON.stringify(resp.body), json: async () => resp.body };
    }
    if (url.includes("/settings")) {
      const resp = opts.settingsResponse || {
        status: 200,
        body: {
          mode: "cli",
          cli_version: "1.79.2",
          base_url: "",
          zdr: false,
          catalog: { count: 87, source: "cli-snapshot", stale: false },
          "manual-models": []
        }
      };
      return { status: resp.status, ok: resp.status >= 200 && resp.status < 300, text: async () => JSON.stringify(resp.body), json: async () => resp.body };
    }
    if (url.includes("/keys") && init.method === "POST") {
      const resp = opts.importResponse || { status: 200, body: { imported: 1, skipped: 0, failed: 0 } };
      return { status: resp.status, ok: resp.status >= 200 && resp.status < 300, text: async () => JSON.stringify(resp.body), json: async () => resp.body };
    }
    if (url.includes("/credentials") && init.method === "DELETE") {
      const resp = opts.deleteResponse || { status: 200, body: { ok: true } };
      return { status: resp.status, ok: resp.status >= 200 && resp.status < 300, text: async () => JSON.stringify(resp.body), json: async () => resp.body };
    }
    // Default GET keys
    const files = opts.files || [];
    return { status: 200, ok: true, text: async () => JSON.stringify({ files }), json: async () => ({ files }) };
  };

  vm.runInNewContext(readAsset("ui.js"), {
    document: dom.document,
    window: {
      location: opts.location || {
        protocol: "http:",
        hostname: "127.0.0.1",
        pathname: "/v0/resource/plugins/cliproxyapi-commandcode-provider/ui"
      },
      confirm: () => true,
      setTimeout,
      clearTimeout
    },
    AbortController,
    TextEncoder,
    fetch: fetchImpl
  });

  const get = (id) => dom.document.getElementById(id);
  const settle = async () => {
    for (let i = 0; i < 30 && get("reload-btn").disabled; i++) {
      await new Promise((resolve) => setImmediate(resolve));
    }
  };

  get("mgmt-key").value = "test-mgmt-key";
  get("auth-form").handlers.submit({ preventDefault() {} });
  await settle();

  return { get, requests, settle };
}

test("successful key import clears the keys textarea and reports count", async () => {
  const { get, requests, settle } = await mountUi({
    importResponse: { status: 200, body: { imported: 2, skipped: 0, failed: 0 } }
  });
  get("keys").value = "cc-key-1\ncc-key-2";
  get("label").value = "my-batch";
  get("import-form").handlers.submit({ preventDefault() {} });
  await settle();

  assert.equal(get("keys").value, "");
  assert.equal(get("label").value, "my-batch");
  assert.match(get("status").textContent, /导入成功/);
  assert.match(get("status").textContent, /成功 2/);
});

test("partial key import (207) retains keys in textarea for review", async () => {
  const { get, settle } = await mountUi({
    importResponse: { status: 207, body: { imported: 1, skipped: 0, failed: 1 } }
  });
  get("keys").value = "key-ok\nkey-fail";
  get("import-form").handlers.submit({ preventDefault() {} });
  await settle();

  assert.equal(get("keys").value, "key-ok\nkey-fail");
  assert.match(get("status").textContent, /部分导入完成/);
  assert.match(get("status").textContent, /失败 1/);
});

test("deleting a key invokes native credential delete API and clears quota", async () => {
  const { get, requests, settle } = await mountUi({
    files: [{ name: "commandcode-key-1.json", auth_index: "auth-1" }]
  });

  const deleteBtn = get("files-body").querySelectorAll("button[data-action='delete']")[0];
  assert.ok(deleteBtn);
  deleteBtn.handlers.click({ currentTarget: deleteBtn });
  await settle();

  const deleteReq = requests.find((r) => r.init.method === "DELETE");
  assert.ok(deleteReq);
  assert.equal(deleteReq.url, "/v8/management/credentials?name=commandcode-key-1.json");
  assert.match(get("status").textContent, /已删除/);
  assert.equal(get("quota-panel").hidden, true);
});

test("quota query displays subscription, credits summary, and five-hour/weekly buckets", async () => {
  const { get, requests, settle } = await mountUi({
    files: [{ name: "commandcode-key-1.json", auth_index: "auth-idx-1" }],
    quotaResponse: {
      status: 200,
      body: {
        subscription: { plan: "CommandCode Pro", status: "active" },
        summary: {
          monthly_credits: 100,
          purchased_credits: 50,
          free_credits: 0,
          total_credits: 150
        },
        groups: [
          {
            buckets: [
              { window: "five-hour", remainingFraction: 0.8, resetTime: "2026-10-12T15:00:00Z" },
              { window: "weekly", remainingFraction: 0.5, resetTime: "2026-10-19T00:00:00Z" }
            ]
          }
        ]
      }
    }
  });

  const quotaBtn = get("files-body").querySelectorAll("button[data-action='quota']")[0];
  assert.ok(quotaBtn);
  quotaBtn.handlers.click({ currentTarget: quotaBtn });
  await settle();

  const quotaReq = requests.find((r) => r.url.includes("/quota"));
  assert.ok(quotaReq);
  assert.deepEqual(JSON.parse(quotaReq.init.body), { auth_index: "auth-idx-1" });

  assert.equal(get("quota-panel").hidden, false);
  assert.match(get("quota-selection").textContent, /CommandCode Pro/);

  // Summary credits
  assert.equal(get("quota-summary").hidden, false);
  assert.equal(get("quota-credits-total").textContent, "150 credits");
  assert.equal(get("quota-credits-monthly").textContent, "100 credits");
  assert.equal(get("quota-credits-purchased").textContent, "50 credits");
  assert.equal(get("quota-credits-free").textContent, "0 credits");

  // Buckets table
  const rows = get("quota-body").children;
  assert.equal(rows.length, 2);
  assert.equal(rows[0].children[0].textContent, "5小时窗口");
  assert.equal(rows[0].children[1].textContent, "80%");
  assert.equal(rows[0].children[2].textContent, "20%");
  assert.equal(rows[0].children[4].textContent, "2026-10-12 15:00 UTC");

  assert.equal(rows[1].children[0].textContent, "每周窗口");
  assert.equal(rows[1].children[1].textContent, "50%");
  assert.equal(rows[1].children[2].textContent, "50%");
});

test("quota auth failure (401/403) clears credentials and hides quota panel", async () => {
  for (const status of [401, 403]) {
    const { get, settle } = await mountUi({
      files: [{ name: "key.json", auth_index: "auth-1" }],
      quotaResponse: { status, body: { error: "bad auth" } }
    });
    const quotaBtn = get("files-body").querySelectorAll("button[data-action='quota']")[0];
    quotaBtn.handlers.click({ currentTarget: quotaBtn });
    await settle();

    assert.equal(get("mgmt-key").value, "");
    assert.equal(get("quota-panel").hidden, true);
    assert.match(get("quota-status").textContent, new RegExp(String(status)));
  }
});

test("settings renders mode, CLI version, and stale catalog warning", async () => {
  const { get, settle } = await mountUi({
    settingsResponse: {
      status: 200,
      body: {
        mode: "cli",
        cli_version: "1.79.2",
        base_url: "https://api.commandcode.com",
        zdr: true,
        catalog: { count: 87, source: "cli-snapshot", stale: true },
        "manual-models": [{ id: "custom-claude", protocol: "claude" }]
      }
    }
  });

  assert.equal(get("setting-mode").textContent, "CLI 模式（默认）");
  assert.equal(get("setting-cli-version").textContent, "1.79.2");
  assert.equal(get("setting-base-url").textContent, "https://api.commandcode.com");
  assert.equal(get("setting-zdr").textContent, "已开启");
  assert.match(get("setting-catalog").textContent, /87 个模型/);
  assert.equal(get("setting-stale").hidden, false);
});

test("quota query with no buckets renders no-buckets hint without error", async () => {
  const { get, settle } = await mountUi({
    files: [{ name: "key.json", auth_index: "auth-1" }],
    quotaResponse: {
      status: 200,
      body: {
        subscription: { plan: "Free" },
        summary: { total_credits: 50 },
        groups: []
      }
    }
  });

  const quotaBtn = get("files-body").querySelectorAll("button[data-action='quota']")[0];
  quotaBtn.handlers.click({ currentTarget: quotaBtn });
  await settle();

  assert.equal(get("quota-panel").hidden, false);
  assert.equal(get("quota-table-wrap").hidden, true);
  assert.equal(get("quota-no-buckets").hidden, false);
});

test("refreshing keys list clears active quota panel to prevent mismatched quota state", async () => {
  const { get, settle } = await mountUi({
    files: [{ name: "key.json", auth_index: "auth-1" }],
    quotaResponse: {
      status: 200,
      body: {
        subscription: { plan: "Pro" },
        summary: { total_credits: 100 }
      }
    }
  });

  const quotaBtn = get("files-body").querySelectorAll("button[data-action='quota']")[0];
  quotaBtn.handlers.click({ currentTarget: quotaBtn });
  await settle();

  assert.equal(get("quota-panel").hidden, false);

  // Click refresh list
  get("reload-btn").handlers.click({});
  await settle();

  // Quota panel must now be hidden
  assert.equal(get("quota-panel").hidden, true);
});

test("off-loopback plaintext HTTP disables all management controls", async () => {
  const { get } = await mountUi({
    location: {
      protocol: "http:",
      hostname: "192.168.1.50",
      pathname: "/v0/resource/plugins/cliproxyapi-commandcode-provider/ui"
    }
  });

  assert.equal(get("insecure-warning").hidden, false);
  assert.equal(get("mgmt-key").disabled, true);
  assert.equal(get("keys").disabled, true);
  assert.equal(get("label").disabled, true);
  assert.equal(get("refresh-btn").disabled, true);
});

test("manual models draft adding, deletion and save lifecycle", async () => {
  const patchCalls = [];
  const validateCalls = [];
  let currentConfig = { "manual-models": [] };
  let currentSettings = {
    mode: "cli",
    cli_version: "1.79.2",
    "manual-models": []
  };

  const dom = createMockDom();
  const fetchImpl = async (url, init) => {
    if (url.includes("/validate")) {
      validateCalls.push(JSON.parse(init.body));
      return { status: 200, ok: true, json: async () => ({ valid: true }) };
    }
    if (url.includes("/config") && init.method === "PATCH") {
      const body = JSON.parse(init.body);
      patchCalls.push(body);
      currentConfig["manual-models"] = body["manual-models"];
      currentSettings["manual-models"] = body["manual-models"];
      return { status: 200, ok: true, json: async () => ({ ok: true }) };
    }
    if (url.includes("/config") && init.method === "GET") {
      return { status: 200, ok: true, json: async () => currentConfig };
    }
    if (url.includes("/settings")) {
      return { status: 200, ok: true, json: async () => currentSettings };
    }
    return { status: 200, ok: true, text: async () => JSON.stringify({ files: [] }), json: async () => ({ files: [] }) };
  };

  vm.runInNewContext(readAsset("ui.js"), {
    document: dom.document,
    window: {
      location: { protocol: "http:", hostname: "127.0.0.1", pathname: "/v0/resource/plugins/cliproxyapi-commandcode-provider/ui" },
      confirm: () => true,
      setTimeout,
      clearTimeout
    },
    AbortController,
    TextEncoder,
    fetch: fetchImpl
  });

  const get = (id) => dom.document.getElementById(id);
  const settle = async () => {
    for (let i = 0; i < 30 && get("reload-btn").disabled; i++) {
      await new Promise((resolve) => setImmediate(resolve));
    }
  };

  get("mgmt-key").value = "test-key";
  get("auth-form").handlers.submit({ preventDefault() {} });
  await settle();

  // Add a model
  get("manual-model-id").value = "commandcode/claude-3-7";
  get("manual-model-protocol").value = "claude";
  get("manual-model-add").handlers.click({});

  assert.equal(get("manual-model-save").disabled, false);
  const draftRows = get("manual-model-body").children;
  assert.equal(draftRows.length, 1);
  assert.equal(draftRows[0].children[0].textContent, "claude-3-7");
  assert.equal(draftRows[0].children[1].textContent, "claude");

  // Save manual model
  get("manual-model-save").handlers.click({});
  await settle();

  assert.equal(validateCalls.length, 1);
  assert.equal(patchCalls.length, 1);
  assert.deepEqual(patchCalls[0]["manual-models"], [{ id: "claude-3-7", protocol: "claude" }]);
  assert.match(get("manual-model-status").textContent, /已保存并确认/);
});

/* ---------------------------------------------------------------- theme bridge */

test("resolveAppliedTheme mirrors parent theme or falls back to system dark/white", () => {
  assert.equal(ui.resolveAppliedTheme("dark", false), "dark");
  assert.equal(ui.resolveAppliedTheme("white", true), "white");
  assert.equal(ui.resolveAppliedTheme("light", true), "light");
  assert.equal(ui.resolveAppliedTheme(null, true), "dark");
  assert.equal(ui.resolveAppliedTheme(null, false), "white");
  assert.equal(ui.resolveAppliedTheme(undefined, false), "white");
  assert.equal(ui.resolveAppliedTheme("custom-unknown", false), "white");
});

test("readParentTheme reads data-theme from same-origin parent", () => {
  assert.equal(ui.readParentTheme(null), null);
  const parentDark = { document: { documentElement: { getAttribute: (attr) => (attr === "data-theme" ? "dark" : null) } } };
  assert.equal(ui.readParentTheme(parentDark), "dark");
  const parentWhite = { document: { documentElement: { getAttribute: (attr) => (attr === "data-theme" ? "white" : null) } } };
  assert.equal(ui.readParentTheme(parentWhite), "white");
  const parentDefault = { document: { documentElement: { getAttribute: () => null } } };
  assert.equal(ui.readParentTheme(parentDefault), "light");

  // Cross-origin access throws and is handled cleanly
  const crossOrigin = {
    get document() {
      throw new Error("Blocked by cross-origin policy");
    }
  };
  assert.equal(ui.readParentTheme(crossOrigin), null);
});

test("collectThemeTokens extracts only whitelisted properties", () => {
  const read = (name) => {
    if (name === "--bg-primary") return "  #123456  ";
    if (name === "--danger-color") return "#ff0000";
    if (name === "--evil-secret") return "secret";
    return "";
  };
  const tokens = ui.collectThemeTokens(read);
  assert.equal(tokens["--bg-primary"], "#123456");
  assert.equal(tokens["--danger-color"], "#ff0000");
  assert.equal(tokens["--evil-secret"], undefined);
});

