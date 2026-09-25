/* sitewatch service worker: cache-first for static assets, network-only for /api/ */
const CACHE = "sitewatch-v1";

self.addEventListener("install", (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(["/", "/manifest.json"])));
});

self.addEventListener("activate", (e) => {
  e.waitUntil(caches.keys().then((keys) =>
    Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k)))
  ));
});

self.addEventListener("fetch", (e) => {
  if (new URL(e.request.url).pathname.startsWith("/api/")) return; // fall through: network-only
  e.respondWith(caches.match(e.request).then((hit) => hit || fetch(e.request)));
});
