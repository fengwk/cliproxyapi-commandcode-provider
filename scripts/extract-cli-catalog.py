#!/usr/bin/env python3
#!/usr/bin/env python3
"""Extract the reviewed CLI model catalog snapshot from an installed command-code
bundle, statically, without evaluating any JS.

The facts live in one client object literal assigned to an identifier:
``IDENT={KEY:{id:"...",name:"...",...},...}``. It is located by a structural
signature (assigned object whose first entry exposes a quoted ``id`` plus
``inputModalities`` nearby) and only that map region is scanned. Supported is
just the minimal minified-literal subset; everything else fails closed:
identifier keys, quoted strings with escapes, decimal and scientific integer
numbers, ``!0``/``!1``, true/false/null, flat scalar arrays; empty
``get NAME(){...}`` accessors are skipped without recording; templates,
nested objects, comments and regexes fail instead of being guessed. Entries
record only the OUTPUT_FIELDS in canonical order from their own balanced
span, so missing properties stay absent and no values cross objects.

Usage (both offline)::

    python3 scripts/extract-cli-catalog.py extract --bundle <cli>/dist/cli.mjs \
        --package-json <cli>/package.json --out internal/catalogdata/cli.json \
        --date "$(date -u +%F)" --diff internal/catalogdata/cli.json
    python3 scripts/extract-cli-catalog.py check --bundle <cli>/dist/cli.mjs \
        --snapshot internal/catalogdata/cli.json
"""
from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
from datetime import datetime, timezone
from pathlib import Path

SOURCE_LINE = "Installed command-code CLI static catalog, facts only"
MIN_ENTRIES = 40
SIGNATURE_WINDOW = 256
OUTPUT_FIELDS = ("id", "name", "inputModalities", "reasoningEfforts", "contextWindow", "maxOutputTokens", "reasoning")
CHANGE_FIELDS = OUTPUT_FIELDS[1:]
ID_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9./:_%@+~-]*$")
DATE_PATTERN = re.compile(r"^\d{4}-\d{2}-\d{2}$")
VERSION_PATTERN = re.compile(r"^\d+(\.\d+){1,3}$")
ASSIGN = re.compile(r"[A-Za-z_$][\w$]*=\s*\{")
FIRST_ENTRY = re.compile(r'{\s*[A-Za-z_$][\w$]*\s*:\s*{\s*id\s*:\s*"')
ENTRY_KEY = re.compile(r'[A-Za-z_$][\w$]*\s*:\s*\{')
PROP_KEY = re.compile(r'([A-Za-z_$][\w$]*)\s*:')
GETTER = re.compile(r'get\s+[A-Za-z_$][\w$]*\s*\(')
BOOL_NULL = re.compile(r'true|false|null')
UNESCAPES = {"n": "\n", "t": "\t", "r": "\r", "b": "\b", "f": "\f", "\\": "\\", '"': '"', "'": "'", "0": "\0"}

class ExtractError(Exception):
    """Raised for unreproducible input: no map, ambiguity or unsupported literal."""

def _skip_string(source: str, i: int, quote: str) -> int:
    """Skip a quoted string with escapes; return the index after the close."""
    i += 1
    while i < len(source):
        c = source[i]
        if c == "\\":
            i += 2
        elif c == quote:
            return i + 1
        else:
            i += 1
    raise ExtractError(f"{i}: unterminated string literal")

def _skip_braces(source: str, i: int, open_ch: str, close_ch: str) -> int:
    """Skip to the balanced close starting at the open char; strings are opaque."""
    depth = 1
    i += 1
    while i < len(source):
        c = source[i]
        if c in "\"'":
            i = _skip_string(source, i, c)
            continue
        depth += (c == open_ch) - (c == close_ch)
        if depth == 0:
            return i + 1
        i += 1
    raise ExtractError(f"{i}: unbalanced {open_ch}{close_ch} region")

def _number(token: str, offset: int) -> int:
    """Integer fact numbers: decimal, hex or scientific short forms like 105e4."""
    try:
        return int(token, 10)
    except ValueError:
        pass
    try:
        value = float(token)  # handles 105e4; hex forms fall back to int(,10)
    except ValueError as exc:
        raise ExtractError(f"{offset}: bad number {token!r}") from exc
    if not (value.is_integer() and abs(value) < 2 ** 53):
        raise ExtractError(f"{offset}: non-integer catalog number {token!r}")
    return int(value)

