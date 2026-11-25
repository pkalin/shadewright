#!/usr/bin/env bash
# Fails when Make, the Dockerfile builder stage, the release plugin script or any GitHub Actions
# workflow that invokes Go directly outside Make does not enforce GOFLAGS=-mod=readonly and
# GOWORK=off (ADR-0048), or when a CI workflow runs `go mod tidy` without `-diff`.
# Usage: hack/check-go-build-settings.sh [repository root]    (run by make lint)
set -euo pipefail

root="${1:-$(cd "$(dirname "$0")/.." && pwd)}"
root="$(cd "${root}" && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

fail=0

# 1. Make: probe a recipe with hostile caller env so a weak assignment (?=) fails too.
cat > "${tmp}/probe.mk" <<'EOF'
.PHONY: __check-go-build-settings
__check-go-build-settings:
	@printf 'GOFLAGS=%s\nGOWORK=%s\n' "$$GOFLAGS" "$$GOWORK"
EOF

if make_out="$(env GOFLAGS=-mod=mod GOWORK=/nonexistent/go.work make -s -C "${root}" -f "${root}/Makefile" -f "${tmp}/probe.mk" __check-go-build-settings 2>&1)"; then
  make_goflags="$(printf '%s\n' "${make_out}" | sed -n 's/^GOFLAGS=//p' | tail -n1)"
  make_gowork="$(printf '%s\n' "${make_out}" | sed -n 's/^GOWORK=//p' | tail -n1)"
  if ! printf '%s\n' "${make_goflags}" | tr ' \t' '\n' | grep -Fxq -- '-mod=readonly'; then
    echo "error: Makefile recipe observed GOFLAGS='${make_goflags}', want -mod=readonly" >&2
    fail=1
  fi
  if [ "${make_gowork}" != "off" ]; then
    echo "error: Makefile recipe observed GOWORK='${make_gowork}', want off" >&2
    fail=1
  fi
else
  echo "error: failed to probe Makefile: ${make_out}" >&2
  fail=1
fi

# 2. Makefile $(shell ... go ...), hack/package-plugin.sh, Dockerfile builder stage, and CI workflows.
if ! python3 -I - "${root}" <<'PY'
import os
import re
import shlex
import sys
from pathlib import Path

root = Path(sys.argv[1])
errors = []

def has_readonly(val: str) -> bool:
    return "-mod=readonly" in val.split()

# 2a. Every $(shell ... go ...) in Makefile must pass GOFLAGS and GOWORK explicitly
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

makefile = root / "Makefile"
if not makefile.is_file():
    errors.append("Makefile not found")
else:
    shell_go_count = 0
    for lineno, line in enumerate(makefile.read_text(encoding="utf-8").splitlines(), 1):
        stripped = line.lstrip()
        if stripped.startswith("#"):
            continue
        for cmd in extract_shell_calls(line):
            if re.search(r"(^|[;&|(\s])go\s+", cmd):
                shell_go_count += 1
                if "GOFLAGS=" not in cmd or "GOWORK=" not in cmd:
                    errors.append(
                        f"Makefile:{lineno}: $(shell ...) invokes go without GOFLAGS and GOWORK: {cmd.strip()}"
                    )
    if shell_go_count == 0:
        errors.append("Makefile: expected at least one $(shell ... go ...) call to verify")

# 2b. Release plugin script must delegate Go builds to `make build-plugin` without weakening env.
pkg_plugin = root / "hack" / "package-plugin.sh"
if not pkg_plugin.is_file():
    errors.append("hack/package-plugin.sh not found")
else:
    found_make_plugin = False
    for lineno, line in enumerate(pkg_plugin.read_text(encoding="utf-8").splitlines(), 1):
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if re.search(r"\bmake\b[^\n]*\bbuild-plugin\b", stripped):
            found_make_plugin = True
        if re.search(r"\b(GOFLAGS|GOWORK)=", stripped):
            errors.append(f"hack/package-plugin.sh:{lineno}: overrides GOFLAGS or GOWORK")
        if re.search(r"(^|[;&|(`]\s*)go\s+", stripped):
            errors.append(f"hack/package-plugin.sh:{lineno}: invokes go directly instead of make build-plugin")
    if not found_make_plugin:
        errors.append("hack/package-plugin.sh: does not invoke `make build-plugin`")

