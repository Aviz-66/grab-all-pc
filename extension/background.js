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

// Setup Context Menus
function setupContextMenus() {
  chrome.contextMenus.removeAll(() => {
    chrome.contextMenus.create({
      id: 'graball-link-direct',
      title: '⚡ Download Link with GrabAll (Direct)',
      contexts: ['link'],
    });

    chrome.contextMenus.create({
      id: 'graball-video-ytdlp',
      title: '🎬 Download Video with GrabAll (yt-dlp)',
      contexts: ['link', 'video', 'audio'],
    });

    chrome.contextMenus.create({
      id: 'graball-page-ytdlp',
      title: '🎬 Download Current Page Video (yt-dlp)',
      contexts: ['page'],
    });
  });
}

// Handle Context Menu Clicks
chrome.contextMenus.onClicked.addListener(async (info, tab) => {
  let targetUrl = '';
  let engine = 'auto';

  if (info.menuItemId === 'graball-link-direct') {
    targetUrl = info.linkUrl;
    engine = 'direct';
  } else if (info.menuItemId === 'graball-video-ytdlp') {
    targetUrl = info.srcUrl || info.linkUrl;
    engine = 'ytdlp';
  } else if (info.menuItemId === 'graball-page-ytdlp') {
    targetUrl = tab.url;
    engine = 'ytdlp';
  }

  if (targetUrl) {
    try {
      await sendToDaemon({
        url: targetUrl,
        pageUrl: tab?.url,
        referer: tab?.url,
        title: tab?.title,
        engine: engine,
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

// Send request to local Go daemon
async function sendToDaemon({ url, pageUrl, referer, title, fileName, engine = 'auto' }) {
  const config = await chrome.storage.local.get(DEFAULT_CONFIG);
  const daemonUrl = config.daemonUrl || 'http://127.0.0.1:17890';
  const token = config.authToken || '';

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
    userAgent: navigator.userAgent,
    referer: referer || pageUrl || '',
    cookies: cookies,
  };

  const response = await fetch(`${daemonUrl}/api/add`, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      'X-Daemon-Token': token,
    },
    body: JSON.stringify(payload),
  });

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
        const res = await fetch(`${config.daemonUrl}/api/ping`);
        if (res.ok) {
          const data = await res.json();
          sendResponse({ online: true, info: data });
        } else {
          sendResponse({ online: false });
        }
      } catch (e) {
        sendResponse({ online: false });
      }
    });
    return true;
  }
});