def _parse_scalar(source: str, i: int):
    """Parse one literal value; ``(kind, value, end)``. Unsupported tokens fail.

    Kinds: string/number/boolean/null, flat ``array`` and identifier.
    """
    c = source[i]
    if c in "\"'":
        end = _skip_string(source, i, c)
        return "string", _unescape(source[i + 1:end - 1]), end
    if source.startswith("!0", i):
        return "boolean", True, i + 2
    if source.startswith("!1", i):
        return "boolean", False, i + 2
    if m := BOOL_NULL.match(source, i):
        return ("null", None, m.end()) if m.group(0) == "null" else ("boolean", m.group(0) == "true", m.end())
    if m := re.match(r"-?\d[\w.]*", source[i:]):
        return "number", _number(m.group(0), i), i + m.end()
    if c == "[":
        end = _skip_braces(source, i, "[", "]")
        return "array", _parse_array(source, i + 1, end - 1), end
    if m := re.match(r"[A-Za-z_$][\w$]*", source[i:]):
        return "identifier", m.group(0), i + m.end()
    raise ExtractError(f"{i}: unsupported catalog literal ({source[i:i+12]!r})")

def _parse_array(source: str, start: int, end: int) -> list:
    """Parse a flat comma-separated array of scalar literals."""
    items = []
    i = start
    while i < end:
        c = source[i]
        if c.isspace() or c == ",":
            i += 1
        elif c == "]":
            break
        else:
            kind, value, i = _parse_scalar(source, i)
            if kind == "identifier" and value not in ("null", "true", "false"):
                raise ExtractError(f"{i}: bare identifier {value!r} inside array is not a fact")
            items.append(value)
    return items

def _unescape(raw: str) -> str:
    if "\\" not in raw:
        return raw
    return re.sub(r"\\(.)", lambda m: UNESCAPES.get(m.group(1), m.group(1)), raw)

def _locate_map(source: str) -> tuple[int, int]:
    """Return the span of the single object literal holding the model facts."""
    candidates = []
    for m in ASSIGN.finditer(source):
        open_brace = m.end() - 1
        if not FIRST_ENTRY.match(source[open_brace:open_brace + SIGNATURE_WINDOW]):
            continue
        if "inputModalities:" not in source[m.end():m.end() + SIGNATURE_WINDOW]:
            continue
        try:
            candidates.append((open_brace, _skip_braces(source, open_brace, "{", "}")))
        except ExtractError as exc:
            raise ExtractError(f"{open_brace}: map candidate is not balanced: {exc}") from exc
    if not candidates:
        raise ExtractError("no object-literal model map matches the structural signature")
    if len(candidates) > 1:
        raise ExtractError(f"ambiguous model maps: {candidates}")
    start, end = candidates[0]
    if len(_entries(source, start, end)) < MIN_ENTRIES:
        raise ExtractError("signature matched too few entries; refusing a partial catalog")
    return start, end

def _entries(source: str, start: int, end: int) -> list[tuple[str, int, int]]:
    """Return the top-level ``KEY:{...}`` entries as ``(key, brace, end)``."""
    entries = []
    i = start + 1
    while i < end - 1:
        c = source[i]
        if c.isspace() or c == ",":
            i += 1
        elif c == "\"":
            i = _skip_string(source, i, c)
        elif c == "}":
            break
        elif not (m := ENTRY_KEY.match(source, i, end)):
            raise ExtractError(f"{i}: entry is not a plain KEY:{{...}} block ({source[i:i+16]!r})")
        else:
            brace = m.end() - 1
            entry_end = _skip_braces(source, brace, "{", "}")
            entries.append((m.group(0)[:m.group(0).index(":")].rstrip(), brace, entry_end))
            i = entry_end
    return entries

