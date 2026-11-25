#!/usr/bin/env bash
# Fails when Make, the Dockerfile builder stage, direct Go helper scripts or any GitHub Actions
# workflow that invokes Go directly outside Make does not enforce GOFLAGS=-mod=readonly and
# GOWORK=off (ADR-0048), or when a CI workflow runs `go mod tidy` without `-diff`.
# Usage: hack/check-go-build-settings.sh [repository root]    (run by make lint)
set -euo pipefail

root="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
root="$(cd "${root}" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

cat > "${tmp}/probe.mk" <<'EOF'
.PHONY: __check-go-build-settings
__check-go-build-settings:
	@printf 'GOFLAGS=%s\nGOWORK=%s\n' "$$GOFLAGS" "$$GOWORK"
EOF

python3 -I - "${root}" "${tmp}/probe.mk" <<'PY'
import os
import re
import shlex
import subprocess
import sys
from pathlib import Path

root = Path(sys.argv[1])
probe_mk = Path(sys.argv[2])
errors = []

GO_SUBCMDS = {
    "build",
    "test",
    "mod",
    "run",
    "list",
    "install",
    "vet",
    "fmt",
    "generate",
    "env",
    "get",
    "tool",
    "work",
    "clean",
    "doc",
    "fix",
}

SHELL_KEYWORDS = {"if", "then", "elif", "else", "do", "while", "until", "!", "{", "}"}
ASSIGN_RE = re.compile(r"^([A-Za-z_][A-Za-z0-9_]*)=(.*)$")
IDENT_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")


def mod_flag_tokens(val: str) -> list[str]:
    out = []
    for tok in val.split():
        if tok in ("-mod", "--mod") or tok.startswith("-mod=") or tok.startswith("--mod="):
            out.append(tok)
    return out


def has_readonly(val: str) -> bool:
    mod = None
    for tok in val.split():
        if tok in ("-mod", "--mod"):
            return False
        if tok.startswith("-mod="):
            mod = tok[len("-mod=") :]
        elif tok.startswith("--mod="):
            mod = tok[len("--mod=") :]
    return mod == "readonly"


def effective_mod_is_readonly(goflags: str, cli_args: list[str]) -> bool:
    if not has_readonly(goflags):
        return False
    mod = "readonly"
    i = 0
    while i < len(cli_args):
        tok = cli_args[i]
        if tok == "--":
            break
        if tok in ("-mod", "--mod"):
            if i + 1 < len(cli_args):
                mod = cli_args[i + 1]
                i += 2
                continue
            return False
        if tok.startswith("-mod="):
            mod = tok[len("-mod=") :]
        elif tok.startswith("--mod="):
            mod = tok[len("--mod=") :]
        i += 1
    return mod == "readonly"


def extract_balanced_dollar_paren(text: str, start: int):
    """start points at '(' in '$('. Returns (inner_text, next_index)."""
    depth = 1
    i = start + 1
    n = len(text)
    in_sq = False
    in_dq = False
    while i < n and depth > 0:
        ch = text[i]
        if in_sq:
            if ch == "'":
                in_sq = False
            i += 1
        elif in_dq:
            if ch == "\\" and i + 1 < n:
                i += 2
            elif ch == '"':
                in_dq = False
                i += 1
            elif ch == "$" and i + 1 < n and text[i + 1] == "(":
                _, i = extract_balanced_dollar_paren(text, i + 1)
            else:
                i += 1
        else:
            if ch == "\\" and i + 1 < n:
                i += 2
            elif ch == "'":
                in_sq = True
                i += 1
            elif ch == '"':
                in_dq = True
                i += 1
            elif ch == "(":
                depth += 1
                i += 1
            elif ch == ")":
                depth -= 1
                i += 1
            else:
                i += 1
    if depth == 0:
        return text[start + 1 : i - 1], i
    return text[start + 1 :], n


