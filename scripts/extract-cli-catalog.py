#!/usr/bin/env python3
"""Extract the reviewed CLI model catalog snapshot from an installed command-code
bundle, statically, without evaluating JS.

One assigned client object literal holds the facts; a structural signature finds
that single map and only it is scanned. Supported: identifier keys, quoted
strings, decimal/scientific integers, ``!0``/``!1``/true/false/null, flat scalar
arrays, empty ``get NAME(){...}`` accessors (skipped). Templates, comments,
regexes, unknown escapes, nested objects, parameterised getters, ambiguous and
partial maps fail closed. Entries record only OUTPUT_FIELDS in canonical order
from their own balanced span; strings decode as JSON after normalizing JS-only
``\\xHH``/``\\'``/``\\v``/``\\0`` (other escapes and lone surrogates are errors).

    extract --bundle <cli>/dist/cli.mjs --package-json <cli>/package.json \\
        --out internal/catalogdata/cli.json --date "$(date -u +%F)"
    check --bundle <cli>/dist/cli.mjs --snapshot internal/catalogdata/cli.json
"""
from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from datetime import date, datetime, timezone
from pathlib import Path

SOURCE_LINE = "Installed command-code CLI static catalog, facts only"
MIN_ENTRIES, SIGNATURE_WINDOW = 40, 256
OUTPUT_FIELDS = ("id", "name", "inputModalities", "reasoningEfforts", "contextWindow", "maxOutputTokens", "reasoning")
IDENT = r"[A-Za-z_$][\w$]*"
ASSIGN = re.compile(IDENT + r"=\s*\{")
FIRST_ENTRY = re.compile(r"{\s*" + IDENT + r"\s*:\s*{\s*id\s*:\s*\"")
PROP_KEY = re.compile(r"(" + IDENT + r")\s*:")
GETTER = re.compile(r"get\s+" + IDENT + r"\s*\(")
BOOL_NULL = re.compile(r"true|false|null")
ID_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9./:_%@+~-]*$")
VERSION_PATTERN = re.compile(r"^\d+(\.\d+){1,3}$")
STRING_TOKEN = re.compile(r'\\(?:x[0-9A-Fa-f]{2}|.)|"')
JS_ONLY_ESCAPES = {"\\'": "'", "\\v": "\\u000b", "\\0": "\\u0000", '"': '\\"'}


class ExtractError(Exception):
    """Raised for unreproducible input: no map, ambiguity or unsupported literal."""


def _skip_string(source: str, i: int, quote: str) -> int:
    """Return the index after the closing quote of a quoted string."""
    i += 1
    while i < len(source):
        if source[i] == "\\":
            i += 2
        elif source[i] == quote:
            return i + 1
        else:
            i += 1
    raise ExtractError(f"{i}: unterminated string literal")


def _skip_braces(source: str, i: int, open_ch: str, close_ch: str) -> int:
    """Return the index after the balanced close; strings stay opaque."""
    depth = 1
    i += 1
    while i < len(source):
        if source[i] in "\"'":
            i = _skip_string(source, i, source[i])
            continue
        depth += (source[i] == open_ch) - (source[i] == close_ch)
        if depth == 0:
            return i + 1
        i += 1
    raise ExtractError(f"{i}: unbalanced {open_ch}{close_ch} region")


def _unescape(raw: str, offset: int) -> str:
    """Decode a string body as JSON after normalizing the few JS-only escapes."""
    def normalize(match):
        token = match.group()
        if token == "\\0" and raw[match.end():match.end() + 1].isdigit():
            raise ExtractError(f"{offset}: legacy octal escape is not supported")
        return "\\u00" + token[2:] if token.startswith("\\x") and len(token) == 4 else JS_ONLY_ESCAPES.get(token, token)
    # Match whole escape tokens: an escaped backslash must never make the
    # following literal x/u/v/0 into a new escape on a second replacement pass.
    body = STRING_TOKEN.sub(normalize, raw)
    try:
        text = json.loads('"' + body + '"')
    except json.JSONDecodeError as exc:
        raise ExtractError(f"{offset}: unsupported or malformed escape in string ({exc.msg})") from exc
    if any(0xD800 <= ord(ch) <= 0xDFFF for ch in text):
        raise ExtractError(f"{offset}: lone surrogate is not a valid catalog string")
    return text


