"use strict";

/*
 * CommandCode key and quota management UI for the cliproxyapi-commandcode-provider CPA plugin.
 *
 * Security invariants:
 * - The CPA management key lives only in the password input and in request headers.
 * - Imported CommandCode keys live only in the textarea and in the in-flight request body.
 * - No secret is written to browser storage, Cookies, URLs, logs, tooltips, titles or toasts.
 * - All endpoints are derived relative to the current location so an upstream proxy prefix works.
 */

var PLUGIN_ID = "cliproxyapi-commandcode-provider";
var MAX_KEYS = 100;
var MAX_KEY_LENGTH = 4096;
var MAX_BODY_BYTES = 64 * 1024;
var MAX_LABEL_LENGTH = 128;
var MAX_CELL_LENGTH = 200;
var MAX_FILE_NAME_LENGTH = 255;
var MAX_AUTH_INDEX_LENGTH = 200;

// CommandCode rate limit quota windows (five-hour and weekly).
var QUOTA_WINDOWS = [
  { window: "five-hour", label: "5小时窗口" },
  { window: "weekly", label: "每周窗口" }
];

// Strict RFC3339 timestamp shape, matching the host's time.RFC3339 parsing.
var ISO_TIMESTAMP_RE = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;

var CONTROL_CHARS_RE = /[\u0000-\u001f\u007f]/;
var CONTROL_CHARS_RE_GLOBAL = /[\u0000-\u001f\u007f]/g;
var WHITESPACE_RE = /\s/;

/* ---------------------------------------------------------------- pure helpers */