def split_shell_statements(text: str):
    """Split shell text into (statement_text, [command_substitution_texts])."""
    stmts = []
    buf = []
    subs = []
    i = 0
    n = len(text)
    in_sq = False
    in_dq = False

    def flush():
        stmt = "".join(buf).strip()
        if stmt or subs:
            stmts.append((stmt, list(subs)))
        buf.clear()
        subs.clear()

    while i < n:
        ch = text[i]
        if in_sq:
            if ch == "'":
                in_sq = False
            buf.append(ch)
            i += 1
        elif in_dq:
            if ch == "\\" and i + 1 < n:
                if text[i + 1] == "\n":
                    i += 2
                    continue
                buf.append(text[i : i + 2])
                i += 2
                continue
            if ch == '"':
                in_dq = False
                buf.append(ch)
                i += 1
            elif ch == "$" and i + 1 < n and text[i + 1] == "(":
                if i + 2 < n and text[i + 2] == "(":
                    # Arithmetic expansion $((...))
                    buf.append(ch)
                    i += 1
                else:
                    inner, i = extract_balanced_dollar_paren(text, i + 1)
                    subs.append(inner)
                    buf.append("__SUBSHELL__")
            elif ch == "`":
                j = i + 1
                inner_buf = []
                while j < n:
                    if text[j] == "\\" and j + 1 < n:
                        inner_buf.append(text[j + 1])
                        j += 2
                    elif text[j] == "`":
                        j += 1
                        break
                    else:
                        inner_buf.append(text[j])
                        j += 1
                subs.append("".join(inner_buf))
                buf.append("__SUBSHELL__")
                i = j
            else:
                buf.append(ch)
                i += 1
        else:
            if ch == "\\" and i + 1 < n:
                if text[i + 1] == "\n":
                    i += 2
                    continue
                buf.append(text[i : i + 2])
                i += 2
                continue
            if ch == "#" and (not buf or buf[-1].isspace()):
                while i < n and text[i] != "\n":
                    i += 1
                continue
            if ch == "'":
                in_sq = True
                buf.append(ch)
                i += 1
            elif ch == '"':
                in_dq = True
                buf.append(ch)
                i += 1
            elif ch == "$" and i + 1 < n and text[i + 1] == "(":
                if i + 2 < n and text[i + 2] == "(":
                    buf.append(ch)
                    i += 1
                else:
                    inner, i = extract_balanced_dollar_paren(text, i + 1)
                    subs.append(inner)
                    buf.append("__SUBSHELL__")
            elif ch == "`":
                j = i + 1
                inner_buf = []
                while j < n:
                    if text[j] == "\\" and j + 1 < n:
                        inner_buf.append(text[j + 1])
                        j += 2
                    elif text[j] == "`":
                        j += 1
                        break
                    else:
                        inner_buf.append(text[j])
                        j += 1
                subs.append("".join(inner_buf))
                buf.append("__SUBSHELL__")
                i = j
            elif ch in "\n;|&()":
                flush()
                if ch in "&|" and i + 1 < n and text[i + 1] == ch:
                    i += 2
                else:
                    i += 1
            else:
                buf.append(ch)
                i += 1
    flush()
    return stmts


def tokenize_stmt(stmt: str) -> list[str]:
    try:
        return shlex.split(stmt, posix=True)
    except ValueError:
        return stmt.split()


