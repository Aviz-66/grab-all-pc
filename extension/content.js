// GrabAll Content Script v1.2.0
// - YouTube Watch, Shorts & Playlist auto-capture with Resolution/Audio selector
// - IDM-style per-video hover overlay for scrolling feeds (Facebook, TikTok, IG, X, Reddit, etc.)

(function () {
  'use strict';

  const YT_WIDGET_ID = 'graball-yt-widget';
  const FLOATING_WIDGET_ID = 'graball-floating-widget';
  const HOVER_WIDGET_ID = 'graball-hover-widget';

  let lastUrl = '';
  let preferredQuality = 'best';

  chrome.storage.local.get({ preferredQuality: 'best' }, (cfg) => {
    if (cfg && cfg.preferredQuality) {
      preferredQuality = cfg.preferredQuality;
      syncQualitySelects(preferredQuality);
    }
  });

  chrome.storage.onChanged.addListener((changes) => {
    if (changes.preferredQuality && changes.preferredQuality.newValue) {
      preferredQuality = changes.preferredQuality.newValue;
      syncQualitySelects(preferredQuality);
    }
  });

  function syncQualitySelects(val) {
    document.querySelectorAll('.graball-quality-select').forEach((sel) => {
      sel.value = val;
    });
  }

  function createQualitySelect(compact = false) {
    const select = document.createElement('select');
    select.className = 'graball-quality-select';
    select.title = 'Select Video Resolution or Audio Only';

    const options = [
      { value: 'best', label: compact ? '🎬 Best' : '🎬 Best' },
      { value: '2160', label: '📺 4K' },
      { value: '1080', label: '📺 1080p' },
      { value: '720', label: '📺 720p' },
      { value: '480', label: '📺 480p' },
      { value: '360', label: '📺 360p' },
      { value: 'audio-mp3', label: '🎵 MP3' },
      { value: 'audio-m4a', label: '🎵 M4A' },
    ];

    options.forEach((opt) => {
      const el = document.createElement('option');
      el.value = opt.value;
      el.textContent = opt.label;
      el.style.backgroundColor = '#1e293b';
      el.style.color = '#f8fafc';
      select.appendChild(el);
    });

    select.value = preferredQuality;

    const h = compact ? '30px' : '36px';
    Object.assign(select.style, {
      backgroundColor: '#1e293b',
      color: '#ffffff',
      border: 'none',
      borderRight: '1px solid rgba(255, 255, 255, 0.2)',
      borderRadius: '18px 0 0 18px',
      padding: compact ? '0 6px 0 10px' : '0 8px 0 12px',
      fontSize: compact ? '12px' : '13px',
      fontWeight: '600',
      fontFamily: '"Roboto", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
      cursor: 'pointer',
      height: h,
      outline: 'none',
    });

    select.addEventListener('click', (e) => e.stopPropagation());
    select.addEventListener('mousedown', (e) => e.stopPropagation());
    select.addEventListener('change', (e) => {
      e.stopPropagation();
      preferredQuality = select.value;
      chrome.storage.local.set({ preferredQuality });
    });

    return select;
  }

  function sendDownloadRequest({ btn, defaultLabel, defaultBg = '#3b82f6', targetUrl, title, quality, playlist = false }) {
    btn.textContent = playlist ? '⏳ Scanning...' : '⏳ Sending...';
    btn.disabled = true;

    chrome.runtime.sendMessage(
      {
        type: 'SEND_DOWNLOAD',
        payload: {
          url: targetUrl,
          pageUrl: location.href,
          referer: location.href,
          title: title || document.title,
          engine: 'ytdlp',
          quality: quality || preferredQuality,
          playlist: Boolean(playlist),
        },
      },
      (res) => {
        if (chrome.runtime.lastError || !res || !res.success) {
          btn.textContent = '❌ Error';
          btn.style.backgroundColor = '#ef4444';
          setTimeout(() => {
            btn.textContent = defaultLabel;
            btn.style.backgroundColor = defaultBg;
            btn.disabled = false;
          }, 2500);
        } else {
          const count = res.data?.count;
          btn.textContent = playlist && count ? `✅ ${count} Queued!` : '✅ Queued!';
          btn.style.backgroundColor = '#10b981';
          setTimeout(() => {
            btn.textContent = defaultLabel;
            btn.style.backgroundColor = defaultBg;
            btn.disabled = false;
          }, 3000);
        }
      }
    );
  }

  // ─── YouTube Helpers (Watch, Shorts, Playlists) ────────────────────

  function isYouTubeHost() {
    return (
      location.hostname === 'www.youtube.com' ||
      location.hostname === 'youtube.com' ||
      location.hostname === 'm.youtube.com' ||
      location.hostname === 'music.youtube.com'
    );
  }

  function isYouTubeWatchOrShorts() {
    return (
      isYouTubeHost() &&
      (location.pathname === '/watch' || location.pathname.startsWith('/shorts/'))
    );
  }

  function isYouTubePlaylistPage() {
    return isYouTubeHost() && location.pathname === '/playlist' && location.search.includes('list=');
  }

  function getWatchPlaylistId() {
    try {
      const listId = new URLSearchParams(location.search).get('list');
      if (!listId) return null;
      // Skip infinite auto-generated YouTube Radio mixes (RD...)
      if (listId.startsWith('RD')) return null;
      return listId;
    } catch {
      return null;
    }
  }

  function getYouTubeTitle() {
    const selectors = [
      'h1.ytd-watch-metadata yt-formatted-string',
      'yt-page-header-view-model h1 span',
      'ytd-playlist-header-renderer #text',
      'ytd-reel-player-header-renderer h2 yt-formatted-string',
      '#title h1 yt-formatted-string',
    ];
    for (const sel of selectors) {
      const el = document.querySelector(sel);
      if (el && el.textContent.trim()) {
        return el.textContent.trim();
      }
    }
    return document.title.replace(' - YouTube', '').trim();
  }

  function buildActionPill({ id, label, isFloating, playlist = false, extraPlaylistBtn = false, titleFn }) {
    const wrapper = document.createElement('div');
    wrapper.id = id;

    Object.assign(wrapper.style, {
      display: 'inline-flex',
      alignItems: 'center',
      height: '36px',
      borderRadius: '18px',
      overflow: 'hidden',
      boxShadow: isFloating ? '0 4px 16px rgba(15, 23, 42, 0.6)' : 'none',
      marginLeft: isFloating ? '0' : '8px',
      verticalAlign: 'middle',
      zIndex: isFloating ? '2147483646' : '999',
    });

    if (isFloating) {
      Object.assign(wrapper.style, {
        position: 'fixed',
        bottom: '20px',
        right: '20px',
      });
    }

    const select = createQualitySelect(false);

    const btn = document.createElement('button');
    btn.textContent = label;
    btn.title = playlist ? 'Queue entire playlist in GrabAll' : 'Download video with GrabAll';

    Object.assign(btn.style, {
      backgroundColor: '#3b82f6',
      color: '#ffffff',
      border: 'none',
      borderRadius: extraPlaylistBtn ? '0' : '0 18px 18px 0',
      padding: '0 14px',
      fontSize: '13px',
      fontWeight: '700',
      fontFamily: '"Roboto", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
      cursor: 'pointer',
      height: '36px',
      display: 'inline-flex',
      alignItems: 'center',
      transition: 'background-color 0.2s ease',
      whiteSpace: 'nowrap',
    });

    btn.addEventListener('mouseenter', () => {
      if (!btn.disabled) btn.style.backgroundColor = '#2563eb';
    });
    btn.addEventListener('mouseleave', () => {
      if (!btn.disabled) btn.style.backgroundColor = '#3b82f6';
    });

    btn.addEventListener('click', (e) => {
      e.preventDefault();
      e.stopPropagation();
      sendDownloadRequest({
        btn,
        defaultLabel: label,
        targetUrl: location.href,
        title: titleFn(),
        quality: select.value,
        playlist,
      });
    });

    wrapper.appendChild(select);
    wrapper.appendChild(btn);

    if (extraPlaylistBtn) {
      const plBtn = document.createElement('button');
      plBtn.textContent = '📑 Playlist';
      plBtn.title = 'Download entire playlist into a subfolder';
      Object.assign(plBtn.style, {
        backgroundColor: '#0284c7',
        color: '#ffffff',
        border: 'none',
        borderLeft: '1px solid rgba(255, 255, 255, 0.25)',
        borderRadius: '0 18px 18px 0',
        padding: '0 12px',
        fontSize: '12px',
        fontWeight: '700',
        fontFamily: '"Roboto", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
        cursor: 'pointer',
        height: '36px',
        display: 'inline-flex',
        alignItems: 'center',
        whiteSpace: 'nowrap',
      });
      plBtn.addEventListener('mouseenter', () => {
        if (!plBtn.disabled) plBtn.style.backgroundColor = '#0369a1';
      });
      plBtn.addEventListener('mouseleave', () => {
        if (!plBtn.disabled) plBtn.style.backgroundColor = '#0284c7';
      });
      plBtn.addEventListener('click', (e) => {
        e.preventDefault();
        e.stopPropagation();
        sendDownloadRequest({
          btn: plBtn,
          defaultLabel: '📑 Playlist',
          defaultBg: '#0284c7',
          targetUrl: location.href,
          title: titleFn(),
          quality: select.value,
          playlist: true,
        });
      });
      wrapper.appendChild(plBtn);
    }

    return wrapper;
  }

  function injectYouTubeWidget() {
    if (!isYouTubeWatchOrShorts() && !isYouTubePlaylistPage()) {
      removeYouTubeWidget();
      return;
    }

    if (document.getElementById(YT_WIDGET_ID)) {
      return;
    }

    // 1. Dedicated YouTube Playlist page (/playlist?list=...)
    if (isYouTubePlaylistPage()) {
      const plHeaderActions =
        document.querySelector('yt-page-header-view-model .ytFlexibleActionsViewModelActionRow') ||
        document.querySelector('ytd-playlist-header-renderer .immersive-header-content .metadata-action-bar') ||
        document.querySelector('ytd-playlist-header-renderer .play-menu');

      const pill = buildActionPill({
        id: YT_WIDGET_ID,
        label: '📑 Grab Playlist',
        isFloating: !plHeaderActions,
        playlist: true,
        titleFn: getYouTubeTitle,
      });

      if (plHeaderActions) {
        plHeaderActions.appendChild(pill);
      } else {
        document.body.appendChild(pill);
      }
      return;
    }

    // 2. YouTube Shorts
    if (location.pathname.startsWith('/shorts/')) {
      const floating = buildActionPill({
        id: YT_WIDGET_ID,
        label: '⚡ GrabAll',
        isFloating: true,
        playlist: false,
        titleFn: getYouTubeTitle,
      });
      document.body.appendChild(floating);
      return;
    }

    // 3. YouTube Watch page (/watch?v=... [&list=...])
    const hasPlaylist = Boolean(getWatchPlaylistId());
    const actionBar =
      document.querySelector('ytd-watch-metadata #actions #top-level-buttons-computed') ||
      document.querySelector('#actions #top-level-buttons-computed') ||
      document.querySelector('#menu #top-level-buttons-computed') ||
      document.querySelector('ytd-watch-metadata #actions');

    if (actionBar) {
      const widget = buildActionPill({
        id: YT_WIDGET_ID,
        label: '⚡ GrabAll',
        isFloating: false,
        playlist: false,
        extraPlaylistBtn: hasPlaylist,
        titleFn: getYouTubeTitle,
      });
      actionBar.appendChild(widget);
    } else if (document.querySelector('video')) {
      const floating = buildActionPill({
        id: YT_WIDGET_ID,
        label: '⚡ GrabAll',
        isFloating: true,
        playlist: false,
        extraPlaylistBtn: hasPlaylist,
        titleFn: getYouTubeTitle,
      });
      document.body.appendChild(floating);
    }
  }

  function removeYouTubeWidget() {
    const existing = document.getElementById(YT_WIDGET_ID);
    if (existing) existing.remove();
  }

  // ─── IDM-Style Per-Video Hover Overlay (Feeds & Any Video Site) ────

  let hoverWidget = null;
  let hoveredVideo = null;
  let hideHoverTimer = null;

  function isSingleVideoPermalinkPage() {
    const p = location.pathname.toLowerCase();
    const s = location.search.toLowerCase();
    return (
      p.includes('/reel/') ||
      p.includes('/reels/') ||
      p.includes('/shorts/') ||
      p.includes('/status/') ||
      p.includes('/video/') ||
      p.includes('/videos/') ||
      p.startsWith('/watch') ||
      s.includes('v=') ||
      s.includes('story_fbid=')
    );
  }

  // Finds the specific post/reel permalink when scrolling a feed (FB, IG, X, TikTok, Reddit)
  function resolveVideoTarget(videoEl) {
    // 1. Direct non-blob HTTP source on generic HTML5 video players
    const directSrc = videoEl.currentSrc || videoEl.src;
    if (directSrc && /^https?:\/\//i.test(directSrc) && !isYouTubeHost()) {
      // On social platforms, CDN mp4 URLs often lack audio; prefer post permalink if on social sites
      const socialHosts = ['facebook.com', 'fb.watch', 'instagram.com', 'tiktok.com', 'x.com', 'twitter.com', 'reddit.com'];
      const onSocial = socialHosts.some((h) => location.hostname.includes(h));
      if (!onSocial) {
        return { url: directSrc, title: document.title };
      }
    }

    // 2. Search ancestor post/article card for a specific video/reel/status permalink
    const permalinkPatterns = [
      /\/reel\/[A-Za-z0-9_-]+/,
      /\/reels\/[A-Za-z0-9_-]+/,
      /\/videos\/\d+/,
      /\/watch\/?\?v=\d+/,
      /\/share\/[vr]\/[A-Za-z0-9_-]+/,
      /story_fbid=/,
      /\/status\/\d+/,
      /\/@[^/]+\/video\/\d+/,
      /\/p\/[A-Za-z0-9_-]+/,
      /\/comments\/[A-Za-z0-9]+/,
    ];

    let node = videoEl;
    for (let depth = 0; depth < 14 && node && node !== document.body; depth++) {
      if (node.tagName === 'SHREDDIT-POST') {
        const perm = node.getAttribute('permalink') || node.getAttribute('content-href');
        if (perm) {
          return {
            url: new URL(perm, location.origin).href,
            title: node.getAttribute('post-title') || document.title,
          };
        }
      }

      if (node.querySelectorAll) {
        const links = node.querySelectorAll('a[href]');
        for (const a of links) {
          const href = a.getAttribute('href') || '';
          if (permalinkPatterns.some((re) => re.test(href))) {
            try {
              const fullUrl = new URL(href, location.origin).href;
              return { url: fullUrl, title: document.title };
            } catch {}
          }
        }
      }

      const role = node.getAttribute ? node.getAttribute('role') : '';
      if (node.tagName === 'ARTICLE' || role === 'article') {
        break;
      }
      node = node.parentElement;
    }

    // 3. Fallback to current page URL (works on dedicated video/reel pages)
    if (isSingleVideoPermalinkPage() || (directSrc && /^https?:\/\//i.test(directSrc))) {
      return { url: location.href, title: document.title };
    }

    return { url: location.href, title: document.title };
  }

  function ensureHoverWidget() {
    if (hoverWidget && document.body.contains(hoverWidget)) {
      return hoverWidget;
    }

    const wrapper = document.createElement('div');
    wrapper.id = HOVER_WIDGET_ID;

    Object.assign(wrapper.style, {
      position: 'fixed',
      top: '-9999px',
      left: '-9999px',
      display: 'none',
      alignItems: 'center',
      height: '30px',
      borderRadius: '15px',
      overflow: 'hidden',
      boxShadow: '0 4px 14px rgba(0, 0, 0, 0.55)',
      zIndex: '2147483647',
      opacity: '0.96',
    });

    const select = createQualitySelect(true);

    const btn = document.createElement('button');
    btn.textContent = '⚡ Grab';
    btn.title = 'Download this video with GrabAll';

    Object.assign(btn.style, {
      backgroundColor: '#3b82f6',
      color: '#ffffff',
      border: 'none',
      borderRadius: '0 15px 15px 0',
      padding: '0 12px',
      fontSize: '12px',
      fontWeight: '700',
      fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif',
      cursor: 'pointer',
      height: '30px',
      display: 'inline-flex',
      alignItems: 'center',
      whiteSpace: 'nowrap',
    });

    btn.addEventListener('mouseenter', () => {
      if (!btn.disabled) btn.style.backgroundColor = '#2563eb';
    });
    btn.addEventListener('mouseleave', () => {
      if (!btn.disabled) btn.style.backgroundColor = '#3b82f6';
    });

    btn.addEventListener('click', (e) => {
      e.preventDefault();
      e.stopPropagation();
      if (!hoveredVideo) return;
      const target = resolveVideoTarget(hoveredVideo);
      sendDownloadRequest({
        btn,
        defaultLabel: '⚡ Grab',
        targetUrl: target.url,
        title: target.title,
        quality: select.value,
        playlist: false,
      });
    });

    wrapper.addEventListener('mouseenter', () => {
      if (hideHoverTimer) {
        clearTimeout(hideHoverTimer);
        hideHoverTimer = null;
      }
    });

    wrapper.addEventListener('mouseleave', () => {
      scheduleHideHover();
    });

    wrapper.appendChild(select);
    wrapper.appendChild(btn);
    document.body.appendChild(wrapper);
    hoverWidget = wrapper;
    return wrapper;
  }

  function positionHoverWidget(videoEl) {
    const rect = videoEl.getBoundingClientRect();
    if (rect.width < 160 || rect.height < 100) return false;
    if (rect.bottom < 40 || rect.top > window.innerHeight - 40) return false;

    const widget = ensureHoverWidget();
    widget.style.display = 'inline-flex';

    const top = Math.max(8, rect.top + 10);
    const widgetWidth = widget.offsetWidth || 155;
    const left = Math.max(8, Math.min(window.innerWidth - widgetWidth - 12, rect.right - widgetWidth - 10));

    widget.style.top = `${top}px`;
    widget.style.left = `${left}px`;
    return true;
  }

  function scheduleHideHover() {
    if (hideHoverTimer) clearTimeout(hideHoverTimer);
    hideHoverTimer = setTimeout(() => {
      if (hoverWidget) {
        hoverWidget.style.display = 'none';
      }
      hoveredVideo = null;
    }, 600);
  }

  let lastMouseCheck = 0;
  document.addEventListener(
    'mousemove',
    (e) => {
      // Skip hover badge on YouTube Watch/Shorts where the dedicated action bar button exists
      if (isYouTubeWatchOrShorts()) return;

      const now = Date.now();
      if (now - lastMouseCheck < 180) return;
      lastMouseCheck = now;

      if (hoverWidget && hoverWidget.contains(e.target)) {
        if (hideHoverTimer) {
          clearTimeout(hideHoverTimer);
          hideHoverTimer = null;
        }
        return;
      }

      const x = e.clientX;
      const y = e.clientY;
      const videos = document.querySelectorAll('video');
      let matched = null;

      for (const v of videos) {
        const r = v.getBoundingClientRect();
        if (r.width >= 160 && r.height >= 100 && x >= r.left && x <= r.right && y >= r.top && y <= r.bottom) {
          matched = v;
          break;
        }
      }

      if (matched) {
        if (hideHoverTimer) {
          clearTimeout(hideHoverTimer);
          hideHoverTimer = null;
        }
        hoveredVideo = matched;
        positionHoverWidget(matched);
      } else if (hoveredVideo) {
        scheduleHideHover();
      }
    },
    { passive: true }
  );

  window.addEventListener(
    'scroll',
    () => {
      if (hoveredVideo && hoverWidget && hoverWidget.style.display !== 'none') {
        if (!positionHoverWidget(hoveredVideo)) {
          hoverWidget.style.display = 'none';
          hoveredVideo = null;
        }
      }
    },
    { passive: true }
  );

  // ─── Floating Bottom-Right Pill on Dedicated Video Pages ───────────

  const VIDEO_SITES = [
    'tiktok.com',
    'twitter.com',
    'x.com',
    'instagram.com',
    'facebook.com',
    'fb.watch',
    'vimeo.com',
    'twitch.tv',
    'dailymotion.com',
    'reddit.com',
    'rumble.com',
  ];

  function matchesOtherVideoSite() {
    const host = location.hostname.toLowerCase();
    return VIDEO_SITES.some((s) => host.includes(s));
  }

  function injectFloatingWidget() {
    if (!matchesOtherVideoSite() || !isSingleVideoPermalinkPage()) {
      removeFloatingWidget();
      return;
    }
    if (!document.querySelector('video')) return;
    if (document.getElementById(FLOATING_WIDGET_ID)) return;

    const widget = buildActionPill({
      id: FLOATING_WIDGET_ID,
      label: '⚡ GrabAll',
      isFloating: true,
      playlist: false,
      titleFn: () => document.title,
    });
    document.body.appendChild(widget);
  }

  function removeFloatingWidget() {
    const existing = document.getElementById(FLOATING_WIDGET_ID);
    if (existing) existing.remove();
  }

  // ─── DOM Media Scanner ─────────────────────────────────────────────

  function scanMediaElements() {
    const mediaList = [];
    document.querySelectorAll('video, audio').forEach((el) => {
      const src = el.currentSrc || el.src;
      if (src && !src.startsWith('blob:') && !src.startsWith('data:')) {
        mediaList.push({
          url: src,
          type: el.tagName.toLowerCase() === 'video' ? 'Video' : 'Audio',
        });
      }
      el.querySelectorAll('source').forEach((s) => {
        if (s.src && !s.src.startsWith('blob:') && !s.src.startsWith('data:')) {
          mediaList.push({ url: s.src, type: s.type || 'Video Source' });
        }
      });
    });
    return mediaList;
  }

  // ─── Lifecycle & SPA Navigation Watcher ────────────────────────────

  function onPageChange() {
    const currentUrl = location.href;
    if (currentUrl === lastUrl) return;
    lastUrl = currentUrl;

    removeYouTubeWidget();
    removeFloatingWidget();

    if (isYouTubeWatchOrShorts() || isYouTubePlaylistPage()) {
      setTimeout(injectYouTubeWidget, 900);
    } else if (matchesOtherVideoSite()) {
      setTimeout(injectFloatingWidget, 1200);
    }
  }

  let debounceTimer = null;
  const observer = new MutationObserver(() => {
    if (debounceTimer) return;
    debounceTimer = setTimeout(() => {
      debounceTimer = null;
      onPageChange();

      if ((isYouTubeWatchOrShorts() || isYouTubePlaylistPage()) && !document.getElementById(YT_WIDGET_ID)) {
        injectYouTubeWidget();
      } else if (!isYouTubeWatchOrShorts() && !isYouTubePlaylistPage() && isYouTubeHost()) {
        removeYouTubeWidget();
      }

      if (matchesOtherVideoSite()) {
        injectFloatingWidget();
      }

      scanMediaElements().forEach((item) => {
        chrome.runtime.sendMessage({
          type: 'MEDIA_DETECTED_FROM_DOM',
          media: item,
        }).catch(() => {});
      });
    }, 500);
  });

  if (document.body) {
    observer.observe(document.body, { childList: true, subtree: true });
  } else {
    document.addEventListener('DOMContentLoaded', () => {
      observer.observe(document.body, { childList: true, subtree: true });
    });
  }

  window.addEventListener('yt-navigate-finish', () => {
    lastUrl = '';
    onPageChange();
  });

  onPageChange();
})();
