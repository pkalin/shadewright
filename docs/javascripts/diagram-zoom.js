/*
 * Opens a diagram in a full-viewport overlay with zoom and pan. No dependency, nothing loaded from another host.
 *
 * Two kinds of diagram are enlargeable:
 *   - Mermaid blocks. The theme renders them into a closed shadow root, so the root is recorded by wrapping
 *     attachShadow before the render finishes (this script loads right after the pinned Mermaid file).
 *   - Images of the hand-written SVGs, wrapped in a link to themselves. Without this script the link still
 *     opens the full-size file (the no-JS fallback), so the link is kept and only its click is taken over.
 *
 * Controls: click or Enter on the diagram; in the overlay the buttons, the wheel (zoom at the pointer), pinch,
 * drag to pan, + - 0 and the arrow keys; Esc, the close button or a click on the backdrop closes it and
 * focus returns to the diagram.
 */
(function () {
  "use strict";

  var LABEL = "Enlarge diagram";
  var MIN_SCALE = 0.1;
  var MAX_SCALE = 16;
  var roots = new WeakMap();

  var attach = Element.prototype.attachShadow;
  Element.prototype.attachShadow = function (init) {
    var root = attach.call(this, init);
    if (this.classList && this.classList.contains("mermaid")) roots.set(this, root);
    return root;
  };

  function mermaidSvg(host) {
    var root = roots.get(host);
    return root ? root.querySelector("svg") : null;
  }

  // Marks every enlargeable diagram on the page; safe to run again (instant navigation, late Mermaid).
  function scan() {
    var hosts = document.querySelectorAll("div.mermaid:not([data-sw-zoom])");
    for (var i = 0; i < hosts.length; i++) {
      if (!mermaidSvg(hosts[i])) continue;
      hosts[i].setAttribute("data-sw-zoom", "mermaid");
      hosts[i].setAttribute("tabindex", "0");
      hosts[i].setAttribute("role", "button");
      hosts[i].setAttribute("aria-label", LABEL);
      hosts[i].setAttribute("title", LABEL);
    }
    var links = document.querySelectorAll('.md-typeset a[href$=".svg"]:not([data-sw-zoom])');
    for (var j = 0; j < links.length; j++) {
      if (!links[j].querySelector("img")) continue;
      links[j].setAttribute("data-sw-zoom", "image");
      links[j].setAttribute("aria-label", LABEL);
      links[j].setAttribute("title", LABEL);
    }
  }

  function targetOf(node) {
    return node && node.closest ? node.closest("[data-sw-zoom]") : null;
  }

  document.addEventListener("click", function (e) {
    var el = targetOf(e.target);
    if (!el || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    open(el);
  });
  document.addEventListener("keydown", function (e) {
    var el = targetOf(e.target);
    if (el && el.getAttribute("data-sw-zoom") === "mermaid" && (e.key === "Enter" || e.key === " ")) {
      e.preventDefault();
      open(el);
    }
  });

  var current = null;

  function button(label, text, onClick) {
    var b = document.createElement("button");
    b.type = "button";
    b.className = "sw-zoom__btn";
    b.setAttribute("aria-label", label);
    b.title = label;
    b.textContent = text;
    b.addEventListener("click", onClick);
    return b;
  }

  function open(source) {
    if (current) return;
    var content = document.createElement("div");
    content.className = "sw-zoom__content";
    var w = 0;
    var h = 0;
    var ready = null;

    if (source.getAttribute("data-sw-zoom") === "mermaid") {
      var svg = mermaidSvg(source);
      if (!svg) return;
      var vb = svg.viewBox && svg.viewBox.baseVal;
      var rect = svg.getBoundingClientRect();
      w = vb && vb.width ? vb.width : rect.width;
      h = vb && vb.height ? vb.height : rect.height;
      var clone = svg.cloneNode(true);
      clone.removeAttribute("style");
      clone.setAttribute("width", w);
      clone.setAttribute("height", h);
      content.appendChild(clone);
    } else {
      var src = source.querySelector("img");
      var img = document.createElement("img");
      img.src = source.href;
      img.alt = src.alt;
      img.draggable = false;
      content.appendChild(img);
      ready = new Promise(function (resolve) {
        if (img.complete && img.naturalWidth) return resolve();
        img.addEventListener("load", resolve);
        img.addEventListener("error", resolve);
      }).then(function () {
        w = img.naturalWidth || src.width || 800;
        h = img.naturalHeight || src.height || 600;
        img.width = w;
        img.height = h;
      });
    }

    var overlay = document.createElement("div");
    overlay.className = "sw-zoom";
    overlay.setAttribute("role", "dialog");
    overlay.setAttribute("aria-modal", "true");
    overlay.setAttribute("aria-label", "Diagram, enlarged");
    var stage = document.createElement("div");
    stage.className = "sw-zoom__stage";
    stage.appendChild(content);
    var bar = document.createElement("div");
    bar.className = "sw-zoom__bar";
    var closeBtn = button("Close", "×", close);
    var buttons = [
      button("Zoom out", "−", function () { zoomBy(1 / 1.4); }),
      button("Zoom in", "+", function () { zoomBy(1.4); }),
      button("Fit to window", "Fit", fit),
      closeBtn
    ];
    buttons.forEach(function (b) { bar.appendChild(b); });
    overlay.appendChild(stage);
    overlay.appendChild(bar);

    var s = { x: 0, y: 0, k: 1 };
    var pointers = new Map();
    var moved = false;
    var pinch = 0;

    function apply() {
      content.style.transform = "translate(" + s.x + "px," + s.y + "px) scale(" + s.k + ")";
    }
    function fit() {
      if (!w || !h) return;
      var vw = stage.clientWidth;
      var vh = stage.clientHeight;
      s.k = Math.min((vw * 0.94) / w, (vh * 0.86) / h, 4);
      s.x = (vw - w * s.k) / 2;
      s.y = (vh - h * s.k) / 2;
      apply();
    }
    function zoomAt(factor, cx, cy) {
      var k = Math.min(MAX_SCALE, Math.max(MIN_SCALE, s.k * factor));
      var r = k / s.k;
      s.x = cx - (cx - s.x) * r;
      s.y = cy - (cy - s.y) * r;
      s.k = k;
      apply();
    }
    function zoomBy(factor) {
      zoomAt(factor, stage.clientWidth / 2, stage.clientHeight / 2);
    }
    function local(e) {
      var r = stage.getBoundingClientRect();
      return { x: e.clientX - r.left, y: e.clientY - r.top };
    }

    stage.addEventListener("wheel", function (e) {
      e.preventDefault();
      var p = local(e);
      var delta = e.deltaMode === 1 ? e.deltaY * 16 : e.deltaY;
      zoomAt(Math.exp(-delta * (e.ctrlKey ? 0.01 : 0.0015)), p.x, p.y);
    }, { passive: false });
    stage.addEventListener("pointerdown", function (e) {
      stage.setPointerCapture(e.pointerId);
      pointers.set(e.pointerId, local(e));
      moved = false;
      pinch = 0;
    });
    stage.addEventListener("pointermove", function (e) {
      if (!pointers.has(e.pointerId)) return;
      var prev = pointers.get(e.pointerId);
      var p = local(e);
      pointers.set(e.pointerId, p);
      if (pointers.size === 1) {
        if (Math.abs(p.x - prev.x) + Math.abs(p.y - prev.y) > 0) moved = true;
        s.x += p.x - prev.x;
        s.y += p.y - prev.y;
        apply();
      } else if (pointers.size === 2) {
        moved = true;
        var pts = Array.from(pointers.values());
        var d = Math.hypot(pts[0].x - pts[1].x, pts[0].y - pts[1].y);
        if (pinch) zoomAt(d / pinch, (pts[0].x + pts[1].x) / 2, (pts[0].y + pts[1].y) / 2);
        pinch = d;
      }
    });
    function release(e) {
      pointers.delete(e.pointerId);
      pinch = 0;
    }
    stage.addEventListener("pointerup", function (e) {
      var lastOne = pointers.size === 1;
      release(e);
      // A click on the empty backdrop (not on the diagram, not after a drag) closes the overlay.
      if (lastOne && !moved && !content.contains(e.target)) close();
    });
    stage.addEventListener("pointercancel", release);

    function onKey(e) {
      if (e.key === "Escape") { e.preventDefault(); close(); return; }
      if (e.key === "Tab") {
        var i = buttons.indexOf(document.activeElement);
        var next = e.shiftKey ? (i <= 0 ? buttons.length - 1 : i - 1) : (i + 1) % buttons.length;
        e.preventDefault();
        buttons[next].focus();
        return;
      }
      var step = 60;
      if (e.key === "+" || e.key === "=") zoomBy(1.4);
      else if (e.key === "-" || e.key === "_") zoomBy(1 / 1.4);
      else if (e.key === "0") fit();
      else if (e.key === "ArrowLeft") { s.x += step; apply(); }
      else if (e.key === "ArrowRight") { s.x -= step; apply(); }
      else if (e.key === "ArrowUp") { s.y += step; apply(); }
      else if (e.key === "ArrowDown") { s.y -= step; apply(); }
      else return;
      e.preventDefault();
    }

    function close() {
      document.removeEventListener("keydown", onKey, true);
      window.removeEventListener("resize", fit);
      overlay.remove();
      document.documentElement.classList.remove("sw-zoom-open");
      current = null;
      if (source.isConnected) source.focus();
    }

    current = overlay;
    document.documentElement.classList.add("sw-zoom-open");
    document.body.appendChild(overlay);
    document.addEventListener("keydown", onKey, true);
    window.addEventListener("resize", fit);
    closeBtn.focus();
    Promise.resolve(ready).then(function () {
      if (current === overlay) {
        fit();
        overlay.classList.add("sw-zoom--ready");
      }
    });
  }

  if (window.document$ && window.document$.subscribe) window.document$.subscribe(scan);
  new MutationObserver(scan).observe(document.documentElement, { childList: true, subtree: true });
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", scan);
  else scan();
})();