def analyze_shell(script_text: str, initial_env: dict[str, str]):
    """Return list of (subcmd, cli_args, effective_env, raw_stmt) for every go invocation."""
    exported = dict(initial_env)
    shell_vars = dict(initial_env)
    found = []

    for stmt, subs in split_shell_statements(script_text):
        for sub in subs:
            found.extend(analyze_shell(sub, exported))

        if not stmt:
            continue
        tokens = tokenize_stmt(stmt)
        while tokens and tokens[0] in SHELL_KEYWORDS:
            tokens = tokens[1:]
        if not tokens:
            continue

        if tokens[0] == "export":
            unexport = False
            i = 1
            while i < len(tokens) and tokens[i].startswith("-"):
                if tokens[i] == "--":
                    i += 1
                    break
                if "n" in tokens[i][1:]:
                    unexport = True
                i += 1
            for tok in tokens[i:]:
                m = ASSIGN_RE.match(tok)
                if m:
                    k, v = m.group(1), m.group(2)
                    shell_vars[k] = v
                    if unexport:
                        exported.pop(k, None)
                    else:
                        exported[k] = v
                elif IDENT_RE.match(tok):
                    if unexport:
                        exported.pop(tok, None)
                    else:
                        exported[tok] = shell_vars.get(tok, "")
            continue

        if tokens[0] == "unset":
            i = 1
            while i < len(tokens) and tokens[i].startswith("-"):
                if tokens[i] == "--":
                    i += 1
                    break
                i += 1
            for tok in tokens[i:]:
                if IDENT_RE.match(tok):
                    exported.pop(tok, None)
                    shell_vars.pop(tok, None)
            continue

        prefix = {}
        idx = 0
        while idx < len(tokens):
            m = ASSIGN_RE.match(tokens[idx])
            if not m:
                break
            prefix[m.group(1)] = m.group(2)
            idx += 1

        if idx == len(tokens):
            # Standalone variable assignments: in bash, assigning to an already-exported
            # variable updates its exported value.
            for k, v in prefix.items():
                shell_vars[k] = v
                if k in exported:
                    exported[k] = v
            continue

        cmd_env = dict(exported)
        cmd_env.update(prefix)
        cmd_tokens = tokens[idx:]

        # Unwrap leading `env` command(s).
        while cmd_tokens and os.path.basename(cmd_tokens[0]) == "env":
            j = 1
            while j < len(cmd_tokens):
                tok = cmd_tokens[j]
                if tok == "--":
                    j += 1
                    break
                if tok in ("-i", "-"):
                    cmd_env.clear()
                    j += 1
                elif tok == "-u" and j + 1 < len(cmd_tokens):
                    cmd_env.pop(cmd_tokens[j + 1], None)
                    j += 2
                elif tok.startswith("-u") and len(tok) > 2:
                    cmd_env.pop(tok[2:], None)
                    j += 1
                elif tok.startswith("--unset="):
                    cmd_env.pop(tok[len("--unset=") :], None)
                    j += 1
                elif tok.startswith("-"):
                    j += 1
                else:
                    m = ASSIGN_RE.match(tok)
                    if m:
                        cmd_env[m.group(1)] = m.group(2)
                        j += 1
                    else:
                        break
            cmd_tokens = cmd_tokens[j:]

        if (
            len(cmd_tokens) >= 2
            and os.path.basename(cmd_tokens[0]) == "go"
            and cmd_tokens[1] in GO_SUBCMDS
        ):
            found.append((cmd_tokens[1], cmd_tokens[2:], cmd_env, stmt))

    return found


def check_go_invocation(location: str, subcmd: str, cli_args: list[str], cmd_env: dict[str, str], check_tidy: bool = False):
    goflags = cmd_env.get("GOFLAGS", "")
    gowork = cmd_env.get("GOWORK", "")
    if not has_readonly(goflags):
        errors.append(
            f"{location}: `go {subcmd}` effective GOFLAGS={goflags!r} does not enforce -mod=readonly"
        )
    elif not effective_mod_is_readonly(goflags, cli_args):
        errors.append(
            f"{location}: `go {subcmd}` CLI flags override -mod=readonly (args={cli_args!r})"
        )
    if gowork != "off":
        errors.append(
            f"{location}: `go {subcmd}` effective GOWORK={gowork!r}, want 'off'"
        )
    if check_tidy and subcmd == "mod" and cli_args and cli_args[0] == "tidy" and "-diff" not in cli_args[1:]:
        errors.append(
            f"{location}: `go mod tidy` must pass -diff to avoid mutating go.mod/go.sum"
        )


# 1. Make: probe hostile env, unrelated caller GOFLAGS, and hostile Make CLI overrides.
makefile = root / "Makefile"
make_ok = False
if not makefile.is_file():
    errors.append("Makefile not found")
