#!/usr/bin/env python3
"""Checks the built docs site (Zensical output in site/): internal links, heading anchors and published URLs.

Usage (run with -I, standard library only):
  hack/docs-check.py check SITE_DIR PATHS_FILE   internal links, anchors, redirect targets and listed paths; exit 1 on any problem
  hack/docs-check.py list SITE_DIR               print every published page path (the format of hack/docs-published-paths.txt)
  hack/docs-check.py external SITE_DIR           report external links that do not answer; always exits 0 (weekly report)

A published path is the URL path of a page, such as /emergency/. It must resolve to a built page; a redirect page
counts. Adding a page is free; removing a listed path needs its line removed from hack/docs-published-paths.txt in
the same pull request.
"""
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from html.parser import HTMLParser

SITE_URL = "https://shadewright.io"


class Page(HTMLParser):
    """Collects the links, the anchors and the meta refresh target of one HTML page."""

    def __init__(self):
        super().__init__(convert_charrefs=True)
        self.links = []
        self.ids = set()
        self.refresh = None
        self.design_redirects = None
        self._script = None

    def handle_data(self, data):
        if self._script is not None:
            self._script.append(data)

    def handle_endtag(self, tag):
        if tag == "script" and self._script is not None:
            self.design_redirects = "".join(self._script)
            self._script = None

    def handle_starttag(self, tag, attrs):
        a = dict(attrs)
        if tag == "script" and a.get("id") == "design-redirects":
            self._script = []
        if a.get("id"):
            self.ids.add(a["id"])
        if tag == "a":
            if a.get("name"):
                self.ids.add(a["name"])
            if a.get("href") is not None:
                self.links.append(a["href"])
        elif tag == "meta" and (a.get("http-equiv") or "").lower() == "refresh":
            m = re.search(r"url\s*=\s*['\"]?([^'\";]+)", a.get("content") or "", re.I)
            if m:
                self.refresh = m.group(1).strip()


def pages(site):
    """Maps the URL path of every built HTML page to its file."""
    found = {}
    for root, _, files in os.walk(site):
        for name in files:
            if not name.endswith(".html"):
                continue
            rel = os.path.relpath(os.path.join(root, name), site).replace(os.sep, "/")
            url = "/" + (rel[: -len("index.html")] if name == "index.html" else rel)
            found[url] = os.path.join(root, name)
    return found


def parse(path):
    p = Page()
    with open(path, encoding="utf-8") as f:
        p.feed(f.read())
    return p


def resolve(href, base):
    """Returns (url path, fragment) of an internal link from the page at URL path base, or None for an external one."""
    if href == SITE_URL or href.startswith(SITE_URL + "/"):
        href = href[len(SITE_URL):] or "/"
    u = urllib.parse.urlsplit(href)
    if u.scheme or u.netloc:
        return None
    path = urllib.parse.unquote(urllib.parse.urljoin(base, u.path)) if u.path else base
    return path, urllib.parse.unquote(u.fragment)


def exists(site, path):
    """True when the URL path is a page or a file of the site."""
    root = os.path.normpath(site)
    target = os.path.normpath(os.path.join(root, path.lstrip("/")))
    if target != root and not target.startswith(root + os.sep):
        return False
    return os.path.isfile(target) or os.path.isfile(os.path.join(target, "index.html"))


def check(site, listed_file):
    problems = []
    built = pages(site)
    parsed = {url: parse(f) for url, f in built.items()}

    for url, p in sorted(parsed.items()):
        for href in p.links:
            r = resolve(href, url)
            if r is None:
                continue
            path, frag = r
            if not exists(site, path):
                problems.append(f"{url}: broken link {href} (no page {path})")
                continue
            if frag:
                # A link may name index.html explicitly; pages are keyed by their folder path.
                folder = path[: -len("index.html")] if path.endswith("/index.html") else path
                t = parsed.get(folder) or parsed.get(path) or parsed.get(path + "/")
                if t is not None and frag not in t.ids:
                    problems.append(f"{url}: broken anchor {href} (no #{frag} on {path})")
        if p.design_redirects is not None:
            # The old #n-title anchors of the one-page design: each must exist on the page it maps to.
            for anchor, target in json.loads(p.design_redirects).items():
                # Resolved from this page's URL, as the browser resolves the script's relative target.
                r = resolve(target, url)
                t = parsed.get(r[0]) if r else None
                if t is None or anchor not in t.ids:
                    problems.append(f"{url}: old design anchor #{anchor} maps to {target}, which has no such anchor")
        if p.refresh:
            r = resolve(p.refresh, url)
            if r is not None and not exists(site, r[0]):
                problems.append(f"{url}: redirect to {p.refresh} leads nowhere")

    with open(listed_file, encoding="utf-8") as f:
        listed = [line.strip() for line in f if line.strip() and not line.startswith("#")]
    for path in listed:
        if path not in built:
            problems.append(f"published path {path} no longer resolves to a page or redirect; "
                            f"restore it (or add a redirect), or remove it from {listed_file} in this pull request")
    return problems


def external(site):
    seen = {}
    for url, f in sorted(pages(site).items()):
        for href in parse(f).links:
            if href.startswith(("http://", "https://")) and not href.startswith(SITE_URL):
                seen.setdefault(href.split("#")[0], url)
    bad = 0
    for href, where in sorted(seen.items()):
        req = urllib.request.Request(href, headers={"User-Agent": "shadewright-docs-link-check"})
        try:
            with urllib.request.urlopen(req, timeout=20):
                continue
        except urllib.error.HTTPError as e:
            if e.code in (401, 403, 429):  # the site answers but refuses a bot
                continue
            msg = f"HTTP {e.code}"
        except Exception as e:  # noqa: BLE001 - report every kind of failure
            msg = type(e).__name__
        bad += 1
        print(f"WARNING {href} ({msg}), linked from {where}")
    print(f"{len(seen)} external links checked, {bad} not answering")


def main(argv):
    if len(argv) == 4 and argv[1] == "check":
        problems = check(argv[2], argv[3])
        for p in problems:
            print(f"ERROR {p}", file=sys.stderr)
        if problems:
            print(f"{len(problems)} problem(s) in the docs site", file=sys.stderr)
            return 1
        print("docs site links, anchors and published paths ok")
        return 0
    if len(argv) == 3 and argv[1] == "list":
        for url in sorted(u for u in pages(argv[2]) if u != "/404.html"):
            print(url)
        return 0
    if len(argv) == 3 and argv[1] == "external":
        external(argv[2])
        return 0
    print(__doc__, file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv))
