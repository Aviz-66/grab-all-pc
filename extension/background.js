// GrabAll Background Service Worker (Manifest V3)

const DEFAULT_CONFIG = {
  daemonUrl: 'http://127.0.0.1:17890',
  authToken: '',
  interceptDownloads: false,
  autoSniff: true,
};

// In-memory cache of sniffed media per tab: tabId -> Array<{ url, type, time }>
const tabMedia = new Map();

// Initialize configuration
chrome.runtime.onInstalled.addListener(async () => {
  const existing = await chrome.storage.local.get(null);
  const updated = { ...DEFAULT_CONFIG, ...existing };
  await chrome.storage.local.set(updated);
  setupContextMenus();
});

chrome.runtime.onStartup.addListener(() => {
  setupContextMenus();
});

// Setup Context Menus
function setupContextMenus() {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({
      id: 'graball-link-direct',
      title: '⚡ Download Link with GrabAll (Direct)',
      contexts: ['link'],
    });

    chrome.contextMenus.create({
      id: 'graball-page-best',
      title: '🎬 Download Video — Best Quality',
      contexts: ['page', 'link', 'video'],
    });

    chrome.contextMenus.create({
      id: 'graball-page-1080',
      title: '📺 Download Video — 1080p',
      contexts: ['page', 'link', 'video'],
    });

    chrome.contextMenus.create({
      id: 'graball-page-720',
      title: '📺 Download Video — 720p',
      contexts: ['page', 'link', 'video'],
    });

    chrome.contextMenus.create({
      id: 'graball-page-audio',
      title: '🎵 Download Audio Only (MP3)',
      contexts: ['page', 'link', 'video', 'audio'],
    });

    chrome.contextMenus.create({
      id: 'graball-playlist',
      title: '📑 Download Entire Playlist',
      contexts: ['page', 'link'],
    });
  });
}

// Handle Context Menu Clicks
chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  let targetUrl = '';
  let engine = 'ytdlp';
  let quality = 'best';
  let playlist = false;

  const stored = await chrome.storage.local.get({ preferredQuality: 'best' });
  if (stored.preferredQuality) {
    quality = stored.preferredQuality;
  }

  if (info.menuItemId === 'graball-link-direct') {
    targetUrl = info.linkUrl;
    engine = 'direct';
  } else if (info.menuItemId === 'graball-page-best') {
    targetUrl = info.linkUrl || info.srcUrl || tab?.url;
    quality = 'best';
  } else if (info.menuItemId === 'graball-page-1080') {
    targetUrl = info.linkUrl || info.srcUrl || tab?.url;
    quality = '1080';
  } else if (info.menuItemId === 'graball-page-720') {
    targetUrl = info.linkUrl || info.srcUrl || tab?.url;
    quality = '720';
  } else if (info.menuItemId === 'graball-page-audio') {
    targetUrl = info.linkUrl || info.srcUrl || tab?.url;
    quality = 'audio-mp3';
  } else if (info.menuItemId === 'graball-playlist') {
    targetUrl = info.linkUrl || tab?.url;
    playlist = true;
  }

  if (targetUrl) {
    try {
      await sendToDaemon({
        url: targetUrl,
        pageUrl: tab?.url,
        referer: tab?.url,
        title: tab?.title,
        engine: engine,
        quality: quality,
        playlist: playlist,
      });
      showBadgeSuccess(tab?.id);
    } catch (err) {
      console.error('Failed to send download:', err);
      showBadgeError(tab?.id);
    }
  }
});

