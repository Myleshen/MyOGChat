/* MyOGChat service worker.
   Cache-first for the static shell only; /api/* is always network-only. */
"use strict";

const CACHE = "myogchat-shell-v13";

const SHELL = [
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

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches
      .open(CACHE)
      .then((cache) => cache.addAll(SHELL))
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

  // Navigations: serve the cached app shell, fall back to network.
  if (request.mode === "navigate") {
    event.respondWith(
      caches
        .match(request)
        .then((cached) => cached || caches.match("index.html"))
        .then((cached) => cached || fetch(request))
    );
    return;
  }

  // Static shell: cache-first, then network (and cache the fresh copy).
  event.respondWith(
    caches.match(request).then((cached) => {
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
