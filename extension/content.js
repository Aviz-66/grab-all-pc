// GrabAll Content Script - YouTube Auto-Capture + Quality/Audio Selector & Media Sniffer

(function () {
  'use strict';

  const YT_WIDGET_ID = 'graball-yt-widget';
  const FLOATING_WIDGET_ID = 'graball-floating-widget';
  let lastUrl = '';
  let preferredQuality = 'best';

  // Load saved quality preference
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

  function createQualitySelect() {
    const select = document.createElement('select');
    select.className = 'graball-quality-select';
    select.title = 'Select Video Resolution or Audio Only';

    const options = [
      { value: 'best', label: '🎬 Best' },
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

    Object.assign(select.style, {
      backgroundColor: '#1e293b',
      color: '#ffffff',
      border: 'none',
      borderRight: '1px solid rgba(255, 255, 255, 0.2)',
      borderRadius: '18px 0 0 18px',
      padding: '0 8px 0 12px',
      fontSize: '13px',
      fontWeight: '600',
      fontFamily: '"Roboto", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
      cursor: 'pointer',
      height: '36px',
      outline: 'none',
    });

    select.addEventListener('click', (e) => e.stopPropagation());
    select.addEventListener('change', (e) => {
      e.stopPropagation();
      preferredQuality = select.value;
      chrome.storage.local.set({ preferredQuality });
    });

    return select;
  }

  function triggerDownload(btn, defaultLabel, titleFn, qualitySelect) {
    const title = titleFn();
    const quality = qualitySelect ? qualitySelect.value : preferredQuality;

    btn.textContent = '⏳ Sending...';
    btn.disabled = true;

    chrome.runtime.sendMessage(
      {
        type: 'SEND_DOWNLOAD',
        payload: {
          url: location.href,
          pageUrl: location.href,
          referer: location.href,
          title: title,
          engine: 'ytdlp',
          quality: quality,
        },
      },
      (res) => {
        if (chrome.runtime.lastError || !res || !res.success) {
          btn.textContent = '❌ Error';
          btn.style.backgroundColor = '#ef4444';
          setTimeout(() => {
            btn.textContent = defaultLabel;
            btn.style.backgroundColor = '#3b82f6';
            btn.disabled = false;
          }, 2500);
        } else {
          btn.textContent = '✅ Queued!';
          btn.style.backgroundColor = '#10b981';
          setTimeout(() => {
            btn.textContent = defaultLabel;
            btn.style.backgroundColor = '#3b82f6';
            btn.disabled = false;
          }, 3000);
        }
      }
    );
  }

  // ─── YouTube Auto-Capture (Watch & Shorts) ─────────────────────────

  function isYouTubeHost() {
    return (
      location.hostname === 'www.youtube.com' ||
      location.hostname === 'youtube.com' ||
      location.hostname === 'm.youtube.com'
    );
  }

  function isYouTubeVideoPage() {
    return (
      isYouTubeHost() &&
      (location.pathname === '/watch' || location.pathname.startsWith('/shorts/'))
    );
  }

  function getYouTubeTitle() {
    const selectors = [
      'h1.ytd-watch-metadata yt-formatted-string',
      'ytd-reel-player-header-renderer h2 yt-formatted-string',
      '#title h1 yt-formatted-string',
      'h1.ytd-video-primary-info-renderer yt-formatted-string',
    ];
    for (const sel of selectors) {
      const el = document.querySelector(sel);
      if (el && el.textContent.trim()) {
        return el.textContent.trim();
      }
    }
    return document.title.replace(' - YouTube', '').trim();
  }

  function buildWidgetPill(id, label, isFloating, titleFn) {
    const wrapper = document.createElement('div');
    wrapper.id = id;

    Object.assign(wrapper.style, {
      display: 'inline-flex',
      alignItems: 'center',
      height: '36px',
      borderRadius: '18px',
      overflow: 'hidden',
      boxShadow: isFloating ? '0 4px 16px rgba(15, 23, 42, 0.55)' : 'none',
      marginLeft: isFloating ? '0' : '8px',
      verticalAlign: 'middle',
      zIndex: isFloating ? '2147483647' : '999',
    });

    if (isFloating) {
      Object.assign(wrapper.style, {
        position: 'fixed',
        bottom: '20px',
        right: '20px',
      });
    }

    const select = createQualitySelect();

    const btn = document.createElement('button');
    btn.textContent = label;
    btn.title = 'Download with GrabAll';

    Object.assign(btn.style, {
      backgroundColor: '#3b82f6',
      color: '#ffffff',
      border: 'none',
      borderRadius: '0 18px 18px 0',
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
      triggerDownload(btn, label, titleFn, select);
    });

    wrapper.appendChild(select);
    wrapper.appendChild(btn);
    return wrapper;
  }

  function injectYouTubeWidget() {
    if (!isYouTubeVideoPage()) {
      removeYouTubeWidget();
      return;
    }

    if (document.getElementById(YT_WIDGET_ID)) {
      return;
    }

    // For Shorts, use floating widget so it's always accessible
    if (location.pathname.startsWith('/shorts/')) {
      const floating = buildWidgetPill(YT_WIDGET_ID, '⚡ GrabAll', true, getYouTubeTitle);
      document.body.appendChild(floating);
      return;
    }

    const actionBar =
      document.querySelector('ytd-watch-metadata #actions #top-level-buttons-computed') ||
      document.querySelector('#actions #top-level-buttons-computed') ||
      document.querySelector('#menu #top-level-buttons-computed') ||
      document.querySelector('ytd-watch-metadata #actions');

    if (actionBar) {
      const widget = buildWidgetPill(YT_WIDGET_ID, '⚡ GrabAll', false, getYouTubeTitle);
      actionBar.appendChild(widget);
    } else if (document.querySelector('video')) {
      // Fallback floating button if YouTube changes action bar DOM
      const floating = buildWidgetPill(YT_WIDGET_ID, '⚡ GrabAll', true, getYouTubeTitle);
      document.body.appendChild(floating);
    }
  }

  function removeYouTubeWidget() {
    const existing = document.getElementById(YT_WIDGET_ID);
    if (existing) existing.remove();
  }

  // ─── Floating Widget for Other Video Sites ─────────────────────────

  const VIDEO_SITES = [
    { match: 'tiktok.com', label: '⚡ GrabAll' },
    { match: 'twitter.com', label: '⚡ GrabAll' },
    { match: 'x.com', label: '⚡ GrabAll' },
    { match: 'instagram.com', label: '⚡ GrabAll' },
    { match: 'facebook.com', label: '⚡ GrabAll' },
    { match: 'vimeo.com', label: '⚡ GrabAll' },
    { match: 'twitch.tv', label: '⚡ GrabAll' },
    { match: 'dailymotion.com', label: '⚡ GrabAll' },
    { match: 'reddit.com', label: '⚡ GrabAll' },
    { match: 'rumble.com', label: '⚡ GrabAll' },
  ];

  function matchesOtherVideoSite() {
    const host = location.hostname;
    return VIDEO_SITES.find((s) => host.includes(s.match));
  }

  function injectFloatingWidget() {
    const site = matchesOtherVideoSite();
    if (!site) return;

    if (!document.querySelector('video')) return;
    if (document.getElementById(FLOATING_WIDGET_ID)) return;

    const widget = buildWidgetPill(FLOATING_WIDGET_ID, site.label, true, () => document.title);
    document.body.appendChild(widget);
  }

  function removeFloatingWidget() {
    const existing = document.getElementById(FLOATING_WIDGET_ID);
    if (existing) existing.remove();
  }

  // ─── DOM Media Scanner ─────────────────────────────────────────────

  function scanMediaElements() {
    const mediaList = [];
    const videos = document.querySelectorAll('video, audio');

    videos.forEach((el) => {
      const src = el.currentSrc || el.src;
      if (src && !src.startsWith('blob:') && !src.startsWith('data:')) {
        mediaList.push({
          url: src,
          type: el.tagName.toLowerCase() === 'video' ? 'Video' : 'Audio',
        });
      }

      el.querySelectorAll('source').forEach((s) => {
        if (s.src && !s.src.startsWith('blob:') && !s.src.startsWith('data:')) {
          mediaList.push({
            url: s.src,
            type: s.type || 'Video Source',
          });
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

    if (isYouTubeVideoPage()) {
      setTimeout(injectYouTubeWidget, 1000);
    } else if (matchesOtherVideoSite()) {
      setTimeout(injectFloatingWidget, 1500);
    }
  }

  let debounceTimer = null;
  const observer = new MutationObserver(() => {
    if (debounceTimer) return;
    debounceTimer = setTimeout(() => {
      debounceTimer = null;
      onPageChange();

      if (isYouTubeVideoPage() && !document.getElementById(YT_WIDGET_ID)) {
        injectYouTubeWidget();
      } else if (!isYouTubeVideoPage() && isYouTubeHost()) {
        removeYouTubeWidget();
      }

      if (matchesOtherVideoSite() && !document.getElementById(FLOATING_WIDGET_ID)) {
        injectFloatingWidget();
      }

      const found = scanMediaElements();
      found.forEach((item) => {
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

  // Also listen to YouTube's native SPA navigation event
  window.addEventListener('yt-navigate-finish', () => {
    lastUrl = '';
    onPageChange();
  });

  onPageChange();
})();
