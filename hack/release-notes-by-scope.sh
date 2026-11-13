#!/usr/bin/env bash
# Regroups the body of a release-please release (read from the file argument or stdin) by Conventional
# Commit scope and writes the result to stdout.
#
# release-please groups by type (Features, Bug fixes, ...) and shows the scope as a bold prefix. This
# script keeps the version heading and a "BREAKING CHANGES" section at the top, then writes one section
# per scope (fixed order, readable titles, "Other" for entries without a known scope). Inside a section
# the entries are ordered by type (Features, Bug fixes, Performance, Reverts, then any other type).
# Every entry is kept as release-please rendered it (links to the PR and the commit), minus the scope
# prefix (an unknown scope keeps its prefix and lands in "Other"). Indented continuation lines stay with their entry.
#
# Usage: hack/release-notes-by-scope.sh [notes.md] > grouped.md
# Regroup a published release by hand:
#   gh release view vX.Y.Z --json body -q .body | hack/release-notes-by-scope.sh > notes.md
#   gh release edit vX.Y.Z --notes-file notes.md
set -euo pipefail

if [ "$#" -gt 1 ]; then
  echo "usage: $0 [notes.md]" >&2
  exit 2
fi

awk '
BEGIN {
  nsc = split("api analyzer verifier policy gitops platforms integrations chart docs ci deps other", scopes, " ")
  split("API|Analyzer|Verifier|Policy|GitOps|Platforms|Integrations|Chart|Docs|CI|Dependencies|Other", titles, "|")
  for (i = 1; i <= nsc; i++) known[scopes[i]] = i
  ntypes = 0
  heading = ""; breaking = ""; cur = 0; ntail = 0
}

function flush() {
  if (cur == 0) return
  if (btype == "BREAKING") { breaking = breaking item "\n"; cur = 0; return }
  s = item_scope
  if (!(s in known)) s = "other"
  si = known[s]
  if (!((btype) in typeidx)) { typeidx[btype] = ++ntypes; typename[ntypes] = btype }
  ti = typeidx[btype]
  body[si, ti] = body[si, ti] item "\n"
  cur = 0
}

/^## / && heading == "" { heading = $0; next }

/^### / {
  flush()
  t = substr($0, 5)
  if (t ~ /BREAKING/) btype = "BREAKING"; else btype = t
  next
}

/^\* / {
  flush()
  line = $0
  item_scope = "other"
  if (match(line, /^\* \*\*[^*:]+:\*\* /)) {
    sc = substr(line, 5, RLENGTH - 8)
    prefix = substr(line, 1, 2)
    rest = substr(line, RLENGTH + 1)
    if (btype != "BREAKING" && (sc in known)) { item_scope = sc; line = prefix rest }
  }
  item = line
  cur = 1
  next
}

/^[[:space:]]+[^[:space:]]/ && cur == 1 { item = item "\n" $0; next }

/^[[:space:]]*$/ { next }

# Anything else (free text) is kept at the end rather than dropped.
{ flush(); tail[++ntail] = $0 }

END {
  flush()
  if (heading != "") print heading "\n"
  if (breaking != "") printf "### Breaking changes\n\n%s\n", breaking
  # Type order: the well-known ones first, then the rest in order of appearance.
  n = 0
  split("Features|Bug fixes|Performance|Reverts", pref, "|")
  for (i = 1; i <= 4; i++) if (pref[i] in typeidx) order[++n] = typeidx[pref[i]]
  for (i = 1; i <= ntypes; i++) {
    if (typename[i] == "BREAKING") continue
    seen = 0
    for (j = 1; j <= n; j++) if (order[j] == i) seen = 1
    if (!seen) order[++n] = i
  }
  for (si = 1; si <= nsc; si++) {
    has = 0
    for (k = 1; k <= n; k++) if ((si, order[k]) in body) has = 1
    if (!has) continue
    print "### " titles[si] "\n"
    for (k = 1; k <= n; k++) {
      if (!((si, order[k]) in body)) continue
      print "#### " typename[order[k]] "\n"
      printf "%s\n", body[si, order[k]]
    }
  }
  for (i = 1; i <= ntail; i++) print tail[i]
}
' "${1:--}" | sed -e :a -e '/^\n*$/{$d;N;ba' -e '}'
