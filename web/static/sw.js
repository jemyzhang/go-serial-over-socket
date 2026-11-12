// Serial Over Socket - Service Worker
// Provides offline support and caching for PWA functionality
//
// ============================================================
//  发版更新说明（必读）
// ============================================================
//  每次更新 JS / CSS / HTML 后，只需把下面 CACHE_NAME 的版本号 +1：
//    例如：v6 → v7
//  浏览器会检测到 sw.js 文件内容变化，自动下载新 SW，
//  安装完成后页面顶部出现更新横幅，用户点击"立即更新"后才会刷新。
// ============================================================
const CACHE_NAME = 'serial-over-socket-v18'; // ← 每次发版只改这里

// Static assets to cache on install
const STATIC_ASSETS = [
    '/',
    '/static/js/xterm.js',
    '/static/js/xterm-addon-fit.js',
    '/static/js/xterm-addon-search.js',
    '/static/js/app.js',
    '/static/css/xterm.min.css',
    '/static/icons/icon-192x192.png',
    '/static/icons/icon-512x512.png',
    '/manifest.json',
];

// Install event: pre-cache static assets
// 注意：这里 *不* 调用 skipWaiting()。
// 新 SW 安装完成后进入 waiting 状态，等待页面发送 SKIP_WAITING 消息才会激活。
self.addEventListener('install', (event) => {
    console.log('[SW] Installing Service Worker...');
    event.waitUntil(
        caches.open(CACHE_NAME).then((cache) => {
            console.log('[SW] Pre-caching static assets');
            // Cache assets individually so one failure doesn't block the others
            return Promise.allSettled(
                STATIC_ASSETS.map((url) =>
                    cache.add(url).catch((err) => {
                        console.warn(`[SW] Failed to cache ${url}:`, err);
                    })
                )
            );
        }).then(() => {
            console.log('[SW] Installation complete - waiting for client approval to activate');
            // 不自动 skipWaiting：新 SW 保持 waiting，等待用户点击"立即更新"
        })
    );
});

// Activate event: clean up old caches
self.addEventListener('activate', (event) => {
    console.log('[SW] Activating Service Worker...');
    event.waitUntil(
        caches.keys().then((cacheNames) => {
            return Promise.all(
                cacheNames
                    .filter((name) => name !== CACHE_NAME)
                    .map((name) => {
                        console.log(`[SW] Deleting old cache: ${name}`);
                        return caches.delete(name);
                    })
            );
        }).then(() => {
            console.log('[SW] Activation complete');
            return self.clients.claim();
        })
    );
});

// Fetch event: serve from cache with network fallback
self.addEventListener('fetch', (event) => {
    const url = new URL(event.request.url);

    // Skip non-GET requests
    if (event.request.method !== 'GET') return;

    // Skip WebSocket connections
    if (url.protocol === 'ws:' || url.protocol === 'wss:') return;

    // Skip API requests - always fetch from network
    if (url.pathname.startsWith('/api/') || url.pathname === '/ws') return;

    // For navigation requests (HTML pages), use network-first strategy
    if (event.request.mode === 'navigate') {
        event.respondWith(
            fetch(event.request)
                .then((response) => {
                    // Cache a fresh copy if successful
                    if (response && response.status === 200) {
                        const responseToCache = response.clone();
                        caches.open(CACHE_NAME).then((cache) => {
                            cache.put(event.request, responseToCache);
                        });
                    }
                    return response;
                })
                .catch(() => {
                    // Offline fallback: serve from cache
                    return caches.match('/').then((cached) => {
                        if (cached) {
                            console.log('[SW] Serving cached page (offline)');
                            return cached;
                        }
                        // Return a minimal offline page
                        return new Response(
                            `<!DOCTYPE html>
              <html lang="en">
              <head>
                <meta charset="UTF-8">
                <meta name="viewport" content="width=device-width, initial-scale=1.0">
                <title>Serial Over Socket - Offline</title>
                <style>
                  body { 
                    font-family: system-ui; background: #1e1e1e; color: #e0e0e0;
                    display: flex; align-items: center; justify-content: center; 
                    min-height: 100vh; margin: 0; text-align: center;
                  }
                  h1 { color: #5c6bc0; } p { color: #a0a0a0; }
                </style>
              </head>
              <body>
                <div>
                  <h1>📡 Serial Over Socket</h1>
                  <p>You are offline. Please connect to the server to use the terminal.</p>
                </div>
              </body>
              </html>`,
                            { headers: { 'Content-Type': 'text/html' } }
                        );
                    });
                })
        );
        return;
    }

    // For static assets: cache-first strategy
    event.respondWith(
        caches.match(event.request).then((cached) => {
            if (cached) {
                // Return cached version; background revalidation happens via the new SW install cycle
                return cached;
            }

            // Not in cache: fetch from network and cache it
            return fetch(event.request).then((response) => {
                if (!response || response.status !== 200 || response.type === 'opaque') {
                    return response;
                }
                const responseToCache = response.clone();
                caches.open(CACHE_NAME).then((cache) => {
                    cache.put(event.request, responseToCache);
                });
                return response;
            });
        })
    );
});

// Listen for messages from the client
// 唯一触发新版本接管的入口：页面发送 { type: 'SKIP_WAITING' } 后新 SW 才会激活
let skipWaitingTriggered = false;
self.addEventListener('message', (event) => {
    if (event.data && event.data.type === 'SKIP_WAITING') {
        if (!skipWaitingTriggered) {
            skipWaitingTriggered = true;
            console.log('[SW] Received SKIP_WAITING - activating new version...');
            self.skipWaiting();
        }
    }
});
