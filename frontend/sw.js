// GraveStack service worker: push delivery + minimal shell cache.
const CACHE = 'gravestack-v3';
const SHELL = ['/', '/index.html', '/style.css', '/app.js', '/manifest.webmanifest', '/icons/icon-192.png'];

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys().then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

// Network-first for everything: always try fresh, cache as fallback for offline.
self.addEventListener('fetch', (e) => {
  const url = new URL(e.request.url);
  if (url.pathname.startsWith('/api/') || url.pathname.startsWith('/internal/')) return;
  e.respondWith(
    fetch(e.request).then((res) => {
      const cacheable = url.origin === location.origin
        || url.hostname === 'fonts.googleapis.com'
        || url.hostname === 'fonts.gstatic.com';
      if (e.request.method === 'GET' && res.ok && cacheable) {
        const copy = res.clone();
        caches.open(CACHE).then((c) => c.put(e.request, copy));
      }
      return res;
    }).catch(() => caches.match(e.request).then((hit) => hit || caches.match('/')))
  );
});

// Big-picture push notification — the product, not a reminder.
self.addEventListener('push', (e) => {
  let data = {};
  try { data = e.data.json(); } catch (_) {}
  const title = data.title || 'GraveStack';
  const options = {
    body: data.body || '',
    icon: data.icon || '/icons/icon-192.png',
    badge: data.badge || '/icons/badge.png',
    image: data.image || undefined,     // big-picture cover (Android)
    data: { url: data.url || '/' },
    tag: 'gravestack-daily',
    renotify: true,
  };
  e.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener('notificationclick', (e) => {
  e.notification.close();
  const target = (e.notification.data && e.notification.data.url) || '/';
  e.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then((cls) => {
      for (const c of cls) { if ('focus' in c) { c.navigate(target); return c.focus(); } }
      return self.clients.openWindow(target);
    })
  );
});