def _entry_facts(source: str, start: int, end: int) -> list[tuple[str, str, object]]:
    """Parse ``KEY:VALUE`` pairs strictly inside one entry object span."""
    facts = []
    i = start + 1
    while i < end - 1:
        c = source[i]
        if c.isspace() or c == ",":
            i += 1
        elif c == "}":
            break
        elif c == "\"":
            i = _skip_string(source, i, c)
        elif g := GETTER.match(source, i, end):
            # get hidden(){return!isXxx()}: a runtime accessor, not a static
            # fact. Empty parameter list required, body skipped, field dropped.
            close = source.index(")", g.end() - 1, end)
            if source[g.end():close].strip():
                raise ExtractError(f"{i}: getter with parameters is not a static value")
            i = _skip_braces(source, source.index("{", close + 1, end), "{", "}")
        elif not (m := PROP_KEY.match(source, i, end)):
            raise ExtractError(f"{i}: unsupported property ({source[i:i+16]!r})")
        else:
            j = source.index(":", m.end(1), end) + 1
            while source[j] in " \t\r\n":
                j += 1
            kind, value, j = _parse_scalar(source, j)
            facts.append((m.group(1), kind, value))
            i = j
    return facts

def extract_models(source: str) -> list[dict]:
    """Extract the model facts in bundle order with object-boundary strictness."""
    start, end = _locate_map(source)
    models: list[dict] = []
    seen_ids: set[str] = set()
    seen_keys: set[str] = set()
    for key, e_start, e_end in _entries(source, start, end):
        if key in seen_keys:
            raise ExtractError(f"duplicate map key {key!r}")
        seen_keys.add(key)
        facts = {
            field: value
            for field, kind, value in _entry_facts(source, e_start, e_end)
            if field in OUTPUT_FIELDS and kind not in ("identifier", "object")
        }
        # canonical field order regardless of source property order
        model = {field: facts[field] for field in OUTPUT_FIELDS if field in facts}
        _validate_entry(model, key)
        if model["id"] in seen_ids:
            raise ExtractError(f"duplicate model id {model['id']!r}")
        seen_ids.add(model["id"])
        models.append(model)
    if not models:
        raise ExtractError("model map has no readable entries")
    return models

def _validate_entry(model: dict, key: str) -> None:
    """Fail closed unless every recorded fact has the reviewed schema."""
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
        if field in model and not (isinstance(model[field], int) and model[field] > 0):
            raise ExtractError(f"entry {model['id']!r} has invalid {field} {model[field]!r}")
    if "reasoning" in model and not isinstance(model["reasoning"], bool):
        raise ExtractError(f"entry {model['id']!r} has invalid reasoning {model['reasoning']!r}")

def resolve_version(args_version: str, package_json: str | None, source: str) -> str:
    """Resolve the CLI version: explicit flag, else the package.json sibling."""
    version = (args_version or "").strip()
    if not version and package_json:
        try:
            version = str(json.loads(Path(package_json).read_text(encoding="utf-8")).get("version") or "").strip()
        except (OSError, json.JSONDecodeError) as exc:
            raise ExtractError(f"cannot read --package-json: {exc}") from exc
    if not version or not VERSION_PATTERN.match(version):
        raise ExtractError(f"missing or malformed version {version!r}: pass --version or --package-json")
    if version not in source:
        raise ExtractError(f"version {version!r} does not appear in the bundle text")
    return version

def build_snapshot(source: bytes, version: str, date: str) -> dict:
    if not DATE_PATTERN.match(date or ""):
        raise ExtractError(f"date {date!r} must be YYYY-MM-DD")
    return {
        "source": SOURCE_LINE,
        "cli_version": version,
        "bundle_sha256": hashlib.sha256(source).hexdigest(),
        "date": date,
        "models": extract_models(source.decode("utf-8")),
    }

def render_snapshot(snapshot: dict) -> str:
    """Serialize exactly like the committed files."""
    return json.dumps(snapshot, indent=2, ensure_ascii=False) + "\n"

def fact_differences(old_models: list[dict], new_models: list[dict]) -> list[str]:
    """Describe per-model fact differences between two model lists."""
    old = {m["id"]: m for m in old_models}
    new = {m["id"]: m for m in new_models}
    notes = [f"{mid}: missing from snapshot" for mid in new if mid not in old]
    notes += [f"{mid}: not in bundle" for mid in old if mid not in new]
    for mid, model in new.items():
        if mid in old:
            changes = "; ".join(
                f"{field} {old[mid].get(field)!r} -> {model.get(field)!r}"
                for field in CHANGE_FIELDS if old[mid].get(field) != model.get(field)
            )
            if changes:
                notes.append(f"{mid}: {changes}")
    return notes

