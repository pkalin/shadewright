#!/usr/bin/env bash
# Tests hack/docs-check.py on a small fake site: a good site passes, and a broken link, a broken anchor, a broken
# redirect and a removed published path each fail.
# Run: hack/docs-check_test.sh (or make test-scripts)
set -euo pipefail

script="$(cd "$(dirname "$0")" && pwd)/docs-check.py"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0

page() { # file body
  mkdir -p "$(dirname "$1")"
  printf '<html><head></head><body>%s</body></html>' "$2" > "$1"
}

build() { # dir: a good site with two pages, a redirect, a file and a list of published paths
  rm -rf "$1"
  page "$1/index.html" '<a href="a/">A</a> <a href="a/#sec">S</a> <a href="https://example.com/x">ext</a> <a href="https://shadewright.io/a/#sec">abs</a> <a href="#top">t</a><h1 id="top">Home</h1>'
  page "$1/a/index.html" '<h2 id="sec">Section</h2><a href="../">home</a> <a href="../old/">old</a> <a href="../file.txt">f</a>'
  mkdir -p "$1/old"
  printf '<html><head><meta http-equiv="refresh" content="0; url=../a/"></head></html>' > "$1/old/index.html"
  echo x > "$1/file.txt"
  printf '# listed\n/\n/a/\n/old/\n' > "$1/paths.txt"
}

# expect <name> <pass|fail> <site dir>
expect() {
  if python3 -I "$script" check "$3" "$3/paths.txt" > "$tmp/out" 2>&1; then got=pass; else got=fail; fi
  if [ "$got" = "$2" ]; then echo "ok: $1"; else echo "FAIL: $1: want $2, got $got" >&2; cat "$tmp/out" >&2; fail=1; fi
}
# mentions <name> <text>: the last run reported the text
mentions() { grep -q -- "$2" "$tmp/out" || { echo "FAIL: $1: output lacks '$2'" >&2; fail=1; }; }

s="$tmp/site"
build "$s"; expect "good site passes" pass "$s"

build "$s"; page "$s/a/index.html" '<h2 id="sec">S</h2><a href="../missing/">x</a>'
expect "broken link fails" fail "$s"; mentions "broken link" "broken link ../missing/"

build "$s"; page "$s/index.html" '<a href="a/#nope">x</a>'
expect "broken anchor fails" fail "$s"; mentions "broken anchor" "no #nope"

build "$s"; page "$s/index.html" '<a href="#nope">x</a>'
expect "broken anchor on the same page fails" fail "$s"

build "$s"; printf '<meta http-equiv="refresh" content="0; url=../gone/">' > "$s/old/index.html"
expect "redirect to nowhere fails" fail "$s"; mentions "redirect" "leads nowhere"

build "$s"; rm -r "$s/old"
expect "removed listed path fails" fail "$s"; mentions "removed path" "published path /old/"

build "$s"; rm -r "$s/old"; page "$s/a/index.html" '<h2 id="sec">Section</h2>'; printf '/\n/a/\n' > "$s/paths.txt"
expect "removed path with the list edited passes" pass "$s"

build "$s"; page "$s/new/index.html" '<p>new</p>'
expect "an added page needs no list change" pass "$s"

# list prints the pages, without 404.html
build "$s"; page "$s/404.html" '<p>no</p>'
if [ "$(python3 -I "$script" list "$s" | tr '\n' ' ')" = "/ /a/ /old/ " ]; then echo "ok: list"; else echo "FAIL: list" >&2; fail=1; fi

exit "$fail"
