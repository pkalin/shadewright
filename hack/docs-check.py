#!/usr/bin/env python3
"""Checks the built docs site (Zensical output in site/): internal links, heading anchors and published URLs.

Usage (run with -I, standard library only):
  hack/docs-check.py check SITE_DIR PATHS_FILE [REPO_DIR]
                                                 internal links, anchors, redirect targets and listed paths, and the
                                                 user-page rules (below); exit 1 on any problem
  hack/docs-check.py list SITE_DIR               print every published page path (the format of hack/docs-published-paths.txt)
  hack/docs-check.py external SITE_DIR           report external links that do not answer; always exits 0 (weekly report)

A published path is the URL path of a page, such as /emergency/. It must resolve to a built page; a redirect page
counts. Adding a page is free; removing a listed path needs its line removed from hack/docs-published-paths.txt in
the same pull request.

User pages (the "Overview", "How it works", "Get started", "User guide" and "Operator manual" sections of nav in
zensical.toml, except docs/troubleshooting.md) carry no issue references (#123), no dates (YYYY-MM-DD) and at most
USER_PAGE_WORDS words. A page that does not pass yet is listed in hack/docs-user-pages-pending.txt (one path per
line, relative to the repo); the list may only shrink: a listed page that passes, is not a user page or does not
exist fails the check.
"""
import json
import os
import re
import sys
import tomllib
import urllib.error
import urllib.parse
import urllib.request
from html.parser import HTMLParser

SITE_URL = "https://shadewright.io"

USER_PAGE_WORDS = 900  # the longest page of the first set has 742 words
USER_PAGE_SECTIONS = ("Overview", "How it works", "Get started", "User guide", "Operator manual")
USER_PAGE_EXCLUDED = ("troubleshooting.md",)
PENDING_FILE = "hack/docs-user-pages-pending.txt"
ISSUE_REF = re.compile(r"(?<![\w&/])#\d+\b")
DATE = re.compile(r"\b\d{4}-\d{2}-\d{2}\b")


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


def nav_files(items):
    """Yields every page path in a nav fragment of zensical.toml."""
    if isinstance(items, str):
        yield items
    elif isinstance(items, list):
        for i in items:
            yield from nav_files(i)
    elif isinstance(items, dict):
        for v in items.values():
            yield from nav_files(v)


def user_pages(repo):
    """Returns the repo-relative paths (docs/...) of the user pages named by the nav sections."""
    with open(os.path.join(repo, "zensical.toml"), "rb") as f:
        nav = tomllib.load(f)["project"]["nav"]
    found = set()
    for section in nav:
        for name, items in section.items():
            if name in USER_PAGE_SECTIONS:
                found.update("docs/" + p for p in nav_files(items) if p.endswith(".md") and p not in USER_PAGE_EXCLUDED)
    return found


def page_problems(path, text):
    """Returns the user-page rule violations of one page's Markdown."""
    problems = []
    for n, line in enumerate(text.splitlines(), 1):
        for m in ISSUE_REF.finditer(line):
            problems.append(f"{path}:{n}: issue or pull request reference {m.group(0)}")
        for m in DATE.finditer(line):
            problems.append(f"{path}:{n}: date {m.group(0)}")
    words = len(text.split())
    if words > USER_PAGE_WORDS:
        problems.append(f"{path}: {words} words, over the budget of {USER_PAGE_WORDS}")
    return problems


def check_user_pages(repo):
    """Checks the user pages against the writing rules, with the allow-list of pages that do not pass yet."""
    problems = []
    pages_ = user_pages(repo)
    pending_path = os.path.join(repo, PENDING_FILE)
    with open(pending_path, encoding="utf-8") as f:
        pending = [line.strip() for line in f if line.strip() and not line.startswith("#")]
    for path in sorted(pages_):
        try:
            with open(os.path.join(repo, path), encoding="utf-8") as f:
                found = page_problems(path, f.read())
        except OSError as e:
            problems.append(f"{path}: cannot read ({e.strerror}); retry")
            continue
        if found and path not in pending:
            problems += found
            problems.append(f"{path}: fix the lines above (user pages carry no issue numbers, dates or history), "
                            f"or, for an existing page only, list it in {PENDING_FILE}")
    for path in pending:
        if path not in pages_:
            problems.append(f"{PENDING_FILE}: {path} is not a user page that exists; remove the line")
            continue
        try:
            with open(os.path.join(repo, path), encoding="utf-8") as f:
                clean = not page_problems(path, f.read())
        except OSError as e:
            problems.append(f"{path}: cannot read ({e.strerror}); retry")
            continue
        if clean:
            problems.append(f"{PENDING_FILE}: {path} passes now; remove the line")
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
    if len(argv) in (4, 5) and argv[1] == "check":
        repo = argv[4] if len(argv) == 5 else os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
        problems = check(argv[2], argv[3]) + check_user_pages(repo)
        for p in problems:
            print(f"ERROR {p}", file=sys.stderr)
        if problems:
            print(f"{len(problems)} problem(s) in the docs site", file=sys.stderr)
            return 1
        print("docs site links, anchors, published paths and user pages ok")
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