def diff_report(old: dict, new: dict) -> str:
    """Human readable candidate diff between two snapshots."""
    old_models = old.get("models") or []
    new_models = new.get("models") or []
    old_ids = {m["id"] for m in old_models}
    new_ids = {m["id"] for m in new_models}
    notes = fact_differences(old_models, new_models)
    changed = [note for note in notes if " -> " in note]
    added, removed = sorted(new_ids - old_ids), sorted(old_ids - new_ids)
    lines = [
        f"snapshot candidate diff: {old.get('cli_version')} ({len(old_ids)} models)"
        f" -> {new.get('cli_version')} ({len(new_ids)} models)",
        "added {0}: {1}".format(len(added), ", ".join(added) or "(none)"),
        "removed {0}: {1}".format(len(removed), ", ".join(removed) or "(none)"),
        f"changed {len(changed)}:",
    ]
    lines += [f"  {note}" for note in changed]
    lines.append(f"unchanged {len(new_ids) - len(added) - len(changed)}")
    return "\n".join(lines)

def _command_extract(args: argparse.Namespace) -> int:
    source = Path(args.bundle).read_bytes()
    version = resolve_version(args.version, args.package_json, source.decode("utf-8"))
    date = args.date or datetime.now(timezone.utc).strftime("%Y-%m-%d")
    snapshot = build_snapshot(source, version, date)
    if args.diff:
        try:
            print(diff_report(json.loads(Path(args.diff).read_text(encoding="utf-8")), snapshot))
        except (OSError, json.JSONDecodeError) as exc:
            print(f"diff skipped: {exc}", file=sys.stderr)
    rendered = render_snapshot(snapshot)
    if args.out:
        out_path = Path(args.out)
        if out_path.exists() and out_path.read_text(encoding="utf-8") == rendered:
            print(f"{out_path} already matches the bundle: {version}, {len(snapshot['models'])} models")
            return 0
        out_path.write_text(rendered, encoding="utf-8")
        print(f"wrote {out_path}: cli {version}, sha {snapshot['bundle_sha256']}, {len(snapshot['models'])} models")
    else:
        print(rendered, end="")
    return 0

def _command_check(args: argparse.Namespace) -> int:
    try:
        snapshot = json.loads(Path(args.snapshot).read_text(encoding="utf-8"))
        source = Path(args.bundle).read_bytes()
        version = resolve_version(str(args.version or snapshot.get("cli_version")), None, source.decode("utf-8"))
        fresh = build_snapshot(source, version, snapshot.get("date") or "")
    except (OSError, json.JSONDecodeError) as exc:
        print(f"error: cannot read snapshot {args.snapshot} or bundle {args.bundle}: {exc}", file=sys.stderr)
        return 1
    problems = []
    if snapshot.get("cli_version") != version:
        problems.append(f"cli_version {snapshot.get('cli_version')!r} != bundle {version!r}")
    actual_sha = hashlib.sha256(source).hexdigest()
    if snapshot.get("bundle_sha256") != actual_sha:
        problems.append(f"bundle_sha256 {snapshot.get('bundle_sha256')!r} != bundle {actual_sha!r}")
    problems += fact_differences(snapshot.get("models") or [], fresh["models"])
    if problems:
        print("MISMATCH bundle vs snapshot:", "; ".join(problems[:16]), file=sys.stderr)
        return 1
    print(f"catalog snapshot verified: cli {version}, sha {actual_sha}, {len(fresh['models'])} models")
    return 0

def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    p = sub.add_parser("extract", help="build a cli.json snapshot from a bundle")
    for flag, helptext in (
        ("--bundle", "path to the installed dist/cli.mjs"),
        ("--version", "CLI version to record; verified against the bundle"),
        ("--package-json", "path to command-code package.json to take the version from"),
        ("--out", "write the snapshot here (default: stdout)"),
        ("--date", "snapshot date, YYYY-MM-DD (default: today UTC)"),
        ("--diff", "existing snapshot to diff the fresh facts against"),
    ):
        p.add_argument(flag, help=helptext)
    p = sub.add_parser("check", help="re-verify a snapshot against a bundle")
    p.add_argument("--bundle", required=True, help="path to the installed dist/cli.mjs")
    p.add_argument("--snapshot", required=True, help="path to the snapshot cli.json to verify")
    p.add_argument("--version", help="expected CLI version; default: the snapshot cli_version")
    args = parser.parse_args(argv)
    try:
        return _command_extract(args) if args.command == "extract" else _command_check(args)
    except ExtractError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2

if __name__ == "__main__":
    raise SystemExit(main())
