#!/usr/bin/env bash
# Tests hack/check-go-build-settings.sh and verifies the two runtime guarantees of ADR-0048:
# (1) a parent go.work with a local replacement cannot influence a Make build, and
# (2) a build that requires a go.mod edit fails without modifying go.mod, while `go mod tidy -diff`
#     detects dependency drift without mutating go.mod.
# Run: hack/check-go-build-settings_test.sh (or make test-scripts)
set -euo pipefail

repo="$(cd "$(dirname "$0")/.." && pwd)"
script="${repo}/hack/check-go-build-settings.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
fail=0

pass_or_fail() { # pass|refuse, name, cmd...
  local want="$1" name="$2"
  shift 2
  local got
  if "$@" > /dev/null 2>&1; then
    got=pass
  else
    got=refuse
  fi
  if [ "${got}" = "${want}" ]; then
    echo "ok: ${name}"
  else
    echo "FAIL: ${name}: ${got}, want ${want}" >&2
    fail=1
  fi
}

# 1. Real repository passes the build-setting regression check.
pass_or_fail pass "real repository passes check-go-build-settings.sh" "${script}" "${repo}"

# 2. Runtime test: parent go.work with a local replacement cannot influence a Make build.
ws="${tmp}/gowork"
mkdir -p "${ws}/evil-dep" "${ws}/real-dep" "${ws}/app"
cat > "${ws}/evil-dep/go.mod" <<'EOF'
module example.com/dep

go 1.26.0
EOF
cat > "${ws}/evil-dep/dep.go" <<'EOF'
package dep

const Source = "gowork"
EOF
cat > "${ws}/real-dep/go.mod" <<'EOF'
module example.com/dep

go 1.26.0
EOF
cat > "${ws}/real-dep/dep.go" <<'EOF'
package dep

const Source = "gomod"
EOF
cat > "${ws}/app/go.mod" <<'EOF'
module example.com/app

go 1.26.0

require example.com/dep v0.0.0

replace example.com/dep => ../real-dep
EOF
cat > "${ws}/app/main.go" <<'EOF'
package main

import (
	"fmt"
	"example.com/dep"
)

func main() {
	fmt.Println(dep.Source)
}
EOF
cat > "${ws}/go.work" <<'EOF'
go 1.26.0

use ./app

replace example.com/dep => ./evil-dep
EOF

# Control: without GOWORK=off, the parent go.work hijacks the build.
control_bin="${tmp}/control-gowork"
(cd "${ws}/app" && env -u GOWORK -u GOFLAGS go build -o "${control_bin}" .)
control_out="$("${control_bin}")"
if [ "${control_out}" = "gowork" ]; then
  echo "ok: control build without GOWORK=off picks up parent go.work"
else
  echo "FAIL: control build produced '${control_out}', want 'gowork'" >&2
  fail=1
fi

# Under the repository Makefile (even with GOWORK explicitly set in the caller env),
# the parent go.work is ignored.
cat > "${tmp}/build-probe.mk" <<'EOF'
.PHONY: __build-probe __gomodver-probe __env-probe
__build-probe:
	go build -o "$(OUT)" .
__gomodver-probe:
	@printf '%s\n' '$(call gomodver,example.com/dep)'
__env-probe:
	@printf 'GOFLAGS=%s\nGOWORK=%s\n' "$$GOFLAGS" "$$GOWORK"
EOF
make_bin="${tmp}/make-gowork"
env GOWORK="${ws}/go.work" GOFLAGS=-mod=mod make -s -C "${ws}/app" \
  -f "${repo}/Makefile" -f "${tmp}/build-probe.mk" OUT="${make_bin}" __build-probe
make_out="$("${make_bin}")"
if [ "${make_out}" = "gomod" ]; then
  echo "ok: Makefile build ignores parent go.work replacement"
else
  echo "FAIL: Makefile build produced '${make_out}', want 'gomod'" >&2
  fail=1
fi

# Command-line Make overrides (make GOFLAGS=... GOWORK=...) must not bypass the settings,
# and unrelated caller GOFLAGS (like -p=1) must be preserved in both env and CLI forms.
make_cli_bin="${tmp}/make-gowork-cli"
make -s -C "${ws}/app" -f "${repo}/Makefile" -f "${tmp}/build-probe.mk" \
  GOFLAGS="-p=1 -mod=mod" GOWORK="${ws}/go.work" OUT="${make_cli_bin}" __build-probe