// Sniff media stream requests (.m3u8, .mpd, .mp4, etc.)
chrome.webRequest.onBeforeRequest.addListener(
  (details) => {
    if (details.tabId < 0) return;

    const url = details.url;
    const lower = url.toLowerCase();

    let mediaType = null;
    if (lower.includes('.m3u8') || lower.includes('playlist.m3u8')) {
      mediaType = 'HLS (m3u8)';
    } else if (lower.includes('.mpd') || lower.includes('manifest.mpd')) {
      mediaType = 'DASH (mpd)';
    } else if (lower.endsWith('.mp4') || lower.includes('.mp4?')) {
      mediaType = 'MP4 Video';
    } else if (lower.endsWith('.webm') || lower.includes('.webm?')) {
      mediaType = 'WebM Video';
    }

    if (mediaType) {
      let list = tabMedia.get(details.tabId) || [];
      if (!list.some((item) => item.url === url)) {
        list.push({
          url: url,
          type: mediaType,
          time: Date.now(),
        });
        if (list.length > 25) list.shift();
        tabMedia.set(details.tabId, list);

        // Update badge
        chrome.action.setBadgeText({
          text: list.length.toString(),
          tabId: details.tabId,
        });
        chrome.action.setBadgeBackgroundColor({
          color: '#3b82f6',
          tabId: details.tabId,
        });
      }
    }
  },
  { urls: ['<all_urls>'] }
);

// Clean up tab media cache when tab is closed
chrome.tabs.onRemoved.addListener((tabId) => {
  tabMedia.delete(tabId);
});

// Intercept browser downloads if enabled
chrome.downloads.onCreated.addListener(async (downloadItem) => {
  const config = await chrome.storage.local.get(DEFAULT_CONFIG);
  if (!config.interceptDownloads) return;

  // Don't intercept downloads originating from localhost daemon
  if (downloadItem.url.includes('127.0.0.1') || downloadItem.url.includes('localhost')) {
    return;
  }

  // Cancel and erase browser's native download
  try {
    await chrome.downloads.cancel(downloadItem.id);
    await chrome.downloads.erase({ id: downloadItem.id });
  } catch (err) {
    // Might fail if already completed or canceled
  }

  // Forward to GrabAll daemon
  try {
    await sendToDaemon({
      url: downloadItem.url,
      pageUrl: downloadItem.referrer,
      referer: downloadItem.referrer,
      fileName: downloadItem.filename ? downloadItem.filename.split(/[\\/]/).pop() : '',
      engine: 'direct',
    });
  } catch (err) {
    console.error('Failed to intercept download:', err);
  }
});

// Auto-pair extension with local GrabAll app (zero manual token copy-pasting)
async function autoPairToken(daemonUrl) {
  try {
    const res = await fetch(`${daemonUrl}/api/pair`, {
      method: 'POST',
      headers: {
        'X-GrabAll-Ext': chrome.runtime.id || 'graball-mv3',
      },
    });
    if (res.ok) {
      const data = await res.json();
      if (data && data.token) {
        await chrome.storage.local.set({ authToken: data.token });
        return data.token;
      }
    }
  } catch (e) {
    // Daemon offline or older version
  }
  return '';
}

// Send request to local Go daemon
async function sendToDaemon({ url, pageUrl, referer, title, fileName, engine = 'auto', quality = 'best', playlist = false }) {
  const config = await chrome.storage.local.get(DEFAULT_CONFIG);
  const daemonUrl = config.daemonUrl || 'http://127.0.0.1:17890';
  let token = config.authToken || '';
  if (!token) {
    token = await autoPairToken(daemonUrl);
  }

  // Collect relevant cookies
  let cookies = [];
  try {
    const primaryCookies = await chrome.cookies.getAll({ url: url });
    cookies.push(...primaryCookies);

    if (pageUrl && pageUrl !== url) {
      const pageCookies = await chrome.cookies.getAll({ url: pageUrl });
      for (const pc of pageCookies) {
        if (!cookies.some((c) => c.name === pc.name && c.domain === pc.domain)) {
          cookies.push(pc);
        }
      }
    }
  } catch (err) {
    console.warn('Could not read cookies for URL:', url, err);
  }

  const payload = {
    url: url,
    title: title || '',
    fileName: fileName || '',
    engine: engine,
    quality: quality || 'best',
    playlist: Boolean(playlist),
    userAgent: navigator.userAgent,
    referer: referer || pageUrl || '',
    cookies: cookies,
  };

  let response = await fetch(`${daemonUrl}/api/add`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'X-Daemon-Token': token,
      'X-GrabAll-Ext': chrome.runtime.id || 'graball-mv3',
    },
    body: JSON.stringify(payload),
  });

  // If token rotated or missing, auto-pair once and retry transparently
  if (response.status === 401) {
    const refreshed = await autoPairToken(daemonUrl);
    if (refreshed) {
      response = await fetch(`${daemonUrl}/api/add`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          'X-Daemon-Token': refreshed,
          'X-GrabAll-Ext': chrome.runtime.id || 'graball-mv3',
        },
        body: JSON.stringify(payload),
      });
    }
  }

  if (!response.ok) {
    const errorData = await response.json().catch(() => ({}));
    throw new Error(errorData.error || `HTTP ${response.status}`);
  }

  return await response.json();
}

