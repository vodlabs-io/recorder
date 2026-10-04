// The scripts the stream sniffer (stream-sniff.ts) runs inside the page.

// ALL_FRAMES_PRELOAD must match the marker in patches/electrobun@1.18.1.patch:
// a custom preload opening with it is injected into every frame, not just the
// top one.
const ALL_FRAMES_PRELOAD = "/* electrobun:all-frames */";

// SNIFFER is the preload injected into every frame of the sniffing view.
export const SNIFFER = `${ALL_FRAMES_PRELOAD}
(function () {
  if (window.__vlSniff) return; window.__vlSniff = true;
  var RX = /\\.m3u8|\\.flv|mpegurl/i;
  var HLS = /\\.m3u8|mpegurl/i;
  var seen = {};
  var sent = 0;
  function post(hit) {
    if (sent >= 300) return; sent++;
    try {
      var b = window.__electrobunEventBridge ||
        (window.webkit && window.webkit.messageHandlers && window.webkit.messageHandlers.eventBridge);
      if (!b) return;
      hit.frame = location.href;
      b.postMessage(JSON.stringify({ id: "webviewEvent", type: "message", payload: {
        id: window.__electrobunWebviewId, eventName: "host-message",
        detail: JSON.stringify({ vlSniff: 1, hit: hit }) } }));
    } catch (e) {}
  }
  function report(kind, u, extra) {
    try {
      if (!u || typeof u !== "string" || u.indexOf("blob:") === 0 || u.indexOf("data:") === 0) return;
      var abs = new URL(u, location.href).href;
      if (!RX.test(abs)) return;
      var key = kind + " " + abs;
      var repeats = kind === "fetch" || kind === "xhr" || kind === "resource";
      if (seen[key] && (!repeats || seen[key] >= 3)) return;
      seen[key] = (seen[key] || 0) + 1;
      var hit = { kind: kind, url: abs };
      if (extra) for (var k in extra) hit[k] = extra[k];
      post(hit);
    } catch (e) {}
  }
  // inspect reads a playlist the page fetched. An address that looks like a
  // playlist but answers with something else (an ad schedule, an error page)
  // is reported invalid.
  function inspect(u, text) {
    if (typeof text !== "string" || !text) return;
    if (text.indexOf("#EXTM3U") === -1) { report("response", u, { invalid: true }); return; }
    report("response", u, { master: text.indexOf("#EXT-X-STREAM-INF") !== -1,
      ended: text.indexOf("#EXT-X-ENDLIST") !== -1 || /PLAYLIST-TYPE:VOD/.test(text) });
  }
  var of = window.fetch;
  if (of) window.fetch = function (input) {
    var u;
    try { u = typeof input === "string" ? input : (input && input.url) || String(input); } catch (e) {}
    report("fetch", u);
    var p = of.apply(this, arguments);
    try {
      if (u && HLS.test(u)) p.then(function (r) {
        try { r.clone().text().then(function (t) { inspect(r.url || u, t.slice(0, 65536)); }, function () {}); } catch (e) {}
      }, function () {});
    } catch (e) {}
    return p;
  };
  var oo = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function (m, u) {
    try {
      var url = String(u);
      report("xhr", url);
      if (HLS.test(url)) {
        var x = this;
        x.addEventListener("load", function () {
          try {
            if (x.responseType === "" || x.responseType === "text")
              inspect(x.responseURL || url, String(x.responseText).slice(0, 65536));
          } catch (e) {}
        });
      }
    } catch (e) {}
    return oo.apply(this, arguments);
  };
  function hookSrc(proto) {
    var d = Object.getOwnPropertyDescriptor(proto, "src");
    if (d && d.set) Object.defineProperty(proto, "src", { get: d.get, configurable: true,
      set: function (v) { report("media", String(v)); return d.set.call(this, v); } });
  }
  hookSrc(HTMLMediaElement.prototype);
  hookSrc(HTMLSourceElement.prototype);
  var sa = Element.prototype.setAttribute;
  Element.prototype.setAttribute = function (n, v) {
    try {
      if (String(n).toLowerCase() === "src" &&
          (this instanceof HTMLMediaElement || this instanceof HTMLSourceElement)) report("media", String(v));
    } catch (e) {}
    return sa.apply(this, arguments);
  };
  try {
    new PerformanceObserver(function (l) {
      l.getEntries().forEach(function (e) { report("resource", e.name); });
    }).observe({ type: "resource", buffered: true });
  } catch (e) {}
  function scan(final) {
    var els = document.querySelectorAll("video,audio,source");
    for (var i = 0; i < els.length; i++) {
      var v = els[i];
      var src = v.currentSrc || v.src || v.getAttribute("src");
      report("dom", src);
      if (v.tagName === "SOURCE") continue;
      if (!v.paused && v.readyState > 2) report("playing", src);
      if (v.paused) {
        try { v.muted = true; var p = v.play(); if (p && p.catch) p.catch(function () {}); } catch (e) {}
      }
    }
    if (final && window === window.top) {
      post({ kind: "title", url: "", title: document.title || "" });
      var html = document.documentElement ? document.documentElement.outerHTML : "";
      html = html.replace(/\\\\\\//g, "/").replace(/\\\\u002[fF]/g, "/").replace(/&amp;/g, "&");
      var m = html.match(/(?:https?:)?\\/\\/[^\\s"'<>\\\\]+?\\.(?:m3u8|flv)(?:\\?[^\\s"'<>\\\\]*)?/gi) || [];
      for (var j = 0; j < m.length && j < 20; j++) report("html", m[j]);
    }
  }
  setInterval(function () { scan(false); }, 1000);
  window.__vlSniffFinal = function () { scan(true); };
})();`;

// NUDGE plays the top document's videos and clicks the likeliest play button.
// Run by the host, so it counts as the user's gesture.
export const NUDGE = `(function () {
  document.querySelectorAll("video").forEach(function (v) {
    try { v.muted = true; var p = v.play(); if (p && p.catch) p.catch(function () {}); } catch (e) {}
  });
  var c = [].slice.call(document.querySelectorAll("button,[role=button],div,span,a")).filter(function (e) {
    var s = ((e.getAttribute("aria-label") || "") + " " + (typeof e.className === "string" ? e.className : "") +
      " " + (e.id || "")).toLowerCase();
    var r = e.getBoundingClientRect();
    return /play/.test(s) && !/playlist|player-?list|display|replay|playback-?rate/.test(s) && r.width > 20 && r.height > 20;
  });
  if (c[0]) c[0].click();
})();`;