make_cli_out="$("${make_cli_bin}")"
if [ "${make_cli_out}" = "gomod" ]; then
  echo "ok: Makefile ignores command-line GOWORK and GOFLAGS overrides"
else
  echo "FAIL: Makefile CLI override produced '${make_cli_out}', want 'gomod'" >&2
  fail=1
fi

env_probe="$(env GOFLAGS=-p=1 make -s -C "${repo}" -f "${repo}/Makefile" -f "${tmp}/build-probe.mk" __env-probe)"
env_probe_goflags="$(printf '%s\n' "${env_probe}" | sed -n 's/^GOFLAGS=//p' | tail -n1)"
env_probe_gowork="$(printf '%s\n' "${env_probe}" | sed -n 's/^GOWORK=//p' | tail -n1)"
if [ "${env_probe_goflags}" = "-p=1 -mod=readonly" ] && [ "${env_probe_gowork}" = "off" ]; then
  echo "ok: Makefile preserves unrelated env GOFLAGS=-p=1"
else
  echo "FAIL: Makefile env GOFLAGS=-p=1 got GOFLAGS='${env_probe_goflags}' GOWORK='${env_probe_gowork}'" >&2
  fail=1
fi

cli_probe="$(make -s -C "${repo}" -f "${repo}/Makefile" -f "${tmp}/build-probe.mk" GOFLAGS="-p=1 -mod=mod" GOWORK=auto __env-probe)"
cli_probe_goflags="$(printf '%s\n' "${cli_probe}" | sed -n 's/^GOFLAGS=//p' | tail -n1)"
cli_probe_gowork="$(printf '%s\n' "${cli_probe}" | sed -n 's/^GOWORK=//p' | tail -n1)"
if [ "${cli_probe_goflags}" = "-p=1 -mod=readonly" ] && [ "${cli_probe_gowork}" = "off" ]; then
  echo "ok: Makefile overrides CLI GOFLAGS=-mod=mod GOWORK=auto while preserving -p=1"
else
  echo "FAIL: Makefile CLI override got GOFLAGS='${cli_probe_goflags}' GOWORK='${cli_probe_gowork}'" >&2
  fail=1
fi

# 3. Runtime test: a build requiring a go.mod edit fails under Makefile and leaves go.mod untouched.
ro="${tmp}/readonly"
mkdir -p "${ro}/dep" "${ro}/app"
cat > "${ro}/dep/go.mod" <<'EOF'
module example.com/dep

go 1.26.0
EOF
cat > "${ro}/dep/dep.go" <<'EOF'
package dep

const Value = "ok"
EOF
cat > "${ro}/app/go.mod" <<'EOF'
module example.com/app

go 1.26.0

replace example.com/dep => ../dep
EOF
cat > "${ro}/app/main.go" <<'EOF'
package main

import (
	"fmt"
	"example.com/dep"
)

func main() {
	fmt.Println(dep.Value)
}
EOF
cp "${ro}/app/go.mod" "${tmp}/ro.go.mod.before"

pass_or_fail refuse "Makefile build refuses missing require in go.mod" \
  env GOFLAGS=-mod=mod GOWORK=off make -s -C "${ro}/app" \
  -f "${repo}/Makefile" -f "${tmp}/build-probe.mk" OUT="${tmp}/ro-bin" __build-probe

pass_or_fail refuse "Makefile CLI override refuses missing require in go.mod" \
  make -s -C "${ro}/app" -f "${repo}/Makefile" -f "${tmp}/build-probe.mk" \
  GOFLAGS=-mod=mod GOWORK=auto OUT="${tmp}/ro-cli-bin" __build-probe

if cmp -s "${tmp}/ro.go.mod.before" "${ro}/app/go.mod"; then
  echo "ok: refused build leaves go.mod untouched"
else
  echo "FAIL: refused build modified go.mod" >&2
  fail=1
fi