function showBadgeSuccess(tabId) {
  if (!tabId) return;
  chrome.action.setBadgeText({ text: '✓', tabId });
  chrome.action.setBadgeBackgroundColor({ color: '#10b981', tabId });
  setTimeout(() => {
    const count = (tabMedia.get(tabId) || []).length;
    chrome.action.setBadgeText({ text: count > 0 ? count.toString() : '', tabId });
    chrome.action.setBadgeBackgroundColor({ color: '#3b82f6', tabId });
  }, 2000);
}

function showBadgeError(tabId) {
  if (!tabId) return;
  chrome.action.setBadgeText({ text: '!', tabId });
  chrome.action.setBadgeBackgroundColor({ color: '#ef4444', tabId });
  setTimeout(() => {
    const count = (tabMedia.get(tabId) || []).length;
    chrome.action.setBadgeText({ text: count > 0 ? count.toString() : '', tabId });
    chrome.action.setBadgeBackgroundColor({ color: '#3b82f6', tabId });
  }, 2000);
}

// Runtime message listener for popup & content scripts
chrome.runtime.onMessage.addListener((message, sender, sendResponse) => {
  if (message.type === 'MEDIA_DETECTED_FROM_DOM') {
    const tabId = sender?.tab?.id;
    const item = message.media;
    if (tabId >= 0 && item && item.url) {
      const list = tabMedia.get(tabId) || [];
      if (!list.some((m) => m.url === item.url)) {
        list.push({ url: item.url, type: item.type || 'Video', time: Date.now() });
        if (list.length > 25) list.shift();
        tabMedia.set(tabId, list);
      }
    }
    sendResponse({ ok: true });
    return true;
  }

  if (message.type === 'GET_TAB_MEDIA') {
    const list = tabMedia.get(message.tabId) || [];
    sendResponse({ media: list });
    return true;
  }

  if (message.type === 'SEND_DOWNLOAD') {
    sendToDaemon(message.payload)
      .then((res) => sendResponse({ success: true, data: res }))
      .catch((err) => sendResponse({ success: false, error: err.message }));
    return true;
  }

  if (message.type === 'CHECK_DAEMON') {
    chrome.storage.local.get(DEFAULT_CONFIG).then(async (config) => {
      try {
        const daemonUrl = config.daemonUrl || 'http://127.0.0.1:17890';
        const res = await fetch(`${daemonUrl}/api/ping`);
        if (res.ok) {
          const data = await res.json();
          let token = config.authToken || '';
          if (!token) {
            token = await autoPairToken(daemonUrl);
          }
          sendResponse({ online: true, info: data, token });
        } else {
          sendResponse({ online: false });
        }
      } catch (e) {
        sendResponse({ online: false });
      }
    });
    return true;
  }

  if (message.type === 'OPEN_GRABALL_APP') {
    chrome.storage.local.get(DEFAULT_CONFIG).then(async (config) => {
      const daemonUrl = config.daemonUrl || 'http://127.0.0.1:17890';
      let token = config.authToken || '';
      if (!token) {
        token = await autoPairToken(daemonUrl);
      }
      try {
        let res = await fetch(`${daemonUrl}/api/show-window`, {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', 'X-Daemon-Token': token },
          body: '{}',
        });
        if (res.status === 401) {
          token = await autoPairToken(daemonUrl);
          res = await fetch(`${daemonUrl}/api/show-window`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json', 'X-Daemon-Token': token },
            body: '{}',
          });
        }
        if (res.ok) {
          const data = await res.json();
          sendResponse({ success: true, nativeGui: Boolean(data.nativeGui), daemonUrl });
          return;
        }
      } catch (e) {
        // Fallback to web tab
      }
      sendResponse({ success: false, nativeGui: false, daemonUrl });
    });
    return true;
  }
});