else:
    base_env = os.environ.copy()
    base_env.pop("GOFLAGS", None)
    base_env.pop("GOWORK", None)

    make_probes = [
        (
            "hostile env with -p=1",
            {**base_env, "GOFLAGS": "-p=1 -mod=mod", "GOWORK": "/nonexistent/go.work"},
            [],
            True,
        ),
        (
            "env GOFLAGS=-p=1",
            {**base_env, "GOFLAGS": "-p=1"},
            [],
            True,
        ),
        (
            "CLI override GOFLAGS/GOWORK",
            base_env,
            ["GOFLAGS=-p=1 -mod=mod --mod=vendor", "GOWORK=auto"],
            True,
        ),
    ]

    make_probe_passed = True
    for label, env_map, cli_vars, expect_p1 in make_probes:
        cmd = [
            "make",
            "-s",
            "-C",
            str(root),
            "-f",
            str(makefile),
            "-f",
            str(probe_mk),
            *cli_vars,
            "__check-go-build-settings",
        ]
        res = subprocess.run(cmd, env=env_map, capture_output=True, text=True)
        if res.returncode != 0:
            errors.append(f"Makefile probe ({label}) failed: {(res.stdout + res.stderr).strip()}")
            make_probe_passed = False
            continue
        out_goflags = ""
        out_gowork = ""
        for line in res.stdout.splitlines():
            if line.startswith("GOFLAGS="):
                out_goflags = line[len("GOFLAGS=") :]
            elif line.startswith("GOWORK="):
                out_gowork = line[len("GOWORK=") :]
        if not has_readonly(out_goflags) or mod_flag_tokens(out_goflags) != ["-mod=readonly"]:
            errors.append(
                f"Makefile probe ({label}) observed GOFLAGS={out_goflags!r}, want -mod=readonly with prior -mod flags replaced"
            )
            make_probe_passed = False
        if expect_p1 and "-p=1" not in out_goflags.split():
            errors.append(
                f"Makefile probe ({label}) dropped unrelated GOFLAGS token '-p=1' (got GOFLAGS={out_goflags!r})"
            )
            make_probe_passed = False
        if out_gowork != "off":
            errors.append(
                f"Makefile probe ({label}) observed GOWORK={out_gowork!r}, want 'off'"
            )
            make_probe_passed = False
    make_ok = make_probe_passed


# 2a. Every $(shell ... go ...) in Makefile must evaluate to effective GOFLAGS=-mod=readonly and GOWORK=off
# because GNU Make 3.81 does not export Make variables into $(shell ...) evaluations.
def extract_shell_calls(line: str):
    calls = []
    pos = 0
    while True:
        idx = line.find("$(shell", pos)
        if idx == -1:
            break
        start = idx + len("$(shell")
        if start >= len(line) or not line[start].isspace():
            pos = start
            continue
        depth = 1
        j = start
        while j < len(line) and depth > 0:
            if line[j] == "(" and j > 0 and line[j - 1] == "$":
                depth += 1
            elif line[j] == ")":
                depth -= 1
            j += 1
        if depth == 0:
            calls.append(line[start : j - 1])
            pos = j
        else:
            break
    return calls


def expand_make_vars_for_shell(cmd: str, goflags_val: str, gowork_val: str) -> str:
    # Unescape Make's $$ -> $ after substituting known Make variables.
    out = cmd.replace("$(GOFLAGS)", goflags_val).replace("${GOFLAGS}", goflags_val)
    out = out.replace("$(GOWORK)", gowork_val).replace("${GOWORK}", gowork_val)
    # Replace remaining single-level Make variable references like $(1) without touching $$(...).
    out = re.sub(r"(?<!\$)\$\([^()$]+\)", "__MAKEVAR__", out)
    out = re.sub(r"(?<!\$)\$\{[^{}$]+\}", "__MAKEVAR__", out)
    out = out.replace("$$", "$")
    return out


if makefile.is_file():
    shell_go_count = 0
    mk_goflags = "-mod=readonly" if make_ok else ""
    mk_gowork = "off" if make_ok else ""
    for lineno, line in enumerate(makefile.read_text(encoding="utf-8").splitlines(), 1):
        stripped = line.lstrip()
        if stripped.startswith("#"):
            continue
        for cmd in extract_shell_calls(line):
            expanded = expand_make_vars_for_shell(cmd, mk_goflags, mk_gowork)
            invocations = analyze_shell(expanded, initial_env={})
            for subcmd, cli_args, cmd_env, _ in invocations:
                shell_go_count += 1
                check_go_invocation(f"Makefile:{lineno} $(shell ...)", subcmd, cli_args, cmd_env)
    if shell_go_count == 0:
        errors.append("Makefile: expected at least one $(shell ... go ...) call to verify")


# 2b. Release plugin script must delegate Go builds to `make build-plugin` without weakening env.
pkg_plugin = root / "hack" / "package-plugin.sh"
if not pkg_plugin.is_file():
    errors.append("hack/package-plugin.sh not found")