# 2c. Dockerfile builder stage must set GOFLAGS=-mod=readonly and GOWORK=off before the first RUN
# and keep them set throughout the builder stage.
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
    env_before_run = {}
    env_final = {}

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
                env_final[k] = v
                if not seen_run:
                    env_before_run[k] = v
        elif re.match(r"(?i)^RUN\b", inst):
            seen_run = True

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
        if not has_readonly(env_final.get("GOFLAGS", "")):
            errors.append(
                f"Dockerfile: builder stage overwrote GOFLAGS (final {env_final.get('GOFLAGS', '')!r})"
            )
        if env_final.get("GOWORK", "") != "off":
            errors.append(
                f"Dockerfile: builder stage overwrote GOWORK (final {env_final.get('GOWORK', '')!r})"
            )

# 2d. GitHub Actions workflows that invoke Go directly outside Make (or CodeQL Go autobuild).
GO_SUBCMDS = (
    "build|test|mod|run|list|install|vet|fmt|generate|env|get|tool|work|clean|doc|fix"
)
DIRECT_GO_RE = re.compile(
    rf"(?:^|[;&|(`])\s*(?:[A-Za-z_][A-Za-z0-9_]*=(?:\"[^\"]*\"|'[^']*'|\S*)\s+)*go\s+({GO_SUBCMDS})\b([^\n;&|)`]*)"
)

def clean_yaml_scalar(val: str) -> str:
    val = val.strip()
    if not val:
        return ""
    if val[0] in ("'", '"') and len(val) >= 2 and val[-1] == val[0]:
        return val[1:-1]
    # Strip trailing unquoted comment
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
                eff_env = {}
                eff_env.update(wf_env)
                eff_env.update(job["env"])
                eff_env.update(step["env"])

                is_autobuild = step["uses"].startswith("github/codeql-action/autobuild")
                go_matches = []
                if step["run"]:
                    for rline in step["run"].splitlines():
                        rstripped = rline.strip()
                        if not rstripped or rstripped.startswith("#"):
                            continue
                        for m in DIRECT_GO_RE.finditer(rstripped):
                            go_matches.append((m.group(1), m.group(2)))

                if not is_autobuild and not go_matches:
                    continue

                checked_direct_ci.append(f"{rel}:{job['name']}")
                step_label = step["name"] or f"line {step['line']}"
                if not has_readonly(eff_env.get("GOFLAGS", "")):
                    errors.append(
                        f"{rel} (job {job['name']}, step {step_label!r}): direct Go step missing GOFLAGS=-mod=readonly"
                    )
                if eff_env.get("GOWORK", "") != "off":
                    errors.append(
                        f"{rel} (job {job['name']}, step {step_label!r}): direct Go step missing GOWORK=off"
                    )
                for subcmd, args in go_matches:
                    if subcmd == "mod":
                        arg_tokens = args.split()
                        if arg_tokens and arg_tokens[0] == "tidy" and "-diff" not in arg_tokens[1:]:
                            errors.append(
                                f"{rel} (job {job['name']}, step {step_label!r}): `go mod tidy` must pass -diff to avoid mutating go.mod/go.sum"
                            )

    if not checked_direct_ci:
        errors.append(".github/workflows: found no direct Go CI steps to check")

if errors:
    for err in errors:
        print(f"error: {err}", file=sys.stderr)
    sys.exit(1)

print(
    f"Go build settings OK (GOFLAGS=-mod=readonly, GOWORK=off in Makefile, Dockerfile, "
    f"hack/package-plugin.sh and {len(checked_direct_ci)} direct CI steps)"
)
PY
then
  fail=1
fi

exit "${fail}"
