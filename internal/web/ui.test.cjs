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
    "min-height: 34px",
    "padding: 6px 12px",
    "border: 1px solid transparent",
    "background-color: var(--bg-secondary)",
    "font-size: 13px",
    "font-weight: 600",
    "line-height: 20px",
    "white-space: nowrap"
  ]) {
    assert.ok(base.includes(declaration), "`.btn` must declare " + declaration);
  }

  assert.match(rule(".btn-primary"), /background-color:\s*var\(--primary-color\)/);
  assert.match(rule(".btn-secondary"), /background-color:\s*var\(--bg-tertiary\)/);
  assert.match(rule(".btn-ghost"), /background-color:\s*transparent/);
  assert.match(rule(".btn-danger"), /background-color:\s*var\(--error-color\)/);
  assert.match(rule(".btn-sm"), /min-height:\s*28px/);
  assert.match(rule(".btn-sm"), /padding:\s*3px 9px/);
  assert.match(rule(".btn-sm"), /font-size:\s*12px/);
  assert.match(rule(".btn-sm"), /line-height:\s*20px/);
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
  const spec = /^([a-zA-Z0-9_-]+|\*)?(?:\[([^\]=]+)(?:=['"]?([^'"\]]*)['"]?)?\])?$/.exec(selector.trim());
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

  let serial = 0;
  function makeElement(tag) {
    const el = {
      serial: ++serial,
      tagName: String(tag).toUpperCase(),
      children: [],
      attributes: {},
      handlers: {},
      listeners: {},
      textContent: "",
      className: "",
      id: "",
      value: "",
      disabled: false,
      hidden: false,
      selected: false,
      isConnected: true,
      title: "",
      style: {},
      options: [],
      max: 0,
      type: "",
      focus() {},
      contains(node) {
        let current = node;
        while (current) {
          if (current === this) return true;
          current = current.parentNode;
        }
        return false;
      },
      removeAttribute(name) {
        delete this.attributes[name];
      },
      dispatchEvent(event) {
        const name = event && event.type;
        event.currentTarget = this;
        for (const handler of this.listeners[name] || []) handler.call(this, event);
        if (typeof this["on" + name] === "function") this["on" + name](event);
        return true;
      },
      getBoundingClientRect() {
        return { left: 16, top: 40, right: 216, bottom: 74, width: 200, height: 34 };
      },
      appendChild(child) {
        this.children.push(child);
        child.parentNode = this;
        if (this.tagName === "SELECT") this.options = this.children.filter((item) => item.tagName === "OPTION");
        return child;
      },
      insertBefore(child, reference) {
        const index = reference ? this.children.indexOf(reference) : -1;
        if (index < 0) this.children.push(child);
        else this.children.splice(index, 0, child);
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
        if (name === "id") this.id = String(value);
      },
      getAttribute(name) {
        return Object.prototype.hasOwnProperty.call(this.attributes, name) ? this.attributes[name] : null;
      },
      addEventListener(name, handler) {
        this.handlers[name] = handler;
        (this.listeners[name] ||= []).push(handler);
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

  const body = makeElement("body");
  const documentElement = makeElement("html");
  documentElement.clientWidth = 1280;
  documentElement.clientHeight = 800;
  return {
    document: {
      readyState: "complete",
      body,
      documentElement,
      addEventListener() {},
      removeEventListener() {},
      createElement: makeElement,
      createEvent() {
        return { initEvent(type) { this.type = type; } };
      },
      querySelector(selector) {
        const match = /^label\[for='([^']+)'\]$/.exec(selector);
        if (!match) return null;
        for (const element of registry.values()) {
          if (element.tagName === "LABEL" && element.getAttribute("for") === match[1]) return element;
        }
        return null;
      },
      querySelectorAll(selector) {
        if (selector === "select") return body.querySelectorAll(selector);
        return body.querySelectorAll(selector);
      },
      getElementById(id) {
        if (!registry.has(id)) {
          const element = makeElement("div");
          element.id = id;
          body.appendChild(element);
          registry.set(id, element);
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
    if (url.includes("/keys") && init.method === "PATCH") {
      const resp = opts.patchResponse || {
        status: 200,
        body: { auth_index: "auth-1", mode: "provider", mode_source: "auth" }
      };
      return { status: resp.status, ok: resp.status >= 200 && resp.status < 300, text: async () => JSON.stringify(resp.body), json: async () => resp.body };
    }
    if (url.includes("/credentials") && init.method === "DELETE") {
      const resp = opts.deleteResponse || { status: 200, body: { ok: true } };
      return { status: resp.status, ok: resp.status >= 200 && resp.status < 300, text: async () => JSON.stringify(resp.body), json: async () => resp.body };
    }
    // Default GET keys
    if (typeof opts.keysTextProvider === "function") {
      const text = opts.keysTextProvider();
      return { status: 200, ok: true, text: async () => text, json: async () => JSON.parse(text) };
    }
    const files = typeof opts.filesProvider === "function" ? opts.filesProvider() : (opts.files || []);
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
      clearTimeout,
      addEventListener() {},
      removeEventListener() {}
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
      clearTimeout,
      addEventListener() {},
      removeEventListener() {}
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

/* -------------------------------------------------------- per-auth mode tests */

test("buildImportPayload preserves compatibility and embeds mode when specified", () => {
  // Backward compatibility: no mode specified
  const noMode = ui.buildImportPayload(["key-1"], "batch");
  assert.equal(noMode.ok, true);
  assert.deepEqual(JSON.parse(noMode.body), { keys: ["key-1"], label: "batch" });

  // Explicit CLI mode
  const cliMode = ui.buildImportPayload(["key-1"], "batch", "cli");
  assert.equal(cliMode.ok, true);
  assert.deepEqual(JSON.parse(cliMode.body), { keys: ["key-1"], label: "batch", mode: "cli" });

  // Explicit Provider mode
  const providerMode = ui.buildImportPayload(["key-1"], "", "provider");
  assert.equal(providerMode.ok, true);
  assert.deepEqual(JSON.parse(providerMode.body), { keys: ["key-1"], mode: "provider" });

  // Explicit invalid mode is rejected and not presumed ok
  const invalidMode = ui.buildImportPayload(["key-1"], "batch", "invalid");
  assert.equal(invalidMode.ok, false);
  const spacedMode = ui.buildImportPayload(["key-1"], "batch", " cli ");
  assert.equal(spacedMode.ok, false);
});

test("normalizeFileEntries parses mode, mode_source, and flags missing or invalid mode as error", () => {
  const rows = ui.normalizeFileEntries([
    { name: "k1.json", auth_index: "auth-1", mode: "cli", mode_source: "auth" },
    { name: "k2.json", auth_index: "auth-2", mode: "provider", mode_source: "default" },
    { name: "k3.json", auth_index: "auth-3", mode: "", mode_source: "", mode_error: true },
    { name: "k4.json", auth_index: "auth-4", mode: "invalid", mode_source: "auth" },
    { name: "k5.json", auth_index: "auth-5" },
    { name: "k6.json", auth_index: "auth-6", mode: " cli ", mode_source: "auth" },
    { name: "k7.json", auth_index: "auth-7", mode: "cli", mode_source: "bad_source" }
  ]);

  assert.equal(rows.length, 7);

  // k1: explicit auth mode
  assert.equal(rows[0].mode, "cli");
  assert.equal(rows[0].modeSource, "auth");
  assert.equal(rows[0].modeError, false);

  // k2: default provider mode
  assert.equal(rows[1].mode, "provider");
  assert.equal(rows[1].modeSource, "default");
  assert.equal(rows[1].modeError, false);

  // k3: mode_error cannot default to cli
  assert.equal(rows[2].mode, "");
  assert.equal(rows[2].modeSource, "");
  assert.equal(rows[2].modeError, true);

  // k4: invalid mode cannot default to cli
  assert.equal(rows[3].mode, "");
  assert.equal(rows[3].modeSource, "");
  assert.equal(rows[3].modeError, true);

  // k5: missing mode is flagged as unreadable error
  assert.equal(rows[4].mode, "");
  assert.equal(rows[4].modeSource, "");
  assert.equal(rows[4].modeError, true);

  // k6: untrimmed mode ' cli ' is strictly invalid (no trim leniency)
  assert.equal(rows[5].mode, "");
  assert.equal(rows[5].modeSource, "");
  assert.equal(rows[5].modeError, true);

  // k7: mode_source must be auth or default; bad_source flags modeError
  assert.equal(rows[6].mode, "");
  assert.equal(rows[6].modeSource, "");
  assert.equal(rows[6].modeError, true);
});

test("key import form sends selected mode in POST payload", async () => {
  // Test explicit provider mode
  const { get, requests, settle } = await mountUi({
    importResponse: { status: 200, body: { imported: 1, skipped: 0, failed: 0 } }
  });

  get("keys").value = "test-key-1";
  get("import-mode").value = "provider";
  get("import-form").handlers.submit({ preventDefault() {} });
  await settle();

  const postReq = requests.find((r) => r.url.includes("/keys") && r.init.method === "POST");
  assert.ok(postReq);
  const body = JSON.parse(postReq.init.body);
  assert.equal(body.mode, "provider");
  assert.deepEqual(body.keys, ["test-key-1"]);
});

test("per-row mode editing sends strict PATCH keys body with only auth_index and mode", async () => {
  let patchRequestBody = null;
  const { get, requests, settle } = await mountUi({
    files: [
      { name: "key-a.json", auth_index: "auth-a", mode: "cli", mode_source: "default" },
      { name: "key-b.json", auth_index: "auth-b", mode: "provider", mode_source: "auth" }
    ],
    patchResponse: {
      status: 200,
      body: { auth_index: "auth-a", mode: "provider", mode_source: "auth" }
    },
    filesProvider: () => [
      { name: "key-a.json", auth_index: "auth-a", mode: "provider", mode_source: "auth" },
      { name: "key-b.json", auth_index: "auth-b", mode: "provider", mode_source: "auth" }
    ]
  });

  const rows = get("files-body").children;
  assert.equal(rows.length, 2);

  // Row 0 has mode select and save button
  const row0Select = rows[0].querySelectorAll("select[data-role='mode-select']")[0];
  const row0SaveBtn = rows[0].querySelectorAll("button[data-action='save-mode']")[0];
  assert.ok(row0Select);
  assert.ok(row0SaveBtn);
  assert.equal(row0SaveBtn.className, "btn btn-secondary btn-sm");

  // Switch to provider and click save
  row0Select.value = "provider";
  row0SaveBtn.handlers.click({ currentTarget: row0SaveBtn });
  await settle();

  const patchReq = requests.find((r) => r.url.includes("/keys") && r.init.method === "PATCH");
  assert.ok(patchReq, "expected PATCH request");
  patchRequestBody = JSON.parse(patchReq.init.body);

  // Strict contract: only auth_index and mode, exactly 2 keys, no secret or extra fields
  assert.deepEqual(Object.keys(patchRequestBody).sort(), ["auth_index", "mode"]);
  assert.equal(patchRequestBody.auth_index, "auth-a");
  assert.equal(patchRequestBody.mode, "provider");
  assert.equal(patchReq.init.headers.Authorization, "Bearer test-mgmt-key");

  // Success confirmed
  assert.match(get("status").textContent, /已更新为 Provider 模式 并确认生效/);
});

test("save mode re-fetches keys list and confirms mode and mode_source=auth before reporting success", async () => {
  let patched = false;
  const { get, requests, settle } = await mountUi({
    files: [{ name: "key-1.json", auth_index: "auth-1", mode: "cli", mode_source: "default" }],
    patchResponse: {
      status: 200,
      body: { auth_index: "auth-1", mode: "provider", mode_source: "auth" }
    },
    filesProvider: () => {
      if (!patched) {
        return [{ name: "key-1.json", auth_index: "auth-1", mode: "cli", mode_source: "default" }];
      }
      return [{ name: "key-1.json", auth_index: "auth-1", mode: "provider", mode_source: "auth" }];
    }
  });

  const saveBtn = get("files-body").querySelectorAll("button[data-action='save-mode']")[0];
  const select = get("files-body").querySelectorAll("select[data-role='mode-select']")[0];
  select.value = "provider";
  patched = true;
  saveBtn.handlers.click({ currentTarget: saveBtn });
  await settle();

  // Must have issued a subsequent GET keys request after the PATCH
  const patchIndex = requests.findIndex((r) => r.init.method === "PATCH");
  const subsequentGet = requests.slice(patchIndex + 1).find((r) => r.url.includes("/keys") && r.init.method === "GET");
  assert.ok(subsequentGet, "GET keys must be re-fetched after PATCH");

  assert.match(get("status").textContent, /已更新为 Provider 模式 并确认生效/);
  assert.equal(get("status").className, "status ok");
});

test("save mode fails and preserves list when confirmation does not match requested mode or source=auth", async () => {
  const { get, settle } = await mountUi({
    // Return unchanged mode_source="default" in re-fetch (server did not persist auth source)
    files: [{ name: "key-1.json", auth_index: "auth-1", mode: "cli", mode_source: "default" }],
    patchResponse: { status: 200, body: { auth_index: "auth-1", mode: "provider", mode_source: "auth" } },
    filesProvider: () => [
      { name: "key-1.json", auth_index: "auth-1", mode: "provider", mode_source: "default" }
    ]
  });

  const saveBtn = get("files-body").querySelectorAll("button[data-action='save-mode']")[0];
  const select = get("files-body").querySelectorAll("select[data-role='mode-select']")[0];
  select.value = "provider";
  saveBtn.handlers.click({ currentTarget: saveBtn });
  await settle();

  // Must not treat submitted as success
  assert.match(get("status").textContent, /未能确认生效/);
  assert.equal(get("status").className, "status error");
  // Existing list is preserved (not wiped out)
  assert.equal(get("files-body").children.length, 1);
});

test("save mode handles 400 invalid mode, 401/403 auth failure, and 500 error without leaking secrets", async () => {
  // Case 1: 400 Bad Request
  {
    const { get, settle } = await mountUi({
      files: [{ name: "k.json", auth_index: "auth-1", mode: "cli", mode_source: "default" }],
      patchResponse: { status: 400, body: { error: "invalid mode" } }
    });
    const saveBtn = get("files-body").querySelectorAll("button[data-action='save-mode']")[0];
    const select = get("files-body").querySelectorAll("select[data-role='mode-select']")[0];
    select.value = "provider";
    saveBtn.handlers.click({ currentTarget: saveBtn });
    await settle();

    assert.match(get("status").textContent, /400/);
    assert.equal(get("status").className, "status error");
    assert.equal(get("files-body").children.length, 1);
    // No leak of server raw strings
    assert.doesNotMatch(get("status").textContent, /invalid mode/);
  }

  // Case 2: 401 Auth Failure disconnects
  {
    const { get, settle } = await mountUi({
      files: [{ name: "k.json", auth_index: "auth-1", mode: "cli", mode_source: "default" }],
      patchResponse: { status: 401, body: { error: "unauthorized" } }
    });
    const saveBtn = get("files-body").querySelectorAll("button[data-action='save-mode']")[0];
    const select = get("files-body").querySelectorAll("select[data-role='mode-select']")[0];
    select.value = "provider";
    saveBtn.handlers.click({ currentTarget: saveBtn });
    await settle();

    assert.equal(get("mgmt-key").value, "", "mgmt key must be cleared on 401");
    assert.match(get("status").textContent, /401/);
  }

  // Case 3: 500 CPA error
  {
    const { get, settle } = await mountUi({
      files: [{ name: "k.json", auth_index: "auth-1", mode: "cli", mode_source: "default" }],
      patchResponse: { status: 500, body: { error: "internal error" } }
    });
    const saveBtn = get("files-body").querySelectorAll("button[data-action='save-mode']")[0];
    const select = get("files-body").querySelectorAll("select[data-role='mode-select']")[0];
    select.value = "provider";
    saveBtn.handlers.click({ currentTarget: saveBtn });
    await settle();

    assert.match(get("status").textContent, /500/);
    assert.equal(get("files-body").children.length, 1);
  }
});

test("invalid mode row shows error status and blocks save until explicitly chosen", async () => {
  const { get, requests, settle } = await mountUi({
    files: [{ name: "bad.json", auth_index: "auth-err", mode: "", mode_error: true }]
  });

  const select = get("files-body").querySelectorAll("select[data-role='mode-select']")[0];
  const saveBtn = get("files-body").querySelectorAll("button[data-action='save-mode']")[0];
  assert.ok(select);
  assert.ok(saveBtn);

  // Initial state is unselected / empty string
  assert.equal(select.value, "");

  // Attempting to save without selecting a valid mode is blocked
  saveBtn.handlers.click({ currentTarget: saveBtn });
  await settle();

  assert.match(get("status").textContent, /请先选择有效的模式/);
  assert.equal(get("status").className, "status error");

  const patchReq = requests.find((r) => r.init.method === "PATCH");
  assert.equal(patchReq, undefined, "no PATCH request may be sent for invalid/empty mode");
});

test("settings reflects default_mode and mode_scope=auth from contract", async () => {
  const { get, settle } = await mountUi({
    settingsResponse: {
      status: 200,
      body: {
        mode: "cli",
        default_mode: "provider",
        mode_scope: "auth",
        cli_version: "1.79.2",
        catalog: { count: 87, source: "cli-snapshot", stale: false },
        catalogs: { cli: { count: 87 }, provider: { count: 12 } },
        "manual-models": []
      }
    }
  });

  assert.equal(get("setting-mode").textContent, "Provider 模式");
});

test("key import blocks invalid mode with zero network writes", async () => {
  const { get, requests, settle } = await mountUi();

  get("keys").value = "test-key-1";
  get("import-mode").value = "invalid_mode";
  get("import-form").handlers.submit({ preventDefault() {} });
  await settle();

  assert.match(get("status").textContent, /导入模式无效/);
  assert.equal(get("status").className, "status error");

  const postReq = requests.find((r) => r.url.includes("/keys") && r.init.method === "POST");
  assert.equal(postReq, undefined, "zero POST writes when import mode is invalid");
});

test("save mode preserves existing rows and selection on empty, missing, or vanishing target in re-fetch", async () => {
  // Scenario 1: GET re-fetch returns {} (missing files array)
  {
    let patched = false;
    const { get, settle } = await mountUi({
      files: [{ name: "key-1.json", auth_index: "auth-1", mode: "cli", mode_source: "default" }],
      patchResponse: { status: 200, body: { auth_index: "auth-1", mode: "provider", mode_source: "auth" } },
      filesProvider: () => {
        if (!patched) {
          return [{ name: "key-1.json", auth_index: "auth-1", mode: "cli", mode_source: "default" }];
        }
        return undefined; // fetch returns {} with no files
      }
    });

    const rowsBefore = get("files-body").children.length;
    assert.equal(rowsBefore, 1);
    const saveBtn = get("files-body").querySelectorAll("button[data-action='save-mode']")[0];
    const select = get("files-body").querySelectorAll("select[data-role='mode-select']")[0];
    select.value = "provider";
    patched = true;
    saveBtn.handlers.click({ currentTarget: saveBtn });
    await settle();

    assert.match(get("status").textContent, /未能确认生效/);
    assert.equal(get("status").className, "status error");
    // Existing rows are preserved, NOT wiped or rendered to empty-cell
    assert.equal(get("files-body").children.length, 1);
    assert.equal(select.value, "provider");
  }

  // Scenario 2: GET re-fetch returns { files: [] } (empty files array)
  {
    let patched = false;
    const { get, settle } = await mountUi({
      files: [{ name: "key-1.json", auth_index: "auth-1", mode: "cli", mode_source: "default" }],
      patchResponse: { status: 200, body: { auth_index: "auth-1", mode: "provider", mode_source: "auth" } },
      filesProvider: () => {
        if (!patched) {
          return [{ name: "key-1.json", auth_index: "auth-1", mode: "cli", mode_source: "default" }];
        }
        return []; // empty files array
      }
    });

    const saveBtn = get("files-body").querySelectorAll("button[data-action='save-mode']")[0];
    const select = get("files-body").querySelectorAll("select[data-role='mode-select']")[0];
    select.value = "provider";
    patched = true;
    saveBtn.handlers.click({ currentTarget: saveBtn });
    await settle();

    assert.match(get("status").textContent, /未能确认生效/);
    assert.equal(get("status").className, "status error");
    // Existing row preserved, not emptied
    assert.equal(get("files-body").children.length, 1);
  }

  // Scenario 3: GET re-fetch returns other keys but target key vanished
  {
    let patched = false;
    const { get, settle } = await mountUi({
      files: [{ name: "target.json", auth_index: "auth-target", mode: "cli", mode_source: "default" }],
      patchResponse: { status: 200, body: { auth_index: "auth-target", mode: "provider", mode_source: "auth" } },
      filesProvider: () => {
        if (!patched) {
          return [{ name: "target.json", auth_index: "auth-target", mode: "cli", mode_source: "default" }];
        }
        // Target disappeared from server response
        return [{ name: "other.json", auth_index: "auth-other", mode: "provider", mode_source: "auth" }];
      }
    });

    const saveBtn = get("files-body").querySelectorAll("button[data-action='save-mode']")[0];
    const select = get("files-body").querySelectorAll("select[data-role='mode-select']")[0];
    select.value = "provider";
    patched = true;
    saveBtn.handlers.click({ currentTarget: saveBtn });
    await settle();

    assert.match(get("status").textContent, /未能确认生效/);
    assert.equal(get("status").className, "status error");
    // Existing rows preserved
    assert.equal(get("files-body").children.length, 1);
  }
});

test("a broken 200 keys list is not reported as empty and preserves existing rows", async () => {
  const badBodies = [
    { name: "invalid-json", text: "NOT_JSON_BODY_200{" },
    { name: "empty-object", text: "{}" },
    { name: "error-body", text: '{"error":"failed"}' },
    { name: "non-array-files", text: '{"files":false}' }
  ];

  for (const bad of badBodies) {
    let calls = 0;
    const { get, settle } = await mountUi({
      keysTextProvider: () => {
        calls += 1;
        // The first load is a valid list; the reload returns the broken body.
        return calls === 1
          ? JSON.stringify({ files: [{ name: "key-1.json", auth_index: "auth-1" }] })
          : bad.text;
      }
    });

    assert.equal(get("files-body").children.length, 1, bad.name + ": initial row rendered");
    assert.equal(get("status").className, "status ok", bad.name + ": initial load ok");

    get("reload-btn").handlers.click({});
    await settle();

    assert.equal(get("files-body").children.length, 1, bad.name + ": existing row preserved");
    assert.equal(get("status").className, "status error", bad.name + ": failure surfaces an error");
    assert.doesNotMatch(get("status").textContent, /暂无已托管的密钥/, bad.name + ": must not claim an empty directory");
    assert.doesNotMatch(get("status").textContent, /共 0 条/, bad.name + ": must not claim zero total");
  }
});

test("a legal empty files list remains an empty-list success", async () => {
  const { get, settle } = await mountUi({
    keysTextProvider: () => JSON.stringify({ files: [] })
  });
  assert.equal(get("status").className, "status ok");
  assert.match(get("status").textContent, /列表为空/);
  await settle();
});

test("a falsy 200 keys body (null/false/0) surfaces an error instead of stalling or rendering empty", async () => {
  // A valid JSON body that is not a key directory must be treated exactly like
  // any other malformed 200 body; the old `if (!payload) return` guard let
  // null/false/0 through and left the UI stuck on the loading status.
  for (const body of ["null", "false", "0"]) {
    // Initial request: the very first GET returns the falsy body.
    {
      const { get } = await mountUi({ keysTextProvider: () => body });
      assert.equal(get("status").className, "status error", body + ": initial falsy body must surface an error");
      assert.doesNotMatch(get("status").textContent, /正在加载/, body + ": must not stall on the loading status");
      assert.doesNotMatch(get("status").textContent, /暂无已托管的密钥/, body + ": must not claim an empty directory");
      assert.equal(get("files-body").children.length, 0, body + ": must not render an empty placeholder row");
    }
    // Refresh control: a valid list first, then the reload returns the falsy body.
    {
      let calls = 0;
      const { get, settle } = await mountUi({
        keysTextProvider: () => {
          calls += 1;
          return calls === 1 ? JSON.stringify({ files: [{ name: "key-1.json", auth_index: "auth-1" }] }) : body;
        }
      });
      assert.equal(get("files-body").children.length, 1, body + ": initial valid row rendered");
      assert.equal(get("status").className, "status ok", body + ": initial load ok");

      get("reload-btn").handlers.click({});
      await settle();

      assert.equal(get("files-body").children.length, 1, body + ": existing row preserved on refresh");
      assert.equal(get("status").className, "status error", body + ": refresh failure surfaces an error");
      assert.doesNotMatch(get("status").textContent, /暂无已托管的密钥/, body + ": must not claim an empty directory");
    }
  }
});

test("ui.html settings copy explicitly states per-credential override and default for legacy keys", () => {
  const html = readAsset("ui.html");
  assert.ok(html.includes("每个凭据单独配置，下面是缺少mode旧凭据的默认值"));
});

// The visual select must commit through the native control and must not persist highlight classes.
test("select controller commits one native change and clears menus when rows are replaced", () => {
  const dom = createMockDom();
  const doc = dom.document;
  const select = doc.createElement("select");
  select.id = "import-mode";
  for (const [value, label] of [["cli", "CLI"], ["provider", "Provider"]]) {
    const option = doc.createElement("option");
    option.value = value;
    option.textContent = label;
    select.appendChild(option);
  }
  select.value = "cli";
  doc.body.appendChild(select);
  select.changeCount = 0;
  select.addEventListener("change", function () { this.changeCount += 1; });
  const controller = ui.createSelectController({
    innerWidth: 390,
    innerHeight: 700,
    addEventListener() {},
    removeEventListener() {}
  }, doc);
  controller.refresh();
  const trigger = select.parentNode.children.find((child) => child.getAttribute("role") === "combobox");
  assert.equal(trigger.getAttribute("aria-expanded"), "false");
  trigger.handlers.click({});
  const listboxes = () => doc.body.querySelectorAll("[role='listbox']");
  const optionNodes = () => doc.body.querySelectorAll("[role='option']");
  assert.equal(listboxes().length, 1);
  const options = optionNodes();
  options[1].handlers.mouseenter({ currentTarget: options[1] });
  assert.equal(options[1].className.includes("is-highlighted"), true);
  options[1].handlers.click({ currentTarget: options[1] });
  assert.equal(select.value, "provider");
  assert.equal(select.changeCount, 1);
  assert.equal(listboxes().length, 0);
  assert.equal(doc.body.querySelectorAll("[role='option']").length, 0);

  trigger.handlers.keydown({ key: "ArrowDown", preventDefault() {}, currentTarget: trigger });
  trigger.handlers.keydown({ key: "Escape", preventDefault() {}, currentTarget: trigger });
  assert.equal(select.value, "provider");
  assert.equal(select.changeCount, 1);
  assert.equal(listboxes().length, 0);

  select.disabled = true;
  controller.refresh();
  trigger.handlers.click({});
  assert.equal(trigger.disabled, true);
  assert.equal(listboxes().length, 0);

  select.isConnected = false;
  controller.refresh();
  assert.equal(select.parentNode, null);
  controller.refresh();
  assert.equal(doc.body.querySelectorAll(".cpa-select-menu").length, 0);
});