else:
    found_make_plugin = False
    pkg_text = pkg_plugin.read_text(encoding="utf-8")
    for lineno, line in enumerate(pkg_text.splitlines(), 1):
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if re.search(r"\bmake\b[^\n]*\bbuild-plugin\b", stripped):
            found_make_plugin = True
        if re.search(r"\b(GOFLAGS|GOWORK)=", stripped):
            errors.append(f"hack/package-plugin.sh:{lineno}: overrides GOFLAGS or GOWORK")
    for subcmd, _, _, raw in analyze_shell(pkg_text, initial_env={}):
        errors.append(
            f"hack/package-plugin.sh: invokes `go {subcmd}` directly instead of make build-plugin ({raw})"
        )
    if not found_make_plugin:
        errors.append("hack/package-plugin.sh: does not invoke `make build-plugin`")


# 2c. Direct Go helper scripts under hack/ must export GOFLAGS=-mod=readonly and GOWORK=off.
checked_helper_scripts = 0
for rel_script in ("hack/check-plugin-deps.sh", "hack/load/measure.sh"):
    spath = root / rel_script
    if not spath.is_file():
        errors.append(f"{rel_script} not found")
        continue
    invocations = analyze_shell(spath.read_text(encoding="utf-8"), initial_env={})
    if not invocations:
        errors.append(f"{rel_script}: expected at least one direct go invocation to verify")
        continue
    checked_helper_scripts += 1
    for subcmd, cli_args, cmd_env, _ in invocations:
        check_go_invocation(rel_script, subcmd, cli_args, cmd_env)


# 2d. Dockerfile builder stage must set GOFLAGS=-mod=readonly and GOWORK=off before the first RUN,
# keep them set at every RUN and at the end of the stage, and not weaken them inside any RUN.
dockerfile = root / "Dockerfile"
if not dockerfile.is_file():
    errors.append("Dockerfile not found")
else:
    raw_lines = dockerfile.read_text(encoding="utf-8").splitlines()
    logical_lines = []
    buf = ""
    start_line = 1
    for lineno, raw in enumerate(raw_lines, 1):
        stripped = raw.strip()
        if not buf:
            if not stripped or stripped.startswith("#"):
                continue
            start_line = lineno
        else:
            if stripped.startswith("#"):
                continue
        if raw.rstrip().endswith("\\"):
            buf += raw.rstrip()[:-1] + " "
        else:
            buf += raw
            logical_lines.append((start_line, buf.strip()))
            buf = ""
    if buf:
        logical_lines.append((start_line, buf.strip()))

    in_builder = False
    found_builder = False
    seen_run = False
    env_current = {}
    env_before_run = {}

    for lineno, inst in logical_lines:
        if re.match(r"(?i)^FROM\s+", inst):
            if re.search(r"(?i)\s+AS\s+builder\s*$", inst):
                in_builder = True
                found_builder = True
            else:
                in_builder = False
            continue
        if not in_builder:
            continue
        m_env = re.match(r"(?i)^ENV\s+(.*)$", inst)
        if m_env:
            rest = m_env.group(1).strip()
            try:
                tokens = shlex.split(rest)
            except ValueError as exc:
                errors.append(f"Dockerfile:{lineno}: invalid ENV syntax: {exc}")
                continue
            if tokens and "=" not in tokens[0] and len(tokens) >= 2:
                pairs = [(tokens[0], " ".join(tokens[1:]))]
            else:
                pairs = []
                for tok in tokens:
                    if "=" in tok:
                        k, v = tok.split("=", 1)
                        pairs.append((k, v))
            for k, v in pairs:
                env_current[k] = v
                if not seen_run:
                    env_before_run[k] = v
        else:
            m_run = re.match(r"(?i)^RUN\s+(.*)$", inst)
            if m_run:
                seen_run = True
                if not has_readonly(env_current.get("GOFLAGS", "")):
                    errors.append(
                        f"Dockerfile:{lineno}: builder RUN has effective GOFLAGS={env_current.get('GOFLAGS', '')!r}, want -mod=readonly"
                    )
                if env_current.get("GOWORK", "") != "off":
                    errors.append(
                        f"Dockerfile:{lineno}: builder RUN has effective GOWORK={env_current.get('GOWORK', '')!r}, want 'off'"
                    )
                run_body = m_run.group(1).strip()
                # Strip BuildKit RUN --flag=val options before the shell command.
                while run_body.startswith("--"):
                    parts = run_body.split(None, 1)
                    run_body = parts[1] if len(parts) > 1 else ""
                for subcmd, cli_args, cmd_env, _ in analyze_shell(run_body, initial_env=env_current):
                    check_go_invocation(f"Dockerfile:{lineno}", subcmd, cli_args, cmd_env)

    if not found_builder:
        errors.append("Dockerfile: builder stage not found")
    else:
        if not has_readonly(env_before_run.get("GOFLAGS", "")):
            errors.append(
                f"Dockerfile: builder stage must set GOFLAGS=-mod=readonly before first RUN (got {env_before_run.get('GOFLAGS', '')!r})"
            )
        if env_before_run.get("GOWORK", "") != "off":
            errors.append(
                f"Dockerfile: builder stage must set GOWORK=off before first RUN (got {env_before_run.get('GOWORK', '')!r})"
            )
        if not has_readonly(env_current.get("GOFLAGS", "")):
            errors.append(
                f"Dockerfile: builder stage overwrote GOFLAGS (final {env_current.get('GOFLAGS', '')!r})"
            )
        if env_current.get("GOWORK", "") != "off":
            errors.append(
                f"Dockerfile: builder stage overwrote GOWORK (final {env_current.get('GOWORK', '')!r})"
            )


