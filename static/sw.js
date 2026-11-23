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

/* Web Push (6.6). The server sends JSON: {title, body, url, tag}. Only the
   conversation title and a status word are ever in it — never message text. */
self.addEventListener("push", (event) => {
  let data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch (e) {
    data = {};
  }
  const title = data.title || "MyOGChat";
  event.waitUntil(
    self.registration.showNotification(title, {
      body: data.body || "Reply ready",
      // The conversation id: a newer notification replaces an older one.
      tag: data.tag || "myogchat",
      data: { url: data.url || "/" },
      icon: "icons/icon.svg",
      badge: "icons/icon.svg",
    })
  );
});

/* Tapping a notification focuses an open tab and navigates it to the
   conversation; with no tab open it opens a new one. */
self.addEventListener("notificationclick", (event) => {
  event.notification.close();
  const url = (event.notification.data && event.notification.data.url) || "/";
  event.waitUntil(
    self.clients.matchAll({ type: "window", includeUncontrolled: true }).then((windows) => {
      for (const client of windows) {
        if (client.url.indexOf(self.location.origin) !== 0) continue;
        return client.focus().then((focused) => {
          if (focused && "navigate" in focused) {
            return focused.navigate(client.url.split("#")[0] + url).catch(() => focused);
          }
          return focused;
        });
      }
      return self.clients.openWindow(url);
    })
  );
});
