/* MyOGChat service worker.
   Cache-first for the static shell only; /api/* is always network-only.

   The cache name and every shell URL are keyed by the version in this script's
   own URL (sw.js?v=<hash>, injected per build). A deploy therefore installs a
   fresh worker that fetches the shell with the same ?v=<hash> cache-buster —
   bypassing any edge/CDN cache that caches .js/.css — while storing under the
   plain URLs the app requests (matched with ignoreSearch). */
"use strict";

const VERSION = new URL(self.location.href).searchParams.get("v") || "0";
const CACHE = "myogchat-shell-" + VERSION;

const ASSETS = [
  "./",
  "index.html",
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

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches
      .open(CACHE)
      .then(async (cache) => {
        await Promise.all(
          ASSETS.map(async (u) => {
            try {
              // cache: "reload" skips the browser HTTP cache; ?v= skips any CDN
              // cache — the response is stored under the plain URL.
              const res = await fetch(new Request(withV(u), { cache: "reload" }));
              if (res.ok) await cache.put(u, res);
            } catch (e) {
              /* best effort; the network fallback still applies */
            }
          })
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

  const url = new URL(request.url);
  if (url.origin !== self.location.origin) return;

  // API: network-only, never cached.
  if (url.pathname === "/api" || url.pathname.startsWith("/api/")) return;

  // Navigations: network-first so a fresh index.html (with the current
  // ?v=<hash> asset URLs) always wins; fall back to the cached shell offline.
  if (request.mode === "navigate") {
    event.respondWith(
      fetch(request)
        .then((res) => {
          if (res.ok) {
            const clone = res.clone();
            caches.open(CACHE).then((cache) => cache.put("index.html", clone));
          }
          return res;
        })
        .catch(() => caches.match("index.html"))
    );
    return;
  }

  // Static shell: cache-first (ignoring the ?v= query), then network.
  event.respondWith(
    caches.match(request, { ignoreSearch: true }).then((cached) => {
      if (cached) return cached;
      return fetch(request).then((res) => {
        if (res.ok) {
          const clone = res.clone();
          caches.open(CACHE).then((cache) => cache.put(request, clone));
        }
        return res;
      });
    })
  );
});