# 2e. GitHub Actions workflows that invoke Go directly outside Make (or CodeQL Go autobuild).
def clean_yaml_scalar(val: str) -> str:
    val = val.strip()
    if not val:
        return ""
    if val[0] in ("'", '"') and len(val) >= 2 and val[-1] == val[0]:
        return val[1:-1]
    val = re.sub(r"\s+#.*$", "", val)
    return val.strip()


def parse_workflow(path: Path):
    lines = path.read_text(encoding="utf-8").splitlines()
    wf_env = {}
    jobs = []
    i = 0
    n = len(lines)

    def read_env_block(start_idx: int, parent_indent: int):
        env_map = {}
        j = start_idx
        while j < n:
            line = lines[j]
            stripped = line.strip()
            if not stripped or stripped.startswith("#"):
                j += 1
                continue
            ind = len(line) - len(line.lstrip(" "))
            if ind <= parent_indent:
                break
            m = re.match(r"^\s*([A-Za-z_][A-Za-z0-9_]*)\s*:\s*(.*)$", line)
            if m:
                env_map[m.group(1)] = clean_yaml_scalar(m.group(2))
            j += 1
        return env_map, j

    while i < n:
        line = lines[i]
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            i += 1
            continue
        ind = len(line) - len(line.lstrip(" "))
        if ind == 0 and stripped == "env:":
            wf_env, i = read_env_block(i + 1, 0)
            continue
        if ind == 0 and stripped == "jobs:":
            i += 1
            while i < n:
                jline = lines[i]
                jstripped = jline.strip()
                if not jstripped or jstripped.startswith("#"):
                    i += 1
                    continue
                jind = len(jline) - len(jline.lstrip(" "))
                if jind == 0:
                    break
                if jind == 2 and jstripped.endswith(":"):
                    job_name = jstripped[:-1]
                    job_env = {}
                    steps = []
                    i += 1
                    while i < n:
                        kline = lines[i]
                        kstripped = kline.strip()
                        if not kstripped or kstripped.startswith("#"):
                            i += 1
                            continue
                        kind = len(kline) - len(kline.lstrip(" "))
                        if kind <= 2:
                            break
                        if kind == 4 and kstripped == "env:":
                            job_env, i = read_env_block(i + 1, 4)
                            continue
                        if kind == 4 and kstripped == "steps:":
                            i += 1
                            cur_step = None
                            while i < n:
                                sline = lines[i]
                                sstripped = sline.strip()
                                if not sstripped or sstripped.startswith("#"):
                                    i += 1
                                    continue
                                sind = len(sline) - len(sline.lstrip(" "))
                                if sind <= 4:
                                    break
                                if sind == 6 and sstripped.startswith("- "):
                                    if cur_step is not None:
                                        steps.append(cur_step)
                                    cur_step = {
                                        "name": "",
                                        "uses": "",
                                        "run": "",
                                        "env": {},
                                        "line": i + 1,
                                    }
                                    field = sstripped[2:].strip()
                                    field_indent = 8
                                else:
                                    if cur_step is None:
                                        i += 1
                                        continue
                                    field = sstripped
                                    field_indent = sind

                                if field == "env:":
                                    senv, i = read_env_block(i + 1, field_indent)
                                    cur_step["env"].update(senv)
                                    continue
                                m_name = re.match(r"^name\s*:\s*(.*)$", field)
                                if m_name:
                                    cur_step["name"] = clean_yaml_scalar(m_name.group(1))
                                    i += 1
                                    continue
                                m_uses = re.match(r"^uses\s*:\s*(.*)$", field)
                                if m_uses:
                                    cur_step["uses"] = clean_yaml_scalar(m_uses.group(1))
                                    i += 1
                                    continue
                                m_run = re.match(r"^run\s*:\s*(.*)$", field)
                                if m_run:
                                    val = m_run.group(1).strip()
                                    if val in ("|", "|-", "|+", ">", ">-", ">+"):
                                        block = []
                                        i += 1
                                        while i < n:
                                            bline = lines[i]
                                            if not bline.strip():
                                                block.append("")
                                                i += 1
                                                continue
                                            bind = len(bline) - len(bline.lstrip(" "))
                                            if bind <= field_indent:
                                                break
                                            block.append(bline[bind:])
                                            i += 1
                                        cur_step["run"] = "\n".join(block)
                                        continue
                                    else:
                                        cur_step["run"] = clean_yaml_scalar(val)
                                        i += 1
                                        continue
                                i += 1
                            if cur_step is not None:
                                steps.append(cur_step)
                            continue
                        i += 1
                    jobs.append({"name": job_name, "env": job_env, "steps": steps})
                    continue
                i += 1
            continue
        i += 1
    return wf_env, jobs