# Control: with -mod=mod outside Make, the same build succeeds and edits go.mod.
if (cd "${ro}/app" && env GOFLAGS=-mod=mod GOWORK=off go build -o "${tmp}/ro-control" . > /dev/null 2>&1) &&
   ! cmp -s "${tmp}/ro.go.mod.before" "${ro}/app/go.mod"; then
  echo "ok: control build with -mod=mod edits go.mod"
else
  echo "FAIL: control build with -mod=mod did not edit go.mod as expected" >&2
  fail=1
fi

# 4. Runtime test: go mod tidy -diff detects drift under GOFLAGS=-mod=readonly GOWORK=off without mutating go.mod.
tidy="${tmp}/tidy"
mkdir -p "${tidy}/dep" "${tidy}/app"
cp "${ro}/dep/go.mod" "${ro}/dep/dep.go" "${tidy}/dep/"
cat > "${tidy}/app/go.mod" <<'EOF'
module example.com/app

go 1.26.0

require example.com/dep v0.0.0

replace example.com/dep => ../dep
EOF
cat > "${tidy}/app/main.go" <<'EOF'
package main

func main() {}
EOF
cp "${tidy}/app/go.mod" "${tmp}/tidy.go.mod.before"
pass_or_fail refuse "go mod tidy -diff fails on unused requirement" \
  env GOFLAGS=-mod=readonly GOWORK=off go -C "${tidy}/app" mod tidy -diff
if cmp -s "${tmp}/tidy.go.mod.before" "${tidy}/app/go.mod"; then
  echo "ok: go mod tidy -diff leaves go.mod untouched"
else
  echo "FAIL: go mod tidy -diff modified go.mod" >&2
  fail=1
fi

# 4b. Runtime test: direct invocation of hack/check-plugin-deps.sh ignores hostile caller GOWORK/GOFLAGS.
pass_or_fail pass "check-plugin-deps.sh ignores hostile caller GOWORK and GOFLAGS" \
  env GOWORK=/nonexistent/go.work GOFLAGS=-mod=mod PLUGIN_DEPS_FILE=/dev/null PLUGIN_SIZE=1 \
  "${repo}/hack/check-plugin-deps.sh"

# 5. Negative fixture tests for hack/check-go-build-settings.sh.
make_fixture() {
  local dir="$1"
  rm -rf "${dir}"
  mkdir -p "${dir}/hack/load" "${dir}/.github/workflows"
  cp "${repo}/Makefile" "${dir}/Makefile"
  cp "${repo}/Dockerfile" "${dir}/Dockerfile"
  cp "${repo}/hack/package-plugin.sh" "${dir}/hack/package-plugin.sh"
  cp "${repo}/hack/check-plugin-deps.sh" "${dir}/hack/check-plugin-deps.sh"
  cp "${repo}/hack/load/measure.sh" "${dir}/hack/load/measure.sh"
  cp "${repo}/.github/workflows/"*.yml "${dir}/.github/workflows/"
}

fx="${tmp}/fixture"

make_fixture "${fx}"
pass_or_fail pass "fixture copy passes" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/^override GOFLAGS :=.*$/# removed/' "${fx}/Makefile"
pass_or_fail refuse "Makefile without GOFLAGS fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/^override GOFLAGS :=.*$/export GOFLAGS ?= -mod=readonly/' "${fx}/Makefile"
pass_or_fail refuse "Makefile with weak GOFLAGS ?= fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/^override GOFLAGS :=.*$/GOFLAGS := -mod=readonly/' "${fx}/Makefile"
pass_or_fail refuse "Makefile without override on GOFLAGS fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/^override GOFLAGS :=.*$/override GOFLAGS := -mod=readonly/' "${fx}/Makefile"
pass_or_fail refuse "Makefile dropping unrelated caller GOFLAGS fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/^override GOWORK := off$/# removed/' "${fx}/Makefile"
pass_or_fail refuse "Makefile without GOWORK=off fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/^override GOWORK := off$/GOWORK := off/' "${fx}/Makefile"
pass_or_fail refuse "Makefile without override on GOWORK fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/GOFLAGS="[^"]*" GOWORK="[^"]*" go list/go list/' "${fx}/Makefile"
pass_or_fail refuse "Makefile shell go call without GOFLAGS/GOWORK fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/GOWORK="\$(GOWORK)" go list/GOWORK=auto go list/' "${fx}/Makefile"
pass_or_fail refuse "Makefile shell go list with GOWORK=auto fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/GOFLAGS="\$(GOFLAGS)" GOWORK="\$(GOWORK)" go list/GOFLAGS="-mod=readonly -mod=mod" GOWORK="$(GOWORK)" go list/' "${fx}/Makefile"
pass_or_fail refuse "Makefile shell go list with GOFLAGS=-mod=readonly -mod=mod fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/GOFLAGS=-mod=readonly/GOFLAGS=/' "${fx}/Dockerfile"
pass_or_fail refuse "Dockerfile without GOFLAGS=-mod=readonly fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/GOFLAGS=-mod=readonly/GOFLAGS="-mod=readonly -mod=mod"/' "${fx}/Dockerfile"
pass_or_fail refuse "Dockerfile with GOFLAGS=-mod=readonly -mod=mod fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/GOWORK=off/GOWORK=auto/' "${fx}/Dockerfile"
pass_or_fail refuse "Dockerfile without GOWORK=off fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/^RUN go mod download$/RUN GOFLAGS=-mod=mod go mod download/' "${fx}/Dockerfile"
pass_or_fail refuse "Dockerfile RUN with inline GOFLAGS=-mod=mod fails" "${script}" "${fx}"