def _number(token: str, offset: int) -> int:
    """Integer fact numbers: plain decimal or scientific short forms like 105e4."""
    try:
        return int(token, 10)
    except ValueError:
        pass
    try:
        value = float(token)
    except ValueError as exc:
        raise ExtractError(f"{offset}: bad number {token!r}") from exc
    if not (value.is_integer() and abs(value) < 2 ** 53):
        raise ExtractError(f"{offset}: non-integer catalog number {token!r}")
    return int(value)


def _parse_scalar(source: str, i: int):
    """Parse one literal value; returns ``(kind, value, end)`` or fails closed."""
    c = source[i]
    if c in "\"'":
        end = _skip_string(source, i, c)
        return "string", _unescape(source[i + 1:end - 1], i), end
    if source.startswith("!0", i):
        return "boolean", True, i + 2
    if source.startswith("!1", i):
        return "boolean", False, i + 2
    if m := BOOL_NULL.match(source, i):
        return ("null", None, m.end()) if m.group(0) == "null" else ("boolean", m.group(0) == "true", m.end())
    if m := re.match(r"-?\d[\w.]*", source[i:]):
        return "number", _number(m.group(0), i), i + m.end()
    if c == "{":
        return "object", None, _skip_braces(source, i, "{", "}")
    if c == "[":
        end = _skip_braces(source, i, "[", "]")
        items, j = [], i + 1
        while j < end - 1:
            if source[j].isspace() or source[j] == ",":
                j += 1
                continue
            kind, value, j = _parse_scalar(source, j)
            if kind not in ("string", "number", "boolean", "null"):
                raise ExtractError(f"{j}: {kind} value inside array is not a static fact")
            items.append(value)
        return "array", items, end
    if m := re.match(IDENT, source[i:]):
        return "identifier", m.group(0), i + m.end()
    raise ExtractError(f"{i}: unsupported catalog literal ({source[i:i+12]!r})")


def _parse_object(source: str, start: int, end: int, allow_objects: bool):
    """Parse the object literal at ``start`` into ``(key, kind, value, start, end)``."""
    props, seen = [], set()
    i = start + 1
    while i < end:
        c = source[i]
        if c.isspace() or c == ",":
            i += 1
        elif c == "}":
            return props
        elif g := GETTER.match(source, i):
            # get hidden(){return!isXxx()}: runtime accessor, skipped and never
            # recorded; a parameterised getter is not a static fact.
            close = source.index(")", g.end() - 1, end)
            if source[g.end():close].strip():
                raise ExtractError(f"{i}: getter with parameters is not a static value")
            i = _skip_braces(source, source.index("{", close + 1, end), "{", "}")
        elif not (m := PROP_KEY.match(source, i, end)):
            raise ExtractError(f"{i}: unsupported object property ({source[i:i+16]!r})")
        else:
            key = m.group(1)
            if key in seen:
                raise ExtractError(f"{i}: duplicate property {key!r}")
            seen.add(key)
            j = m.end(1)
            while source[j] in " \t\r\n:":
                j += 1
            value_start = j
            kind, value, j = _parse_scalar(source, j)
            if kind == "object" and not allow_objects:
                raise ExtractError(f"{i}: nested object value for {key!r} is not a static fact")
            props.append((key, kind, value, value_start, j))
            if source[j:end].lstrip()[:1] not in (",", "}"):
                raise ExtractError(f"{j}: expected a comma or object close")
            i = j
    raise ExtractError(f"{start}: unterminated object literal")


def _locate_map(source: str) -> tuple[int, int]:
    """Return the span of the single object literal holding the model facts."""
    found = []
    for m in ASSIGN.finditer(source):
        open_brace = m.end() - 1
        if FIRST_ENTRY.match(source[open_brace:open_brace + SIGNATURE_WINDOW]) and \
                "inputModalities:" in source[m.end():m.end() + SIGNATURE_WINDOW]:
            found.append((open_brace, _skip_braces(source, open_brace, "{", "}")))
    if not found:
        raise ExtractError("no object-literal model map matches the structural signature")
    if len(found) > 1:
        raise ExtractError(f"ambiguous model maps: {found}")
    start, end = found[0]
    if sum(kind == "object" for _k, kind, _v, _s, _e in _parse_object(source, start, end, True)) < MIN_ENTRIES:
        raise ExtractError("signature matched too few entries; refusing a partial catalog")
    return start, end