function isPlainObject(value) {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

// UTF-8 byte length, used to enforce the 64 KiB request-body budget.
function utf8ByteLength(text) {
  var value = String(text);
  if (typeof TextEncoder === "function") {
    return new TextEncoder().encode(value).length;
  }
  var bytes = 0;
  for (var i = 0; i < value.length; i++) {
    var code = value.codePointAt(i);
    if (code > 0xffff) {
      i++;
    }
    if (code <= 0x7f) {
      bytes += 1;
    } else if (code <= 0x7ff) {
      bytes += 2;
    } else if (code <= 0xffff) {
      bytes += 3;
    } else {
      bytes += 4;
    }
  }
  return bytes;
}

// Derive management endpoints from the page location, keeping any proxy prefix.
function deriveEndpoints(locationLike) {
  var pathname = locationLike && typeof locationLike.pathname === "string" ? locationLike.pathname : "";
  var marker = "/v0/resource/plugins/" + PLUGIN_ID + "/";
  var index = pathname.indexOf(marker);
  var prefix = index >= 0 ? pathname.slice(0, index) : "";
  return {
    prefix: prefix,
    keysUrl: prefix + "/v0/management/plugins/" + PLUGIN_ID + "/keys",
    settingsUrl: prefix + "/v0/management/plugins/" + PLUGIN_ID + "/settings",
    validateUrl: prefix + "/v0/management/plugins/" + PLUGIN_ID + "/validate",
    configUrl: prefix + "/v0/management/plugins/" + PLUGIN_ID + "/config",
    credentialsUrl: prefix + "/v8/management/credentials",
    quotaUrl: prefix + "/v0/management/plugins/" + PLUGIN_ID + "/quota"
  };
}

// Plaintext HTTP is only tolerated on loopback; everything else needs HTTPS.
function isSecureUiContext(locationLike) {
  var protocol = locationLike && typeof locationLike.protocol === "string" ? locationLike.protocol : "";
  var hostname = String((locationLike && locationLike.hostname) || "").toLowerCase();
  if (protocol === "https:") {
    return true;
  }
  if (protocol !== "http:") {
    return false;
  }
  return hostname === "localhost" || hostname === "127.0.0.1" || hostname === "[::1]" || hostname === "::1";
}

// Parse the textarea: one key per line, trimmed, deduplicated, validated.
function parseKeys(raw) {
  var text = typeof raw === "string" ? raw : "";
  var lines = text.split(/\r\n|\r|\n/);
  var seen = new Set();
  var keys = [];
  var invalidLines = [];
  var empty = 0;
  var duplicates = 0;

  for (var i = 0; i < lines.length; i++) {
    var value = lines[i].trim();
    if (!value) {
      empty++;
      continue;
    }
    if (CONTROL_CHARS_RE.test(value)) {
      invalidLines.push({ line: i + 1, reason: "control" });
      continue;
    }
    if (WHITESPACE_RE.test(value)) {
      invalidLines.push({ line: i + 1, reason: "whitespace" });
      continue;
    }
    if (value.length > MAX_KEY_LENGTH) {
      invalidLines.push({ line: i + 1, reason: "too-long" });
      continue;
    }
    if (seen.has(value)) {
      duplicates++;
      continue;
    }
    seen.add(value);
    keys.push(value);
  }

  var counts = {
    lines: lines.length,
    empty: empty,
    duplicates: duplicates,
    invalid: invalidLines.length,
    valid: keys.length
  };

  if (keys.length > MAX_KEYS) {
    return {
      keys: [],
      counts: counts,
      invalidLines: invalidLines,
      error: { code: "too-many", message: "有效密钥超过 " + MAX_KEYS + " 条上限，请分批导入。" }
    };
  }

  return { keys: keys, counts: counts, invalidLines: invalidLines, error: null };
}

function validateLabel(label) {
  var value = typeof label === "string" ? label.trim() : "";
  if (!value) {
    return { ok: true, value: "", reason: "" };
  }
  if (CONTROL_CHARS_RE.test(value)) {
    return { ok: false, value: "", reason: "control" };
  }
  if (value.length > MAX_LABEL_LENGTH) {
    return { ok: false, value: "", reason: "too-long" };
  }
  return { ok: true, value: value, reason: "" };
}

// Build the exact JSON body that will be POSTed, and check the byte budget.
function buildImportPayload(keys, label, mode) {
  var payload = { keys: Array.isArray(keys) ? keys.slice() : [] };
  if (typeof label === "string" && label) {
    payload.label = label;
  }
  if (mode !== undefined && mode !== null && mode !== "") {
    if (mode !== "cli" && mode !== "provider") {
      return { body: "", bytes: 0, ok: false };
    }
    payload.mode = mode;
  }
  var body = JSON.stringify(payload);
  var bytes = utf8ByteLength(body);
  return { body: body, bytes: bytes, ok: bytes <= MAX_BODY_BYTES };
}

function toCellText(value) {
  var text = "";
  if (typeof value === "string") {
    text = value;
  } else if (typeof value === "number" && isFinite(value)) {
    text = String(value);
  } else if (typeof value === "boolean") {
    text = value ? "true" : "false";
  } else {
    return "";
  }
  text = text.replace(CONTROL_CHARS_RE_GLOBAL, "").trim();
  if (text.length > MAX_CELL_LENGTH) {
    text = text.slice(0, MAX_CELL_LENGTH) + "…";
  }
  return text;
}

function toCount(value) {
  var number = typeof value === "number" ? value : Number(value);
  if (!isFinite(number) || number <= 0) {
    return 0;
  }
  return Math.trunc(number);
}

// Only a plain file name (no path separators, no control characters) may be deleted.
function isSafeFileName(name) {
  if (typeof name !== "string") {
    return false;
  }
  var value = name.trim();
  if (!value || value.length > MAX_FILE_NAME_LENGTH || value === "." || value === "..") {
    return false;
  }
  if (CONTROL_CHARS_RE.test(value)) {
    return false;
  }
  return value.indexOf("/") === -1 && value.indexOf("\\") === -1;
}

// A host credential index is only ever placed in a JSON body, but it must still
// be a bounded, single-line token so a hostile listing cannot smuggle anything.
function isSafeAuthIndex(value) {
  if (typeof value !== "string") {
    return false;
  }
  var trimmed = value.trim();
  if (!trimmed || trimmed.length > MAX_AUTH_INDEX_LENGTH) {
    return false;
  }
  if (CONTROL_CHARS_RE.test(trimmed) || WHITESPACE_RE.test(trimmed)) {
    return false;
  }
  return true;
}

// Build the exact read-only quota request body for a validated auth index.
function buildQuotaPayload(authIndex) {
  if (!isSafeAuthIndex(authIndex)) {
    return { ok: false, body: "" };
  }
  return { ok: true, body: JSON.stringify({ auth_index: authIndex.trim() }) };
}

function isValidIsoTimestamp(value) {
  if (typeof value !== "string" || !ISO_TIMESTAMP_RE.test(value)) {
    return false;
  }
  return isFinite(Date.parse(value));
}

function pad2(value) {
  return value < 10 ? "0" + value : String(value);
}

// Render a validated timestamp deterministically in UTC so the display never
// depends on the viewer's timezone or echoes an unvalidated server string.
function formatQuotaReset(value) {
  var parsed = Date.parse(value);
  if (!isFinite(parsed)) {
    return "";
  }
  var date = new Date(parsed);
  return (
    date.getUTCFullYear() + "-" + pad2(date.getUTCMonth() + 1) + "-" + pad2(date.getUTCDate()) +
    " " + pad2(date.getUTCHours()) + ":" + pad2(date.getUTCMinutes()) + " UTC"
  );
}

function quotaWindowLabel(name) {
  for (var i = 0; i < QUOTA_WINDOWS.length; i++) {
    if (QUOTA_WINDOWS[i].window === name) {
      return QUOTA_WINDOWS[i].label;
    }
  }
  return "";
}

function toNullableCredit(value) {
  if (typeof value === "number" && isFinite(value)) {
    return value;
  }
  return null;
}

// Strictly validate the CommandCode quota response.
// Supports subscription, summary (credits), and five-hour/weekly buckets without forcing 3 windows.
function parseQuotaResponse(payload) {
  var failure = { ok: false, subscription: null, summary: null, buckets: [] };
  if (!isPlainObject(payload)) {
    return failure;
  }

  var sub = null;
  if (isPlainObject(payload.subscription)) {
    sub = {
      plan: toCellText(payload.subscription.plan),
      status: toCellText(payload.subscription.status),
      period: toCellText(payload.subscription.period)
    };
  }

  var summary = null;
  if (isPlainObject(payload.summary)) {
    summary = {
      totalCredits: toNullableCredit(payload.summary.total_credits),
      monthlyCredits: toNullableCredit(payload.summary.monthly_credits),
      purchasedCredits: toNullableCredit(payload.summary.purchased_credits),
      freeCredits: toNullableCredit(payload.summary.free_credits)
    };
  }

  var groups = Array.isArray(payload.groups) ? payload.groups : [];
  var byWindow = {};
  for (var g = 0; g < groups.length; g++) {
    var group = groups[g];
    if (!isPlainObject(group) || !Array.isArray(group.buckets)) {
      continue;
    }
    for (var b = 0; b < group.buckets.length; b++) {
      var bucket = group.buckets[b];
      if (!isPlainObject(bucket)) {
        continue;
      }
      var winName = typeof bucket.window === "string" ? bucket.window : "";
      var label = quotaWindowLabel(winName);
      if (!label) {
        continue; // unknown windows are ignored, never guessed at
      }
      if (Object.prototype.hasOwnProperty.call(byWindow, winName)) {
        return failure; // duplicate window is ambiguous
      }
      var fraction = bucket.remainingFraction;
      if (typeof fraction !== "number" || !isFinite(fraction) || fraction < 0 || fraction > 1) {
        return failure;
      }
      var resetTimeFormatted = "";
      if (bucket.resetTime !== undefined && bucket.resetTime !== null && bucket.resetTime !== "") {
        if (!isValidIsoTimestamp(bucket.resetTime)) {
          return failure;
        }
        resetTimeFormatted = formatQuotaReset(bucket.resetTime);
      }
      var remainingPercent = Math.round(fraction * 100);
      byWindow[winName] = {
        window: winName,
        label: label,
        remainingFraction: fraction,
        remainingPercent: remainingPercent,
        usedPercent: 100 - remainingPercent,
        resetTime: resetTimeFormatted
      };
    }
  }

  var buckets = [];
  for (var w = 0; w < QUOTA_WINDOWS.length; w++) {
    var entry = byWindow[QUOTA_WINDOWS[w].window];
    if (entry) {
      buckets.push(entry);
    }
  }

  var hasSub = sub && (sub.plan || sub.status);
  var hasSummary =
    summary &&
    (summary.totalCredits !== null ||
      summary.monthlyCredits !== null ||
      summary.purchasedCredits !== null ||
      summary.freeCredits !== null);
  var hasBuckets = buckets.length > 0;

  if (!hasSub && !hasSummary && !hasBuckets) {
    return failure;
  }

  return {
    ok: true,
    subscription: sub || { plan: "", status: "", period: "" },
    summary: summary,
    buckets: buckets
  };
}

// Fixed, local quota messages: server error strings are never surfaced.
function describeQuotaFailure(status) {
  if (status === 400) {
    return "配额请求无效（400）。";
  }
  if (status === 404) {
    return "配额接口不可用（404），请确认 CPA 与插件版本匹配。";
  }
  if (status === 501) {
    return "当前插件不支持配额查询（501）。";
  }
  if (status >= 500) {
    return "无法获取配额（服务端错误 " + status + "），请稍后重试。";
  }
  return "配额查询失败（" + status + "）。";
}

// Convert non-secret list entries into plain display rows; never carries secrets.
function normalizeFileEntries(files) {
  if (!Array.isArray(files)) {
    return [];
  }
  var rows = [];
  for (var i = 0; i < files.length; i++) {
    var entry = files[i];
    if (!isPlainObject(entry)) {
      continue;
    }
    var rawName = typeof entry.name === "string" ? entry.name : "";
    var rawIndex = typeof entry.auth_index === "string" ? entry.auth_index : "";
    var rawMode = typeof entry.mode === "string" ? entry.mode : "";
    var rawModeSource = typeof entry.mode_source === "string" ? entry.mode_source : "";
    var modeValid = rawMode === "cli" || rawMode === "provider";
    var sourceValid = rawModeSource === "auth" || rawModeSource === "default";
    var modeError = entry.mode_error === true || !modeValid || !sourceValid;
    var mode = modeError ? "" : rawMode;
    var modeSource = modeError ? "" : rawModeSource;

    rows.push({
      rawName: rawName,
      name: toCellText(rawName),
      label: toCellText(entry.label),
      mode: mode,
      modeSource: modeSource,
      modeError: modeError,
      status: toCellText(entry.status),
      disabled: entry.disabled === true,
      unavailable: entry.unavailable === true,
      success: toCount(entry.success),
      failed: toCount(entry.failed),
      deletable: isSafeFileName(rawName),
      authIndex: rawIndex,
      quotaAvailable: isSafeAuthIndex(rawIndex)
    });
  }
  return rows;
}

// Local, fixed messages only: untrusted server strings are never surfaced.
function describeHttpFailure(status) {
  if (status === 400) {
    return "请求无效（400），请检查输入内容后重试。";
  }
  if (status === 401) {
    return "管理密钥无效或缺失（401）。已断开连接，请重新输入密钥。";
  }
  if (status === 403) {
    return "未授权（403）。可能未启用 CPA 远程管理、密钥错误，或当前 IP 因多次失败被临时封禁。";
  }
  if (status === 404) {
    return "管理接口不存在（404），请确认 CPA 与插件版本匹配。";
  }
  if (status === 405) {
    return "请求方法不被支持（405），请确认 CPA 与插件版本匹配。";
  }
  if (status === 413) {
    return "请求体过大（413），请减少密钥数量后重试。";
  }
  if (status >= 500) {
    return "CPA 服务端错误（" + status + "），请稍后重试。";
  }
  return "请求失败（" + status + "）。";
}

function countNotice(counts) {
  var parts = [];
  if (counts.duplicates > 0) {
    parts.push("忽略重复 " + counts.duplicates + " 条");
  }
  if (counts.invalid > 0) {
    parts.push("跳过无效 " + counts.invalid + " 行");
  }
  if (counts.empty > 0) {
    parts.push("忽略空行 " + counts.empty + " 行");
  }
  return parts.length ? "（" + parts.join("，") + "）" : "";
}

// Import counts must be actual non-negative safe integers.
function isNonNegativeSafeInteger(value) {
  return typeof value === "number" && Number.isSafeInteger(value) && value >= 0;
}

// Confirm that the import report accounts for every submitted key.
function formatImportResult(status, payload, expectedCount) {
  if (!(status >= 200 && status < 300)) {
    return {
      ok: false,
      level: "error",
      summary: describeHttpFailure(status),
      imported: 0,
      skipped: 0,
      failed: 0
    };
  }
  var data = isPlainObject(payload) ? payload : null;
  var countsValid =
    data !== null &&
    isNonNegativeSafeInteger(expectedCount) &&
    isNonNegativeSafeInteger(data.imported) &&
    isNonNegativeSafeInteger(data.skipped) &&
    isNonNegativeSafeInteger(data.failed) &&
    data.imported + data.skipped + data.failed === expectedCount;
  if (!countsValid) {
    // Never guess: an unrecognized body is not a confirmed success.
    return {
      ok: false,
      level: "error",
      summary: "无法确认导入结果，请刷新列表核对后再决定是否重试。",
      imported: 0,
      skipped: 0,
      failed: 0
    };
  }
  var summary;
  var level;
  if (status === 207 || data.failed > 0) {
    summary = "部分导入完成：成功 " + data.imported + "，跳过 " + data.skipped + "，失败 " + data.failed + "。";
    level = data.imported > 0 ? "warn" : "error";
  } else {
    summary = "导入成功：成功 " + data.imported + "，跳过 " + data.skipped + "，失败 " + data.failed + "。";
    level = "ok";
  }
  return {
    ok: true,
    level: level,
    summary: summary,
    imported: data.imported,
    skipped: data.skipped,
    failed: data.failed
  };
}

// Compose the management request. The key is only ever placed in the Authorization header.
function buildManagementRequest(key, options) {
  var opts = isPlainObject(options) ? options : {};
  var headers = { Accept: "application/json" };
  if (typeof key === "string" && key.length > 0) {
    headers.Authorization = "Bearer " + key;
  }
  var init = {
    method: typeof opts.method === "string" ? opts.method : "GET",
    credentials: "omit",
    cache: "no-store",
    headers: headers
  };
  if (typeof opts.body === "string") {
    init.body = opts.body;
    headers["Content-Type"] = "application/json";
  }
  return init;
}

/* ------------------------------------------------------------------- theming */

// Only these non-secret design tokens may ever be copied from the host page.
var THEME_TOKENS = [
  "--bg-secondary", "--bg-primary", "--bg-tertiary", "--bg-hover",
  "--text-primary", "--text-secondary", "--text-tertiary",
  "--muted-bg", "--muted-foreground", "--accent-bg",
  "--border-color", "--border-primary", "--border-hover",
  "--primary-color", "--primary-hover", "--primary-active", "--primary-contrast",
  "--success-color", "--warning-color", "--error-color", "--danger-color",
  "--radius-md", "--radius-lg", "--shadow", "--shadow-lg"
];

function resolveAppliedTheme(parentTheme, systemPrefersDark) {
  if (parentTheme === "dark" || parentTheme === "white" || parentTheme === "light") {
    return parentTheme;
  }
  return systemPrefersDark ? "dark" : "white";
}

function readParentTheme(parentWindow) {
  try {
    if (!parentWindow || (typeof window !== "undefined" && parentWindow === window)) {
      return null;
    }
    var parentDocument = parentWindow.document;
    if (!parentDocument || !parentDocument.documentElement) {
      return null;
    }
    var theme = parentDocument.documentElement.getAttribute("data-theme");
    if (theme === "dark" || theme === "white") {
      return theme;
    }
    return "light";
  } catch (err) {
    return null;
  }
}

function collectThemeTokens(readToken) {
  var tokens = {};
  if (typeof readToken !== "function") {
    return tokens;
  }
  for (var i = 0; i < THEME_TOKENS.length; i++) {
    var name = THEME_TOKENS[i];
    var value = readToken(name);
    if (typeof value === "string") {
      value = value.trim();
      if (value) {
        tokens[name] = value;
      }
    }
  }
  return tokens;
}

function applyThemeToRoot(root, applied, parentWindow) {
  root.setAttribute("data-theme", applied);
  if (!parentWindow || typeof window === "undefined" || typeof window.getComputedStyle !== "function") {
    return;
  }
  var tokens = {};
  try {
    var hostRoot = parentWindow.document.documentElement;
    var hostStyle = window.getComputedStyle(hostRoot);
    tokens = collectThemeTokens(function (name) {
      return hostStyle.getPropertyValue(name);
    });
  } catch (err) {
    tokens = {};
  }
  for (var i = 0; i < THEME_TOKENS.length; i++) {
    var name = THEME_TOKENS[i];
    if (typeof root.style.removeProperty === "function") {
      root.style.removeProperty(name);
    }
    if (Object.prototype.hasOwnProperty.call(tokens, name)) {
      root.style.setProperty(name, tokens[name]);
    }
  }
}

function prefersDarkScheme() {
  try {
    return !!(window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches);
  } catch (err) {
    return false;
  }
}

function initTheme() {
  if (typeof document === "undefined" || !document.documentElement) {
    return;
  }
  var root = document.documentElement;
  var hostWindow = typeof window !== "undefined" ? window.parent : null;
  var hostTheme = readParentTheme(hostWindow);

  function reapply() {
    var observed = readParentTheme(hostWindow);
    applyThemeToRoot(root, resolveAppliedTheme(observed, prefersDarkScheme()), observed === null ? null : hostWindow);
  }

  reapply();

  if (hostTheme !== null && typeof MutationObserver === "function") {
    try {
      var observer = new MutationObserver(reapply);
      observer.observe(hostWindow.document.documentElement, {
        attributes: true,
        attributeFilter: ["data-theme"]
      });
    } catch (err) {
      // Cross-origin: keep static tokens
    }
  }

  if (hostTheme === null) {
    try {
      var media = window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)");
      if (media && typeof media.addEventListener === "function") {
        media.addEventListener("change", reapply);
      }
    } catch (err) {
      // No live system sync
    }
  }
}