wf_dir = root / ".github" / "workflows"
checked_direct_ci = []
if not wf_dir.is_dir():
    errors.append(".github/workflows directory not found")
else:
    wf_files = sorted(list(wf_dir.glob("*.yml")) + list(wf_dir.glob("*.yaml")))
    for wf in wf_files:
        wf_env, jobs = parse_workflow(wf)
        rel = wf.relative_to(root).as_posix()
        for job in jobs:
            for step in job["steps"]:
                step_env = {}
                step_env.update(wf_env)
                step_env.update(job["env"])
                step_env.update(step["env"])

                step_label = step["name"] or f"line {step['line']}"
                location = f"{rel} (job {job['name']}, step {step_label!r})"

                is_autobuild = step["uses"].startswith("github/codeql-action/autobuild")
                go_invocations = analyze_shell(step["run"], initial_env=step_env) if step["run"] else []

                if not is_autobuild and not go_invocations:
                    continue

                checked_direct_ci.append(f"{rel}:{job['name']}")
                if is_autobuild:
                    if not has_readonly(step_env.get("GOFLAGS", "")):
                        errors.append(
                            f"{location}: autobuild step missing GOFLAGS=-mod=readonly (got {step_env.get('GOFLAGS', '')!r})"
                        )
                    if step_env.get("GOWORK", "") != "off":
                        errors.append(
                            f"{location}: autobuild step missing GOWORK=off (got {step_env.get('GOWORK', '')!r})"
                        )

                for subcmd, cli_args, cmd_env, _ in go_invocations:
                    check_go_invocation(location, subcmd, cli_args, cmd_env, check_tidy=True)

    if not checked_direct_ci:
        errors.append(".github/workflows: found no direct Go CI steps to check")

if errors:
    for err in errors:
        print(f"error: {err}", file=sys.stderr)
    sys.exit(1)

print(
    f"Go build settings OK (GOFLAGS=-mod=readonly, GOWORK=off in Makefile, Dockerfile, "
    f"hack/package-plugin.sh, {checked_helper_scripts} hack scripts and {len(checked_direct_ci)} direct CI steps)"
)
PY
