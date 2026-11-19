#!/usr/bin/env bash
# Tests hack/docs-check.py on a small fake site: a good site passes, and a broken link, a broken anchor, a broken
# redirect and a removed published path each fail. The user-page rules run on a fake repo: an issue reference, a date
# and a page over the budget fail, and the allow-list may only shrink.
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

# repo <dir> <pending lines>: a fake repo whose nav names docs/ok.md and docs/bad.md as user pages (both clean)
repo() {
  rm -rf "$1"; mkdir -p "$1/docs" "$1/hack"
  printf '[project]\nnav = [{ "Overview" = [{ "Home" = "ok.md" }, { "Bad" = "bad.md" }] }, { "Operator manual" = ["troubleshooting.md"] }]\n' > "$1/zensical.toml"
  printf 'Plain words.\n' > "$1/docs/ok.md"; printf 'More plain words.\n' > "$1/docs/bad.md"
  printf '%s' "${2:-}" > "$1/hack/docs-user-pages-pending.txt"
  printf 'Fixed in #123 on 2026-01-02.\n' > "$1/docs/troubleshooting.md"  # excluded from the set
}

# expect <name> <pass|fail> <site dir>
expect() {
  if python3 -I "$script" check "$3" "$3/paths.txt" "$tmp/repo" > "$tmp/out" 2>&1; then got=pass; else got=fail; fi
  if [ "$got" = "$2" ]; then echo "ok: $1"; else echo "FAIL: $1: want $2, got $got" >&2; cat "$tmp/out" >&2; fail=1; fi
}
# mentions <name> <text>: the last run reported the text
mentions() { grep -q -- "$2" "$tmp/out" || { echo "FAIL: $1: output lacks '$2'" >&2; fail=1; }; }

s="$tmp/site"
repo "$tmp/repo"
build "$s"; expect "good site passes" pass "$s"

build "$s"; page "$s/a/index.html" '<h2 id="sec">S</h2><a href="../missing/">x</a>'
expect "broken link fails" fail "$s"; mentions "broken link" "broken link ../missing/"

build "$s"; page "$s/index.html" '<a href="a/#nope">x</a>'
expect "broken anchor fails" fail "$s"; mentions "broken anchor" "no #nope"

build "$s"; page "$s/index.html" '<a href="a/index.html#nope">x</a>'
expect "broken anchor via index.html fails" fail "$s"; mentions "index.html anchor" "no #nope"

build "$s"; page "$s/index.html" '<a href="a/index.html#sec">x</a>'
expect "good anchor via index.html passes" pass "$s"

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

build "$s"; page "$s/design/index.html" '<script id="design-redirects" type="application/json">{"x-a":"p/"}</script>'; page "$s/design/p/index.html" '<h2 id="x-a">A</h2>'
expect "old design anchor that exists passes" pass "$s"

build "$s"; page "$s/design/index.html" '<script id="design-redirects" type="application/json">{"x-gone":"p/"}</script>'; page "$s/design/p/index.html" '<h2 id="x-a">A</h2>'
expect "old design anchor without a target fails" fail "$s"; mentions "design anchor" "old design anchor #x-gone"

# user pages
build "$s"; repo "$tmp/repo"
expect "clean user pages pass" pass "$s"

printf 'See #123 for why.\n' > "$tmp/repo/docs/bad.md"
expect "an issue reference fails" fail "$s"; mentions "issue reference" "docs/bad.md:1: issue or pull request reference #123"

printf 'Decided on 2026-11-19.\n' > "$tmp/repo/docs/bad.md"
expect "a date fails" fail "$s"; mentions "date" "docs/bad.md:1: date 2026-11-19"

printf 'Colour #7c24fc and &#35; and https://x/issues/#1 and #section.\n' > "$tmp/repo/docs/bad.md"
expect "colours, entities and anchors are not issue references" pass "$s"

python3 -c "print('word ' * 901)" > "$tmp/repo/docs/bad.md"
expect "a page over the budget fails" fail "$s"; mentions "budget" "docs/bad.md: 901 words, over the budget of 900"

python3 -c "print('word ' * 900)" > "$tmp/repo/docs/bad.md"
expect "a page at the budget passes" pass "$s"

repo "$tmp/repo" $'docs/bad.md\n'; printf 'See #123.\n' > "$tmp/repo/docs/bad.md"
expect "a listed page that fails is allowed" pass "$s"

printf 'Plain.\n' > "$tmp/repo/docs/bad.md"
expect "a listed page that passes fails" fail "$s"; mentions "passes now" "docs/bad.md passes now"

repo "$tmp/repo" $'docs/gone.md\n'
expect "a listed page that does not exist fails" fail "$s"; mentions "missing listed" "docs/gone.md is not a user page that exists"

repo "$tmp/repo" $'docs/troubleshooting.md\n'
expect "a listed page outside the set fails" fail "$s"; mentions "outside listed" "docs/troubleshooting.md is not a user page"

repo "$tmp/repo"; rm "$tmp/repo/hack/docs-user-pages-pending.txt"
expect "an unreadable allow-list fails" fail "$s"

# list prints the pages, without 404.html
build "$s"; page "$s/404.html" '<p>no</p>'
if [ "$(python3 -I "$script" list "$s" | tr '\n' ' ')" = "/ /a/ /old/ " ]; then echo "ok: list"; else echo "FAIL: list" >&2; fail=1; fi

exit "$fail"