make_fixture "${fx}"
python3 -I - "${fx}/Dockerfile" <<'PY'
from pathlib import Path
import sys
p = Path(sys.argv[1])
lines = p.read_text(encoding="utf-8").splitlines()
out = [l for l in lines if "GOFLAGS=-mod=readonly" not in l and "GOWORK=off" not in l]
idx = next(i for i, l in enumerate(out) if l.strip().startswith("RUN go mod download"))
out.insert(idx + 1, "ENV GOFLAGS=-mod=readonly GOWORK=off")
p.write_text("\n".join(out) + "\n", encoding="utf-8")
PY
pass_or_fail refuse "Dockerfile setting ENV after first RUN fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/GOFLAGS: -mod=readonly/# removed/' "${fx}/.github/workflows/evaluation.yml"
pass_or_fail refuse "direct Go workflow missing GOFLAGS fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/run: go test -c -o \/dev\/null \.\/test\/eval/run: GOFLAGS=-mod=mod GOWORK=auto go test -c -o \/dev\/null .\/test\/eval/' "${fx}/.github/workflows/evaluation.yml"
pass_or_fail refuse "direct Go workflow inline override fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/run: go test -c -o \/dev\/null \.\/test\/eval/run: go test -mod=mod -c -o \/dev\/null .\/test\/eval/' "${fx}/.github/workflows/evaluation.yml"
pass_or_fail refuse "direct Go workflow CLI -mod=mod override fails" "${script}" "${fx}"

make_fixture "${fx}"
python3 -I - "${fx}/.github/workflows/copilot-setup-steps.yml" <<'PY'
from pathlib import Path
import sys
p = Path(sys.argv[1])
p.write_text(
    p.read_text(encoding="utf-8").replace(
        "          go mod download",
        "          export GOWORK=auto\n          go mod download",
        1,
    ),
    encoding="utf-8",
)
PY
pass_or_fail refuse "direct Go workflow export GOWORK=auto override fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/GOWORK: off/# removed/' "${fx}/.github/workflows/codeql.yml"
pass_or_fail refuse "CodeQL autobuild workflow missing GOWORK fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/go mod tidy -diff/go mod tidy/' "${fx}/.github/workflows/test.yml"
pass_or_fail refuse "CI workflow running go mod tidy without -diff fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/make build-plugin/go build .\/cmd\/kubectl-shadewright/' "${fx}/hack/package-plugin.sh"
pass_or_fail refuse "package-plugin.sh bypassing make build-plugin fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/^export GOWORK=off$/# removed/' "${fx}/hack/check-plugin-deps.sh"
pass_or_fail refuse "check-plugin-deps.sh missing GOWORK=off fails" "${script}" "${fx}"

make_fixture "${fx}"
sed -i.bak 's/^export GOFLAGS=-mod=readonly$/# removed/' "${fx}/hack/load/measure.sh"
pass_or_fail refuse "load/measure.sh missing GOFLAGS=-mod=readonly fails" "${script}" "${fx}"

exit "${fail}"