function normalizeManualModels(raw) {
  if (!Array.isArray(raw) || raw.length > 100) throw new Error("手动模型必须是最多 100 项的列表。");
  var seen = new Set();
  return raw.map(function (item) {
    if (!isPlainObject(item) || typeof item.id !== "string") throw new Error("模型 ID 无效。");
    var id = item.id.trim().replace(/^commandcode\//, "");
    if (!id || /^commandcode\//.test(id) || /[\s\u0000-\u001f\u007f-\u009f]/.test(id) || utf8ByteLength(id) > 200 || seen.has(id)) {
      throw new Error("模型 ID 必须唯一、不含空白或控制字符，且不超过 200 字节。");
    }
    seen.add(id);
    var protocol = item.protocol === undefined ? "" : item.protocol;
    if (typeof protocol !== "string" || ["", "openai", "claude", "openai-response"].indexOf(protocol) < 0) {
      throw new Error("请选择受支持的上游协议。");
    }
    return protocol ? { id: id, protocol: protocol } : { id: id };
  });
}

var api = {
  PLUGIN_ID: PLUGIN_ID,
  MAX_KEYS: MAX_KEYS,
  MAX_KEY_LENGTH: MAX_KEY_LENGTH,
  MAX_BODY_BYTES: MAX_BODY_BYTES,
  MAX_LABEL_LENGTH: MAX_LABEL_LENGTH,
  MAX_AUTH_INDEX_LENGTH: MAX_AUTH_INDEX_LENGTH,
  QUOTA_WINDOWS: QUOTA_WINDOWS,
  utf8ByteLength: utf8ByteLength,
  deriveEndpoints: deriveEndpoints,
  isSecureUiContext: isSecureUiContext,
  parseKeys: parseKeys,
  validateLabel: validateLabel,
  buildImportPayload: buildImportPayload,
  toCellText: toCellText,
  toCount: toCount,
  isSafeFileName: isSafeFileName,
  isSafeAuthIndex: isSafeAuthIndex,
  buildQuotaPayload: buildQuotaPayload,
  isValidIsoTimestamp: isValidIsoTimestamp,
  formatQuotaReset: formatQuotaReset,
  parseQuotaResponse: parseQuotaResponse,
  normalizeFileEntries: normalizeFileEntries,
  describeHttpFailure: describeHttpFailure,
  describeQuotaFailure: describeQuotaFailure,
  countNotice: countNotice,
  formatImportResult: formatImportResult,
  buildManagementRequest: buildManagementRequest,
  THEME_TOKENS: THEME_TOKENS,
  resolveAppliedTheme: resolveAppliedTheme,
  readParentTheme: readParentTheme,
  collectThemeTokens: collectThemeTokens,
  normalizeManualModels: normalizeManualModels
};

if (typeof module !== "undefined" && module.exports) {
  module.exports = api;
}

/* ---------------------------------------------------------------- DOM wiring */

function initUi() {
  var doc = document;
  var el = {
    mgmtKey: doc.getElementById("mgmt-key"),
    authForm: doc.getElementById("auth-form"),
    refreshBtn: doc.getElementById("refresh-btn"),
    importForm: doc.getElementById("import-form"),
    keys: doc.getElementById("keys"),
    label: doc.getElementById("label"),
    importMode: doc.getElementById("import-mode"),
    importBtn: doc.getElementById("import-btn"),
    importProgress: doc.getElementById("import-progress"),
    reloadBtn: doc.getElementById("reload-btn"),
    summary: doc.getElementById("summary"),
    status: doc.getElementById("status"),
    tableWrap: doc.getElementById("table-wrap"),
    filesBody: doc.getElementById("files-body"),
    insecureWarning: doc.getElementById("insecure-warning"),
    quotaStatus: doc.getElementById("quota-status"),
    quotaPanel: doc.getElementById("quota-panel"),
    quotaSelection: doc.getElementById("quota-selection"),
    quotaSummary: doc.getElementById("quota-summary"),
    quotaCreditsTotal: doc.getElementById("quota-credits-total"),
    quotaCreditsMonthly: doc.getElementById("quota-credits-monthly"),
    quotaCreditsPurchased: doc.getElementById("quota-credits-purchased"),
    quotaCreditsFree: doc.getElementById("quota-credits-free"),
    quotaTableWrap: doc.getElementById("quota-table-wrap"),
    quotaBody: doc.getElementById("quota-body"),
    quotaNoBuckets: doc.getElementById("quota-no-buckets"),
    settingMode: doc.getElementById("setting-mode"),
    settingCliVersion: doc.getElementById("setting-cli-version"),
    settingBaseUrl: doc.getElementById("setting-base-url"),
    settingZdr: doc.getElementById("setting-zdr"),
    settingCatalog: doc.getElementById("setting-catalog"),
    settingStale: doc.getElementById("setting-stale")
  };

  if (!el.mgmtKey || !el.keys || !el.filesBody || !el.status || !el.quotaStatus || !el.quotaBody) {
    return;
  }

  var endpoints = deriveEndpoints(window.location);
  var secureContext = false;
  var busy = false;
  var manualDraft = [];
  var modelsLoaded = false;
  var modelsDirty = false;
  var modelBody = doc.getElementById("manual-model-body");
  var modelStatus = doc.getElementById("manual-model-status");
  var modelId = doc.getElementById("manual-model-id");
  var modelProtocol = doc.getElementById("manual-model-protocol");
  var modelSave = doc.getElementById("manual-model-save");
  var modelReload = doc.getElementById("manual-model-reload");
  var modelAdd = doc.getElementById("manual-model-add");

  function setStatus(message, level) {
    el.status.textContent = message;
    el.status.className = "status" + (level ? " " + level : "");
  }

  function setQuotaStatus(message, level) {
    el.quotaStatus.textContent = message;
    el.quotaStatus.className = "status" + (level ? " " + level : "");
  }

  var QUOTA_IDLE_MESSAGE = "尚未选择密钥。请在“已托管的密钥”列表中点击某个密钥的“查看配额”。";

  // Drop any rendered quota so a stale selection is never shown as current.
  function clearQuota() {
    el.quotaPanel.hidden = true;
    el.quotaSelection.textContent = "";
    if (el.quotaSummary) {
      el.quotaSummary.hidden = true;
    }
    if (el.quotaNoBuckets) {
      el.quotaNoBuckets.hidden = true;
    }
    while (el.quotaBody.firstChild) {
      el.quotaBody.removeChild(el.quotaBody.firstChild);
    }
    setQuotaStatus(QUOTA_IDLE_MESSAGE, "info");
  }

  function setBusy(flag) {
    busy = flag;
    var disabled = flag || !secureContext;
    el.refreshBtn.disabled = disabled;
    el.importBtn.disabled = disabled;
    el.reloadBtn.disabled = disabled;
    el.mgmtKey.disabled = disabled;
    if (modelId) modelId.disabled = disabled || !modelsLoaded;
    if (modelProtocol) modelProtocol.disabled = disabled || !modelsLoaded;
    if (modelAdd) modelAdd.disabled = disabled || !modelsLoaded;
    if (modelReload) modelReload.disabled = disabled;
    if (modelSave) modelSave.disabled = disabled || !modelsLoaded || !modelsDirty;
    if (modelBody) {
      var modelButtons = modelBody.querySelectorAll("button");
      for (var b = 0; b < modelButtons.length; b++) {
        modelButtons[b].disabled = disabled;
      }
    }
    // Lock the key inputs while busy so an in-flight response can never clear
    // text the user typed after submitting.
    el.keys.disabled = disabled;
    el.label.disabled = disabled;
    if (el.importMode) el.importMode.disabled = disabled;
    el.importProgress.hidden = !flag;
    var actionButtons = el.filesBody.querySelectorAll("button[data-action]");
    for (var i = 0; i < actionButtons.length; i++) {
      var blocked = disabled;
      var action = actionButtons[i].getAttribute("data-action");
      if (action === "quota" || action === "save-mode") {
        blocked = disabled || !isSafeAuthIndex(actionButtons[i].getAttribute("data-auth-index"));
      }
      actionButtons[i].disabled = blocked;
    }
    var modeSelects = el.filesBody.querySelectorAll("select[data-role='mode-select']");
    for (var s = 0; s < modeSelects.length; s++) {
      modeSelects[s].disabled = disabled || !isSafeAuthIndex(modeSelects[s].getAttribute("data-auth-index"));
    }
  }

  function applySecurityGate() {
    secureContext = isSecureUiContext(window.location);
    el.insecureWarning.hidden = secureContext;
    el.mgmtKey.disabled = !secureContext;
    el.keys.disabled = !secureContext;
    el.label.disabled = !secureContext;
    if (el.importMode) el.importMode.disabled = !secureContext;
    clearQuota();
    setBusy(false);
    if (secureContext) {
      setStatus("尚未加载。请输入 CPA 管理密钥并点击“连接并刷新”。", "info");
    } else {
      setStatus("当前环境不安全，已禁用密钥操作。请改用 HTTPS 或本机地址访问。", "error");
    }
  }

  function disconnect() {
    el.mgmtKey.value = "";
  }

  function currentKey() {
    return typeof el.mgmtKey.value === "string" ? el.mgmtKey.value : "";
  }

  function appendCell(row, text, className) {
    var cell = doc.createElement("td");
    if (className) {
      cell.className = className;
    }
    cell.textContent = text;
    row.appendChild(cell);
    return cell;
  }

  function buildRow(row) {
    var tr = doc.createElement("tr");
    appendCell(tr, row.name, "cell-name");
    appendCell(tr, row.label || "—");

    var modeCell = doc.createElement("td");
    modeCell.className = "cell-mode";

    var modeSelect = doc.createElement("select");
    modeSelect.className = "select-mode";
    modeSelect.setAttribute("data-role", "mode-select");
    modeSelect.setAttribute("data-auth-index", row.authIndex);
    modeSelect.setAttribute("aria-label", "修改密钥 " + toCellText(row.rawName) + " 的模式");

    if (row.modeError || !row.mode) {
      var invalidOption = doc.createElement("option");
      invalidOption.value = "";
      invalidOption.textContent = "无效 / 不可读";
      invalidOption.disabled = true;
      invalidOption.selected = true;
      modeSelect.appendChild(invalidOption);
    }

    var cliOption = doc.createElement("option");
    cliOption.value = "cli";
    cliOption.textContent = "CLI 模式 (cli)";
    if (row.mode === "cli" && !row.modeError) {
      cliOption.selected = true;
    }
    modeSelect.appendChild(cliOption);

    var providerOption = doc.createElement("option");
    providerOption.value = "provider";
    providerOption.textContent = "Provider 模式 (provider)";
    if (row.mode === "provider" && !row.modeError) {
      providerOption.selected = true;
    }
    modeSelect.appendChild(providerOption);

    modeSelect.value = row.modeError ? "" : row.mode;
    modeSelect.disabled = busy || !secureContext || !isSafeAuthIndex(row.authIndex);
    modeCell.appendChild(modeSelect);

    var saveModeBtn = doc.createElement("button");
    saveModeBtn.type = "button";
    saveModeBtn.className = "btn btn-secondary btn-sm";
    saveModeBtn.setAttribute("data-action", "save-mode");
    saveModeBtn.setAttribute("data-auth-index", row.authIndex);
    saveModeBtn.setAttribute("data-file-name", row.rawName);
    saveModeBtn.textContent = "保存模式";
    saveModeBtn.setAttribute("aria-label", "保存密钥 " + toCellText(row.rawName) + " 的模式配置");
    saveModeBtn.disabled = busy || !secureContext || !isSafeAuthIndex(row.authIndex);
    saveModeBtn.addEventListener("click", function (event) {
      onSaveModeClick(event, modeSelect);
    });
    modeCell.appendChild(saveModeBtn);

    if (row.modeError) {
      var modeBadge = doc.createElement("span");
      modeBadge.className = "mode-source-badge mode-error";
      modeBadge.textContent = "模式异常";
      modeCell.appendChild(modeBadge);
    } else if (row.modeSource === "default") {
      var modeBadge = doc.createElement("span");
      modeBadge.className = "mode-source-badge mode-default";
      modeBadge.textContent = "旧凭据默认";
      modeCell.appendChild(modeBadge);
    } else if (row.modeSource === "auth") {
      var modeBadge = doc.createElement("span");
      modeBadge.className = "mode-source-badge mode-auth";
      modeBadge.textContent = "已单独配置";
      modeCell.appendChild(modeBadge);
    }
    tr.appendChild(modeCell);

    appendCell(tr, row.status || "—");
    appendCell(tr, row.disabled ? "是" : "否");
    appendCell(tr, row.unavailable ? "不可用" : "可用", row.unavailable ? "cell-warn" : "cell-ok");
    appendCell(tr, String(row.success));
    appendCell(tr, String(row.failed));
    var actions = doc.createElement("td");
    actions.className = "cell-actions";

    var quotaLabel = row.quotaAvailable
      ? "查看密钥 " + toCellText(row.rawName) + " 的配额"
      : "密钥 " + toCellText(row.rawName) + " 缺少可用的凭据索引，无法查看配额";
    var quotaButton = doc.createElement("button");
    quotaButton.type = "button";
    quotaButton.className = "btn btn-secondary btn-sm";
    quotaButton.setAttribute("data-action", "quota");
    quotaButton.setAttribute("data-auth-index", row.authIndex);
    quotaButton.setAttribute("data-file-name", row.rawName);
    quotaButton.textContent = "查看配额";
    quotaButton.setAttribute("aria-label", quotaLabel);
    quotaButton.disabled = !row.quotaAvailable || busy || !secureContext;
    quotaButton.addEventListener("click", onQuotaClick);
    actions.appendChild(quotaButton);

    if (row.deletable) {
      var button = doc.createElement("button");
      button.type = "button";
      button.className = "btn btn-danger btn-sm";
      button.setAttribute("data-action", "delete");
      button.setAttribute("data-file-name", row.rawName);
      button.textContent = "删除";
      button.setAttribute("aria-label", "删除密钥文件 " + toCellText(row.rawName));
      button.disabled = busy || !secureContext;
      button.addEventListener("click", onDeleteClick);
      actions.appendChild(button);
    }
    tr.appendChild(actions);
    return tr;
  }

  function updateSummary(rows) {
    var total = rows.length;
    var unavailable = 0;
    var disabled = 0;
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].unavailable) {
        unavailable++;
      }
      if (rows[i].disabled) {
        disabled++;
      }
    }
    el.summary.textContent =
      "共 " + total + " 条：可用 " + (total - unavailable) + "，不可用 " + unavailable + "，已禁用 " + disabled + "。";
  }

  function renderFiles(rows) {
    el.tableWrap.classList.toggle("is-empty", rows.length === 0);
    while (el.filesBody.firstChild) {
      el.filesBody.removeChild(el.filesBody.firstChild);
    }
    if (!rows.length) {
      var emptyRow = doc.createElement("tr");
      var emptyCell = doc.createElement("td");
      emptyCell.colSpan = 9;
      emptyCell.className = "empty-cell";
      emptyCell.textContent = "暂无数据。";
      emptyRow.appendChild(emptyCell);
      el.filesBody.appendChild(emptyRow);
    } else {
      for (var i = 0; i < rows.length; i++) {
        el.filesBody.appendChild(buildRow(rows[i]));
      }
    }
    updateSummary(rows);
  }

  function readJson(response) {
    return response.text().then(function (text) {
      if (!text) {
        return {};
      }
      try {
        return JSON.parse(text);
      } catch (err) {
        return {};
      }
    });
  }

  function renderSettings(settings) {
    if (!isPlainObject(settings)) {
      return;
    }
    if (el.settingMode) {
      var effectiveMode =
        typeof settings.default_mode === "string" && settings.default_mode
          ? settings.default_mode
          : settings.mode;
      el.settingMode.textContent = effectiveMode === "provider" ? "Provider 模式" : "CLI 模式（默认）";
    }
    if (el.settingCliVersion) {
      el.settingCliVersion.textContent = toCellText(settings.cli_version) || "1.79.2";
    }
    if (el.settingBaseUrl) {
      el.settingBaseUrl.textContent = toCellText(settings.base_url) || "默认";
    }
    if (el.settingZdr) {
      el.settingZdr.textContent = settings.zdr === true ? "已开启" : "未开启";
    }
    if (el.settingCatalog) {
      if (isPlainObject(settings.catalog)) {
        var count = toCount(settings.catalog.count);
        var source = toCellText(settings.catalog.source);
        el.settingCatalog.textContent = "已加载 " + count + " 个模型" + (source ? "（来源: " + source + "）" : "");
        if (el.settingStale) {
          el.settingStale.hidden = settings.catalog.stale !== true;
        }
      } else {
        el.settingCatalog.textContent = "默认目录";
        if (el.settingStale) {
          el.settingStale.hidden = true;
        }
      }
    }
  }

  function refreshList() {
    if (busy || !secureContext) {
      return;
    }
    var managementKey = currentKey();
    if (!managementKey) {
      setStatus("请先输入 CPA 管理密钥。", "error");
      el.mgmtKey.focus();
      return;
    }

    clearQuota();
    setBusy(true);
    setStatus("正在加载密钥列表…", "info");

    fetch(endpoints.keysUrl, buildManagementRequest(managementKey, { method: "GET" }))
      .then(function (response) {
        if (response.status === 401 || response.status === 403) {
          disconnect();
          setStatus(describeHttpFailure(response.status), "error");
          return null;
        }
        if (!response.ok) {
          setStatus(describeHttpFailure(response.status), "error");
          return null;
        }
        return readJson(response);
      })
      .then(function (payload) {
        if (!payload) {
          return;
        }
        var rows = normalizeFileEntries(isPlainObject(payload) ? payload.files : []);
        renderFiles(rows);
        setStatus(rows.length ? "已加载 " + rows.length + " 条密钥。" : "列表为空，暂无已托管的密钥。", "ok");
      })
      .catch(function () {
        setStatus("无法连接 CPA 管理接口，请确认地址与网络。", "error");
      })
      .then(function () {
        managementKey = "";
        setBusy(false);
        if (!modelsDirty && currentKey()) {
          loadSettings();
        }
      });
  }

  function importKeys() {
    if (busy || !secureContext) {
      return;
    }
    var managementKey = currentKey();
    if (!managementKey) {
      setStatus("请先输入 CPA 管理密钥（管理认证）。", "error");
      el.mgmtKey.focus();
      return;
    }

    var labelCheck = validateLabel(el.label.value);
    if (!labelCheck.ok) {
      setStatus(
        labelCheck.reason === "too-long"
          ? "标签过长（上限 " + MAX_LABEL_LENGTH + " 个字符）。"
          : "标签包含非法控制字符。",
        "error"
      );
      return;
    }

    var parsed = parseKeys(el.keys.value);
    var notice = countNotice(parsed.counts);
    if (parsed.error) {
      setStatus(parsed.error.message, "error");
      return;
    }
    if (parsed.counts.valid === 0) {
      setStatus("没有可导入的有效密钥" + notice + "。", "error");
      return;
    }

    var importMode = el.importMode && el.importMode.value ? el.importMode.value : "cli";
    if (importMode !== "cli" && importMode !== "provider") {
      setStatus("导入模式无效，仅支持 CLI 模式或 Provider 模式。", "error");
      return;
    }
    var payload = buildImportPayload(parsed.keys, labelCheck.value, importMode);
    var expectedCount = parsed.counts.valid;
    parsed.keys.length = 0;
    if (!payload.ok) {
      payload.body = "";
      setStatus("请求体过大，请减少密钥数量后重试。", "error");
      return;
    }

    clearQuota();
    setBusy(true);
    setStatus("正在导入 " + parsed.counts.valid + " 条密钥…" + notice, "info");

    fetch(endpoints.keysUrl, buildManagementRequest(managementKey, { method: "POST", body: payload.body }))
      .then(function (response) {
        if (response.status === 401 || response.status === 403) {
          disconnect();
          setStatus(describeHttpFailure(response.status), "error");
          return null;
        }
        if (!response.ok) {
          setStatus(describeHttpFailure(response.status), "error");
          return null;
        }
        return readJson(response).then(function (resultPayload) {
          var result = formatImportResult(response.status, resultPayload, expectedCount);
          if (result.ok && result.failed === 0) {
            el.keys.value = "";
          }
          var hint = result.ok ? " 请点击“刷新列表”查看最新状态。" : "";
          setStatus(result.summary + notice + hint, result.level);
        });
      })
      .catch(function () {
        setStatus("无法确认导入结果，请检查网络并刷新列表后再决定是否重试。", "error");
      })
      .then(function () {
        managementKey = "";
        payload.body = "";
        setBusy(false);
      });
  }

  function onSaveModeClick(event, selectElem) {
    if (busy || !secureContext) {
      return;
    }
    var button = (event && event.currentTarget) || (event && event.target);
    var authIndex = button ? button.getAttribute("data-auth-index") : "";
    var fileName = button ? button.getAttribute("data-file-name") : "";
    var select = selectElem;
    if (!select && button && button.parentNode) {
      var list = button.parentNode.querySelectorAll("select");
      if (list && list.length > 0) {
        select = list[0];
      }
    }
    var selectedMode = select ? select.value : "";
    saveMode(authIndex, selectedMode, fileName);
  }

  function saveMode(authIndex, selectedMode, fileName) {
    if (busy || !secureContext) {
      return;
    }
    if (!isSafeAuthIndex(authIndex)) {
      setStatus("凭据索引无效，无法保存模式。", "error");
      return;
    }
    if (selectedMode !== "cli" && selectedMode !== "provider") {
      setStatus("请先选择有效的模式（CLI 或 Provider）。", "error");
      return;
    }
    var managementKey = currentKey();
    if (!managementKey) {
      setStatus("请先输入 CPA 管理密钥。", "error");
      el.mgmtKey.focus();
      return;
    }

    var patchPayload = JSON.stringify({ auth_index: authIndex.trim(), mode: selectedMode });

    clearQuota();
    setBusy(true);
    setStatus("正在保存模式配置…", "info");

    fetch(endpoints.keysUrl, buildManagementRequest(managementKey, { method: "PATCH", body: patchPayload }))
      .then(function (response) {
        if (response.status === 401 || response.status === 403) {
          disconnect();
          setStatus(describeHttpFailure(response.status), "error");
          return null;
        }
        if (!response.ok) {
          setStatus(describeHttpFailure(response.status), "error");
          return null;
        }
        return readJson(response);
      })
      .then(function (patchResult) {
        if (!patchResult) {
          return null;
        }
        setStatus("模式已提交，正在确认生效…", "info");
        return fetch(endpoints.keysUrl, buildManagementRequest(managementKey, { method: "GET" }))
          .then(function (getResponse) {
            if (getResponse.status === 401 || getResponse.status === 403) {
              disconnect();
              setStatus(describeHttpFailure(getResponse.status), "error");
              return null;
            }
            if (!getResponse.ok) {
              setStatus("模式已提交，但无法获取最新列表以确认生效（" + getResponse.status + "）。", "error");
              return null;
            }
            return readJson(getResponse);
          })
          .then(function (getPayload) {
            if (!getPayload) {
              return;
            }
            var confirmed = false;
            var rows = null;
            if (isPlainObject(getPayload) && Array.isArray(getPayload.files)) {
              rows = normalizeFileEntries(getPayload.files);
              for (var i = 0; i < rows.length; i++) {
                if (rows[i].authIndex === authIndex) {
                  if (rows[i].mode === selectedMode && rows[i].modeSource === "auth" && !rows[i].modeError) {
                    confirmed = true;
                  }
                  break;
                }
              }
            }
            if (confirmed && rows) {
              renderFiles(rows);
              setStatus("密钥模式已更新为 " + (selectedMode === "provider" ? "Provider 模式" : "CLI 模式") + " 并确认生效。", "ok");
            } else {
              setStatus("模式配置未能确认生效，请核对凭据状态或刷新列表。", "error");
            }
          });
      })
      .catch(function () {
        setStatus("无法连接 CPA 管理接口，请确认地址与网络。", "error");
      })
      .then(function () {
        managementKey = "";
        patchPayload = "";
        setBusy(false);
      });
  }

  function onDeleteClick(event) {
    if (busy || !secureContext) {
      return;
    }
    var button = event.currentTarget;
    var fileName = button.getAttribute("data-file-name");
    if (!isSafeFileName(fileName)) {
      setStatus("文件名无效，无法删除。", "error");
      return;
    }
    var managementKey = currentKey();
    if (!managementKey) {
      setStatus("请先输入 CPA 管理密钥。", "error");
      el.mgmtKey.focus();
      return;
    }
    if (!window.confirm('确定删除密钥文件 "' + toCellText(fileName) + '" 吗？此操作不可撤销。')) {
      return;
    }

    clearQuota();
    setBusy(true);
    setStatus("正在删除…", "info");
    var url = endpoints.credentialsUrl + "?name=" + encodeURIComponent(fileName);

    fetch(url, buildManagementRequest(managementKey, { method: "DELETE" }))
      .then(function (response) {
        if (response.status === 401 || response.status === 403) {
          disconnect();
          setStatus(describeHttpFailure(response.status), "error");
          return;
        }
        if (!response.ok) {
          setStatus(describeHttpFailure(response.status), "error");
          return;
        }
        setStatus("已删除。列表可能已过期，请点击“刷新列表”。", "ok");
      })
      .catch(function () {
        setStatus("无法连接 CPA 管理接口，请确认地址与网络。", "error");
      })
      .then(function () {
        managementKey = "";
        setBusy(false);
      });
  }

  function renderQuota(parsed, name) {
    var subParts = [];
    if (parsed.subscription && parsed.subscription.plan) {
      subParts.push("订阅计划：" + parsed.subscription.plan);
    }
    if (parsed.subscription && parsed.subscription.status) {
      subParts.push("状态：" + parsed.subscription.status);
    }
    el.quotaSelection.textContent = "已选择密钥：" + (name || "—") + (subParts.length ? "；" + subParts.join("，") : "") + "。";

    if (parsed.summary && el.quotaSummary) {
      el.quotaCreditsTotal.textContent = parsed.summary.totalCredits !== null ? parsed.summary.totalCredits + " credits" : "—";
      el.quotaCreditsMonthly.textContent = parsed.summary.monthlyCredits !== null ? parsed.summary.monthlyCredits + " credits" : "—";
      el.quotaCreditsPurchased.textContent = parsed.summary.purchasedCredits !== null ? parsed.summary.purchasedCredits + " credits" : "—";
      el.quotaCreditsFree.textContent = parsed.summary.freeCredits !== null ? parsed.summary.freeCredits + " credits" : "—";
      el.quotaSummary.hidden = false;
    } else if (el.quotaSummary) {
      el.quotaSummary.hidden = true;
    }

    while (el.quotaBody.firstChild) {
      el.quotaBody.removeChild(el.quotaBody.firstChild);
    }

    if (parsed.buckets.length > 0) {
      if (el.quotaTableWrap) el.quotaTableWrap.hidden = false;
      if (el.quotaNoBuckets) el.quotaNoBuckets.hidden = true;
      for (var i = 0; i < parsed.buckets.length; i++) {
        var bucket = parsed.buckets[i];
        var tr = doc.createElement("tr");
        appendCell(tr, bucket.label);
        appendCell(tr, bucket.remainingPercent + "%");
        appendCell(tr, bucket.usedPercent + "%");
        var progressCell = doc.createElement("td");
        var progress = doc.createElement("progress");
        progress.max = 1;
        progress.value = bucket.remainingFraction;
        progress.setAttribute("aria-label", bucket.label + "剩余 " + bucket.remainingPercent + "%");
        progress.textContent = bucket.remainingPercent + "%";
        progressCell.appendChild(progress);
        tr.appendChild(progressCell);
        appendCell(tr, bucket.resetTime || "—");
        el.quotaBody.appendChild(tr);
      }
    } else {
      if (el.quotaTableWrap) el.quotaTableWrap.hidden = true;
      if (el.quotaNoBuckets) el.quotaNoBuckets.hidden = false;
    }

    el.quotaPanel.hidden = false;
  }

  function onQuotaClick(event) {
    if (busy || !secureContext) {
      return;
    }
    var button = event.currentTarget;
    var authIndex = button.getAttribute("data-auth-index");
    if (!isSafeAuthIndex(authIndex)) {
      clearQuota();
      setQuotaStatus("该密钥缺少可用的凭据索引，无法查看配额。", "error");
      return;
    }
    var managementKey = currentKey();
    if (!managementKey) {
      setStatus("请先输入 CPA 管理密钥。", "error");
      el.mgmtKey.focus();
      return;
    }
    var payload = buildQuotaPayload(authIndex);
    var rowName = toCellText(button.getAttribute("data-file-name"));

    clearQuota();
    setBusy(true);
    setQuotaStatus("正在查询配额…", "info");

    fetch(endpoints.quotaUrl, buildManagementRequest(managementKey, { method: "POST", body: payload.body }))
      .then(function (response) {
        if (response.status === 401 || response.status === 403) {
          disconnect();
          clearQuota();
          setQuotaStatus(describeHttpFailure(response.status), "error");
          return null;
        }
        if (!response.ok) {
          setQuotaStatus(describeQuotaFailure(response.status), "error");
          return null;
        }
        return readJson(response);
      })
      .then(function (result) {
        if (result === null) {
          return;
        }
        var parsed = parseQuotaResponse(result);
        if (!parsed.ok) {
          setQuotaStatus("配额数据不可用，未显示任何配额信息。", "error");
          return;
        }
        renderQuota(parsed, rowName);
        setQuotaStatus("已加载所选密钥的配额。", "ok");
      })
      .catch(function () {
        setQuotaStatus("无法连接 CPA 管理接口，请确认地址与网络。", "error");
      })
      .then(function () {
        managementKey = "";
        payload.body = "";
        setBusy(false);
      });
  }

  function renderManualModels() {
    if (!modelBody) return;
    while (modelBody.firstChild) modelBody.removeChild(modelBody.firstChild);
    if (!manualDraft.length) {
      var empty = doc.createElement("tr");
      var td = appendCell(empty, modelsLoaded ? "未手动注册模型。" : "尚未加载。", "empty-cell");
      td.colSpan = 3;
      modelBody.appendChild(empty);
    }
    manualDraft.forEach(function (model, index) {
      var row = doc.createElement("tr");
      appendCell(row, model.id, "cell-name");
      appendCell(row, model.protocol || "自动 / 缺省");
      var actions = doc.createElement("td");
      var remove = doc.createElement("button");
      remove.type = "button";
      remove.className = "btn btn-danger btn-sm";
      remove.textContent = "删除";
      remove.disabled = busy || !secureContext;
      remove.addEventListener("click", function () {
        if (busy || !secureContext) return;
        manualDraft.splice(index, 1);
        modelsDirty = true;
        renderManualModels();
        if (modelStatus) modelStatus.textContent = "有未保存的模型草稿。";
        setBusy(false);
      });
      actions.appendChild(remove);
      row.appendChild(actions);
      modelBody.appendChild(row);
    });
  }

  async function manualRequest(url, method, payload, key, signal) {
    if (currentKey() !== key || signal.aborted) throw new Error("连接已变更或操作超时。");
    var init = buildManagementRequest(key, { method: method, body: payload === undefined ? undefined : JSON.stringify(payload) });
    init.signal = signal;
    init.redirect = "error";
    var response = await fetch(url, init);
    if (currentKey() !== key || signal.aborted) throw new Error("连接已变更或操作超时。");
    if (response.status === 401 || response.status === 403) {
      disconnect();
      throw new Error("管理认证失败，请核对管理密钥。");
    }
    if (!response.ok) {
      var err = new Error("管理请求失败（HTTP " + response.status + "）。");
      err.status = response.status;
      throw err;
    }
    return response.json();
  }

  async function loadSettings() {
    if (busy || !secureContext || !currentKey()) return;
    var key = currentKey();
    var controller = new AbortController();
    var timer = window.setTimeout(function () { controller.abort(); }, 15000);
    setBusy(true);
    if (modelStatus) modelStatus.textContent = "正在加载生效设置与模型…";
    try {
      var settings = await manualRequest(endpoints.settingsUrl, "GET", undefined, key, controller.signal);
      if (currentKey() !== key || controller.signal.aborted) throw new Error("连接已变更或操作超时。");
      renderSettings(settings);
      manualDraft = normalizeManualModels(settings["manual-models"]);
      modelsLoaded = true;
      modelsDirty = false;
      renderManualModels();
      if (modelStatus) modelStatus.textContent = "已加载当前生效的手动模型。";
    } catch (_) {
      if (modelStatus) modelStatus.textContent = "加载失败，现有草稿保留；请核对管理认证与插件状态。";
    } finally {
      key = "";
      window.clearTimeout(timer);
      setBusy(false);
    }
  }

  async function saveManualModels() {
    if (busy || !secureContext || !modelsLoaded || !modelsDirty || !currentKey()) return;
    var patch;
    try {
      patch = { "manual-models": normalizeManualModels(manualDraft) };
    } catch (err) {
      if (modelStatus) modelStatus.textContent = err.message;
      return;
    }
    var key = currentKey();
    var controller = new AbortController();
    var timer = window.setTimeout(function () { controller.abort(); }, 15000);
    var writeAttempted = false;
    setBusy(true);
    if (modelStatus) modelStatus.textContent = "正在校验并保存…";
    try {
      var valid = await manualRequest(endpoints.validateUrl, "POST", patch, key, controller.signal);
      if (valid.valid !== true) throw new Error("服务端未确认校验通过。");
      writeAttempted = true;
      await manualRequest(endpoints.configUrl, "PATCH", patch, key, controller.signal);
      var confirmed = false;
      for (var i = 0; i <= 20; i++) {
        try {
          var values = await Promise.all([
            manualRequest(endpoints.configUrl, "GET", undefined, key, controller.signal),
            manualRequest(endpoints.settingsUrl, "GET", undefined, key, controller.signal)
          ]);
          var saved = normalizeManualModels(values[0]["manual-models"]);
          var effective = normalizeManualModels(values[1]["manual-models"]);
          if (
            JSON.stringify(saved) === JSON.stringify(patch["manual-models"]) &&
            JSON.stringify(effective) === JSON.stringify(patch["manual-models"])
          ) {
            confirmed = true;
            renderSettings(values[1]);
            break;
          }
        } catch (err) {
          if (err.status !== 503) throw err;
        }
        if (controller.signal.aborted || currentKey() !== key) throw new Error("确认超时或连接已变更。");
        if (i < 20) await new Promise(function (resolve) { window.setTimeout(resolve, 500); });
      }
      if (!confirmed || controller.signal.aborted || currentKey() !== key) throw new Error("未确认配置生效。");
      manualDraft = patch["manual-models"];
      modelsDirty = false;
      renderManualModels();
      if (modelStatus) modelStatus.textContent = "已保存并确认手动模型配置生效。";
    } catch (_) {
      if (modelStatus) {
        modelStatus.textContent = writeAttempted
          ? "可能已写入，但未确认生效；草稿保留，请重新加载核对。"
          : "校验或保存失败，未发出写入请求；草稿保留。";
      }
    } finally {
      key = "";
      window.clearTimeout(timer);
      setBusy(false);
    }
  }

  if (modelAdd) {
    modelAdd.addEventListener("click", function () {
      if (busy || !secureContext || !modelsLoaded) return;
      try {
        manualDraft = normalizeManualModels(manualDraft.concat([{ id: modelId.value, protocol: modelProtocol.value }]));
        modelsDirty = true;
        modelId.value = "";
        renderManualModels();
        if (modelStatus) modelStatus.textContent = "有未保存的模型草稿。";
        setBusy(false);
      } catch (err) {
        if (modelStatus) modelStatus.textContent = err.message;
      }
    });
  }

  if (modelSave) modelSave.addEventListener("click", saveManualModels);
  if (modelReload) modelReload.addEventListener("click", loadSettings);

  el.mgmtKey.addEventListener("input", function () {
    manualDraft = [];
    modelsLoaded = false;
    modelsDirty = false;
    renderManualModels();
    setBusy(busy);
  });

  el.authForm.addEventListener("submit", function (event) {
    event.preventDefault();
    refreshList();
  });

  el.importForm.addEventListener("submit", function (event) {
    event.preventDefault();
    importKeys();
  });

  el.reloadBtn.addEventListener("click", function () {
    refreshList();
  });

  applySecurityGate();
  if (secureContext) {
    setStatus("尚未加载。请输入 CPA 管理密钥并点击“连接并刷新”。", "info");
  }
}

if (typeof document !== "undefined" && typeof window !== "undefined" && document.getElementById) {
  initTheme();
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", initUi);
  } else {
    initUi();
  }
}