def _validate_entry(model: dict, key: str) -> None:
    """Reject unreviewed fields and any fact outside the reviewed schema."""
    if unknown := set(model) - set(OUTPUT_FIELDS):
        raise ExtractError(f"entry {key!r} carries unreviewed fields {sorted(unknown)}")
    for required in ("id", "name"):
        if not isinstance(model.get(required), str) or not model[required]:
            raise ExtractError(f"entry {key!r} has no readable {required}")
    if not ID_PATTERN.match(model["id"]):
        raise ExtractError(f"entry {key!r} has invalid id {model['id']!r}")
    for field in ("inputModalities", "reasoningEfforts"):
        values = model.get(field)
        if field in model and (not isinstance(values, list) or not all(isinstance(v, str) and v for v in values)
                               or (field == "inputModalities" and len(set(values)) != len(values))):
            raise ExtractError(f"entry {model['id']!r} has invalid {field}")
    for field in ("contextWindow", "maxOutputTokens"):
        value = model.get(field)
        if field in model and (isinstance(value, bool) or not isinstance(value, int) or value <= 0):
            raise ExtractError(f"entry {model['id']!r} has invalid {field} {value!r}")
    if "reasoning" in model and not isinstance(model["reasoning"], bool):
        raise ExtractError(f"entry {model['id']!r} has invalid reasoning {model['reasoning']!r}")


def _validate_models(models, label: str) -> list[dict]:
    """Validate a complete list: entries, reviewed fields, unique ids."""
    if not isinstance(models, list) or not models:
        raise ExtractError(f"{label} has no model list")
    seen = set()
    for model in models:
        if not isinstance(model, dict):
            raise ExtractError(f"{label} contains a non-object model entry")
        _validate_entry(model, str(model.get("id")))
        if model["id"] in seen:
            raise ExtractError(f"{label} contains duplicate model id {model['id']!r}")
        seen.add(model["id"])
    return models


def extract_models(source: str) -> list[dict]:
    """Extract the model facts in bundle order with object-boundary strictness."""
    start, end = _locate_map(source)
    models = []
    for key, kind, _value, body_start, body_end in _parse_object(source, start, end, True):
        if kind != "object":
            raise ExtractError(f"entry {key!r} is not a plain KEY:{{...}} block")
        facts = {field: value for field, k, value, _s, _e in _parse_object(source, body_start, body_end, False)
                 if field in OUTPUT_FIELDS and k != "identifier"}
        models.append({field: facts[field] for field in OUTPUT_FIELDS if field in facts})
    return _validate_models(models, "bundle")


def resolve_version(args_version: str, package_json: str | None, source: str) -> str:
    """Resolve the CLI version: explicit flag, else the package.json sibling."""
    version = (args_version or "").strip()
    if not version and package_json:
        try:
            version = str(json.loads(Path(package_json).read_text(encoding="utf-8")).get("version") or "").strip()
        except (OSError, json.JSONDecodeError) as exc:
            raise ExtractError(f"cannot read --package-json {package_json}: {exc}") from exc
    if not version or not VERSION_PATTERN.match(version):
        raise ExtractError(f"missing or malformed version {version!r}: pass --version or --package-json")
    if version not in source:
        raise ExtractError(f"version {version!r} does not appear in the bundle text")
    return version


def build_snapshot(source: bytes, version: str, date_value: str) -> dict:
    try:
        if date.fromisoformat(date_value).isoformat() != date_value:
            raise ValueError(date_value)
    except (TypeError, ValueError) as exc:
        raise ExtractError(f"date {date_value!r} is not a valid YYYY-MM-DD calendar date") from exc
    return {"source": SOURCE_LINE, "cli_version": version, "bundle_sha256": hashlib.sha256(source).hexdigest(),
            "date": date_value, "models": extract_models(source.decode("utf-8"))}


def _read_bytes(path: str) -> bytes:
    """Read a UTF-8 text artifact, reporting the path instead of a traceback."""
    try:
        data = Path(path).read_bytes()
        data.decode("utf-8")
        return data
    except OSError as exc:
        raise ExtractError(f"cannot read {path}: {exc}") from exc
    except UnicodeDecodeError as exc:
        raise ExtractError(f"{path} is not UTF-8 text: {exc}") from exc


