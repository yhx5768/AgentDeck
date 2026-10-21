/* cc-webui service worker: app-shell precache + network-first runtime cache */
const VERSION = 'cc-webui-v1';
const SHELL = [
  '/',
  '/index.html',
  '/manifest.json',
  '/style.css',
  '/app.js',
  '/icons/icon-192.png',
  '/icons/icon-512.png',
  '/icons/icon-512-maskable.png',
  '/vendor/marked.min.js',
  '/vendor/purify.min.js',
  '/vendor/highlight.min.js',
  '/vendor/lucide.min.js',
  '/vendor/mermaid.min.js',
  '/vendor/github.min.css',
  '/vendor/github-dark.min.css',
];

self.addEventListener('install', (e) => {
  e.waitUntil(
    caches.open(VERSION).then((cache) => cache.addAll(SHELL)).then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => k !== VERSION).map((k) => caches.delete(k)))
    ).then(() => self.clients.claim())
  );
});

const isAPI = (url) =>
  url.pathname.startsWith('/api/') ||
  url.pathname.startsWith('/login') ||
  url.pathname === '/logout';

self.addEventListener('fetch', (e) => {
  const req = e.request;
  const url = new URL(req.url);
  if (req.method !== 'GET' || url.origin !== self.location.origin) return;
  if (isAPI(url)) return; // never cache API/auth/SSE

  if (req.mode === 'navigate') {
    e.respondWith(
      fetch(req).then((res) => {
        if (res.ok) {
          const copy = res.clone();
          caches.open(VERSION).then((c) => c.put('/index.html', copy));
        }
        return res;
      }).catch(() => caches.match('/index.html'))
    );
    return;
  }

  // static assets: network-first with cache fallback
  e.respondWith(
    fetch(req).then((res) => {
      if (res.ok) {
        const copy = res.clone();
        caches.open(VERSION).then((c) => c.put(req, copy));
      }
      return res;
    }).catch(() => caches.match(req))
  );
});
