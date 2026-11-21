/* MyOGChat service worker.
   The shell (/, index.html) is network-first so a deploy's fresh HTML — and
   therefore its ?v=<hash> asset URLs — always wins; the cached copy is only the
   offline fallback. Hashed assets are cache-first keyed by the exact URL, so a
   new hash is a cache miss by construction and an old body can never shadow a
   new one. /api/* is always network-only.

   The cache name is keyed by the version in this script's own URL
   (sw.js?v=<hash>, injected per build), so a deploy installs a fresh worker,
   replaces the cache, and claims the open pages. */
"use strict";

const VERSION = new URL(self.location.href).searchParams.get("v") || "0";
const CACHE = "myogchat-shell-v2-" + VERSION;

const ASSETS = [
  "app.js",
  "style.css",
  "manifest.webmanifest",
  "icons/icon.svg",
  "vendor/marked.min.js",
  "vendor/highlight.min.js",
  "vendor/highlight-theme.css",
  "vendor/highlight-light.css",
];

const withV = (u) => u + (u.indexOf("?") >= 0 ? "&" : "?") + "v=" + VERSION;

/* Fetch a shell URL past the HTTP cache and any CDN cache (?v=), so what is
   stored is what this build shipped, not a stale edge copy. */
async function fetchFresh(url) {
  try {
    const res = await fetch(new Request(url, { cache: "reload" }));
    return res.ok ? res : null;
  } catch (e) {
    return null;
  }
}

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches
      .open(CACHE)
      .then(async (cache) => {
        await Promise.all(
          [
            // The offline shell, under the exact keys a navigation asks for.
            (async () => {
              const res = await fetchFresh(withV("./"));
              if (res) {
                await cache.put("./", res.clone());
                await cache.put("index.html", res.clone());
              }
            })(),
            // Assets under the hashed URL this build's index.html points at.
            ...ASSETS.map(async (u) => {
              const res = await fetchFresh(withV(u));
              if (res) await cache.put(withV(u), res);
            }),
          ].map((p) => p.catch(() => {}))
        );
      })
      .then(() => self.skipWaiting())
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k)))
      )
      .then(() => self.clients.claim())
  );
});

self.addEventListener("fetch", (event) => {
  const request = event.request;
  if (request.method !== "GET") return;
  // Range requests (audio/export downloads) go straight to the network.
  if (request.headers.has("range")) return;

  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return;

  // API: network-only, never cached or wrapped.
  if (url.pathname === "/api" || url.pathname.startsWith("/api/")) return;

  if (request.mode === "navigate") {
    event.respondWith(networkFirstShell(request));
    return;
  }

  // A non-navigation request for the bare shell is the app checking for a
  // deploy: always fresh, never cached (navigations get the offline copy).
  if (url.pathname === "/" || url.pathname === "/index.html") {
    event.respondWith(fetch(request));
    return;
  }

  event.respondWith(cacheFirstExact(request));
});

/* Fresh HTML whenever the network answers; the cached shell only when it does
   not (offline, or a server that is down). */
async function networkFirstShell(request) {
  const cache = await caches.open(CACHE);
  try {
    const res = await fetch(request);
    if (res && res.ok) {
      const copy = res.clone();
      cache.put("index.html", copy.clone()).catch(() => {});
      cache.put("./", copy).catch(() => {});
    }
    return res;
  } catch (err) {
    const cached = (await cache.match("index.html")) || (await cache.match("./"));
    if (cached) return cached;
    throw err;
  }
}

/* Exact-URL matching: app.js?v=<hash> is a distinct cache entry per deploy. */
async function cacheFirstExact(request) {
  const cache = await caches.open(CACHE);
  const cached = await cache.match(request);
  if (cached) return cached;
  const res = await fetch(request);
  if (res && res.ok && res.type === "basic") {
    await cache.put(request, res.clone()).catch(() => {});
  }
  return res;
}