def diff_report(old: dict, new: dict) -> str:
    """Human readable full-fact diff of two snapshots (order included)."""
    old_models = {m["id"]: m for m in old.get("models") or []}
    new_models = {m["id"]: m for m in new.get("models") or []}
    added, removed = sorted(set(new_models) - set(old_models)), sorted(set(old_models) - set(new_models))
    changed = [f"{mid}: " + "; ".join(f"{f} {old_models[mid].get(f)!r} -> {new_models[mid].get(f)!r}"
                                      for f in OUTPUT_FIELDS if old_models[mid].get(f) != new_models[mid].get(f))
               for mid in new_models if mid in old_models and old_models[mid] != new_models[mid]]
    lines = [f"added %d: %s" % (len(added), ", ".join(added) or "(none)"),
             f"removed %d: %s" % (len(removed), ", ".join(removed) or "(none)"),
             f"changed {len(changed)}:"]
    lines += [f"  {note}" for note in changed]
    if not changed and list(old_models) != list(new_models) and not added and not removed:
        lines.append("  model order differs from the bundle")
    lines.append(f"unchanged {len(new_models) - len(added) - len(changed)}")
    return "\n".join(lines)


def _command_extract(args: argparse.Namespace) -> int:
    source = _read_bytes(args.bundle)
    version = resolve_version(args.version, args.package_json, source.decode("utf-8"))
    snapshot = build_snapshot(source, version, args.date or datetime.now(timezone.utc).strftime("%Y-%m-%d"))
    if args.diff:
        try:
            previous = json.loads(Path(args.diff).read_text(encoding="utf-8"))
            print(f"snapshot candidate diff: {previous.get('cli_version')} -> {version}")
            print(diff_report(previous, snapshot))
        except (OSError, json.JSONDecodeError) as exc:
            print(f"diff skipped: {exc}", file=sys.stderr)
    rendered = json.dumps(snapshot, indent=2, ensure_ascii=False) + "\n"
    if not args.out:
        print(rendered, end="")
        return 0
    out_path = Path(args.out)
    try:
        current = out_path.read_text(encoding="utf-8") if out_path.exists() else None
        if current != rendered:
            out_path.write_text(rendered, encoding="utf-8")
    except OSError as exc:
        raise ExtractError(f"cannot write snapshot {args.out}: {exc}") from exc
    state = "already matches" if current == rendered else "updated with"
    print(f"{args.out} {state} cli {version}, sha {snapshot['bundle_sha256']}, {len(snapshot['models'])} models")
    return 0


def _command_check(args: argparse.Namespace) -> int:
    try:
        snapshot = json.loads(_read_bytes(args.snapshot).decode("utf-8"))
        old_models = _validate_models(snapshot.get("models"), "snapshot")
    except (ExtractError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        print(f"error: snapshot {args.snapshot} is unusable: {exc}", file=sys.stderr)
        return 1
    source = _read_bytes(args.bundle)
    version = resolve_version(str(args.version or snapshot.get("cli_version")), None, source.decode("utf-8"))
    fresh = build_snapshot(source, version, snapshot.get("date") or "")
    actual_sha = hashlib.sha256(source).hexdigest()
    problems = []
    if snapshot.get("cli_version") != version:
        problems.append(f"cli_version {snapshot.get('cli_version')!r} != bundle {version!r}")
    if snapshot.get("bundle_sha256") != actual_sha:
        problems.append(f"bundle_sha256 {snapshot.get('bundle_sha256')!r} != bundle {actual_sha!r}")
    facts_differ = old_models != fresh["models"]
    if problems or facts_differ:
        print("MISMATCH bundle vs snapshot:", "; ".join(problems) or "model facts differ", file=sys.stderr)
        if facts_differ:
            print(diff_report(snapshot, fresh), file=sys.stderr)
        return 1
    print(f"catalog snapshot verified: cli {version}, sha {actual_sha}, {len(fresh['models'])} models")
    return 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    p = sub.add_parser("extract", help="build a cli.json snapshot from a bundle")
    for flag, helptext in (("--bundle", "path to the installed dist/cli.mjs"),
                           ("--version", "CLI version to record; verified against the bundle"),
                           ("--package-json", "command-code package.json to take the version from"),
                           ("--out", "write the snapshot here (default: stdout)"),
                           ("--date", "snapshot date, YYYY-MM-DD (default: today UTC)"),
                           ("--diff", "existing snapshot to diff the fresh facts against")):
        p.add_argument(flag, required=flag == "--bundle", help=helptext)
    p = sub.add_parser("check", help="re-verify a snapshot against a bundle")
    p.add_argument("--bundle", required=True, help="path to the installed dist/cli.mjs")
    p.add_argument("--snapshot", required=True, help="path to the snapshot cli.json to verify")
    p.add_argument("--version", help="expected CLI version; default: snapshot cli_version")
    args = parser.parse_args(argv)
    try:
        return _command_extract(args) if args.command == "extract" else _command_check(args)
    except ExtractError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
