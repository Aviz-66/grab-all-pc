// GrabAll Content Script v2.1.0
// - YouTube Watch, Shorts & Playlist auto-capture with Resolution + Audio-Only selector
// - Smart Playlist Popover Card: Quick Count (5/10/25/50/All), Custom Start # & Video Count,
//   "Start from Current Video", and Interactive Video Checklist (instant DOM + yt-dlp preview)
// - IDM-style per-video hover overlay for scrolling feeds & video players (TrustedHTML safe)

(function () {
  'use strict';

  const YT_WIDGET_ID = 'graball-yt-widget';
  const YT_PANEL_BTN_ID = 'graball-yt-panel-btn';
  const PLAYLIST_POPOVER_ID = 'graball-playlist-popover';
  const FLOATING_WIDGET_ID = 'graball-floating-widget';
  const HOVER_WIDGET_ID = 'graball-hover-widget';

  const QUALITY_OPTIONS = [
    { value: 'best', short: '🎬 Best', full: '🎬 Best Quality (Auto MP4)', badge: 'Best' },
    { value: '2160', short: '📺 4K', full: '📺 4K Ultra HD (2160p)', badge: '4K' },
    { value: '1080', short: '📺 1080p', full: '📺 1080p Full HD', badge: '1080p' },
    { value: '720', short: '📺 720p', full: '📺 720p HD', badge: '720p' },
    { value: '480', short: '📺 480p', full: '📺 480p Standard', badge: '480p' },
    { value: '360', short: '📺 360p', full: '📺 360p Data Saver', badge: '360p' },
    { value: 'audio-mp3', short: '🎵 MP3', full: '🎵 Audio Only — MP3', badge: 'MP3' },
    { value: 'audio-m4a', short: '🎵 M4A', full: '🎵 Audio Only — M4A', badge: 'M4A' },
    { value: 'audio-opus', short: '🎵 Opus', full: '🎵 Audio Only — Opus', badge: 'Opus' },
  ];

  let lastUrl = '';
  let preferredQuality = 'best';

  // TrustedHTML-safe DOM builder (never uses innerHTML, 100% YouTube CSP safe)
  function dom(tag, styles = {}, attrs = {}, children = []) {
    const node = document.createElement(tag);
    Object.assign(node.style, styles);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === 'text') {
        node.textContent = v;
      } else if (k === 'className') {
        node.className = v;
      } else if (k.startsWith('on') && typeof v === 'function') {
        node.addEventListener(k.slice(2).toLowerCase(), v);
      } else if (v !== undefined && v !== null) {
        node.setAttribute(k, v);
      }
    }
    for (const child of children) {
      if (child) node.appendChild(child);
    }
    return node;
  }

  function qualityBadgeLabel(val) {
    const found = QUALITY_OPTIONS.find((o) => o.value === val);
    return found ? found.badge : 'Best';
  }

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

  function createQualitySelect({ compact = false, fullLabels = false, standalone = false } = {}) {
    const select = document.createElement('select');
    select.className = 'graball-quality-select';
    select.title = 'Select Video Resolution or Audio Only';

    QUALITY_OPTIONS.forEach((opt) => {
      const el = document.createElement('option');
      el.value = opt.value;
      el.textContent = fullLabels ? opt.full : opt.short;
      el.style.backgroundColor = '#0f172a';
      el.style.color = '#f8fafc';
      select.appendChild(el);
    });

    select.value = preferredQuality;

    if (standalone) {
      Object.assign(select.style, {
        width: '100%',
        backgroundColor: '#0f172a',
        color: '#f8fafc',
        border: '1px solid #334155',
        borderRadius: '8px',
        padding: '7px 10px',
        fontSize: '12.5px',
        fontWeight: '600',
        fontFamily: '"Roboto", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
        cursor: 'pointer',
        outline: 'none',
      });
    } else {
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
    }

    select.addEventListener('click', (e) => e.stopPropagation());
    select.addEventListener('mousedown', (e) => e.stopPropagation());
    select.addEventListener('change', (e) => {
      e.stopPropagation();
      preferredQuality = select.value;
      chrome.storage.local.set({ preferredQuality });
    });

    return select;
  }

  function stripPlaylistFromWatchUrl(rawUrl) {
    try {
      const u = new URL(rawUrl);
      if (u.pathname === '/watch' && u.searchParams.get('v')) {
        const clean = new URL('/watch', u.origin);
        clean.searchParams.set('v', u.searchParams.get('v'));
        return clean.href;
      }
    } catch {}
    return rawUrl;
  }

  function sendDownloadRequest({
    btn,
    defaultLabel,
    defaultBg = '#3b82f6',
    targetUrl,
    title,
    quality,
    playlist = false,
    playlistStart = 0,
    playlistEnd = 0,
    playlistCount = 0,
    playlistItems = null,
    onDone = null,
  }) {
    if (!chrome?.runtime?.id) {
      btn.textContent = '↻ Refresh Tab (F5)';
      btn.title = 'Extension was updated — press F5 once to reconnect this tab';
      btn.style.backgroundColor = '#f59e0b';
      return;
    }

    const effectiveUrl = playlist ? targetUrl : stripPlaylistFromWatchUrl(targetUrl);
    btn.textContent = playlist ? '⏳ Queuing Playlist...' : '⏳ Sending...';
    btn.disabled = true;

    try {
      chrome.runtime.sendMessage(
        {
          type: 'SEND_DOWNLOAD',
          payload: {
            url: effectiveUrl,
            pageUrl: location.href,
            referer: location.href,
            title: title || document.title,
            engine: 'ytdlp',
            quality: quality || preferredQuality,
            playlist: Boolean(playlist),
            playlistStart: playlist ? Number(playlistStart) || 0 : 0,
            playlistEnd: playlist ? Number(playlistEnd) || 0 : 0,
            playlistCount: playlist ? Number(playlistCount) || 0 : 0,
            playlistItems: playlist && Array.isArray(playlistItems) ? playlistItems : undefined,
          },
        },
        (res) => {
          const runtimeErr = chrome.runtime.lastError?.message || '';
          if (runtimeErr || !res || !res.success) {
            const errMsg = res?.error || runtimeErr || 'Error';
            if (errMsg.toLowerCase().includes('invalidated')) {
              btn.textContent = '↻ Refresh Tab (F5)';
            } else if (errMsg.toLowerCase().includes('failed to fetch')) {
              btn.textContent = '❌ Start graball.exe';
            } else {
              btn.textContent = '❌ Error';
            }
            btn.title = errMsg;
            btn.style.backgroundColor = '#ef4444';
            setTimeout(() => {
              btn.textContent = defaultLabel;
              btn.style.backgroundColor = defaultBg;
              btn.disabled = false;
            }, 3500);
          } else {
            const count = res.data?.count;
            btn.textContent = playlist && count ? `✅ ${count} Videos Queued!` : '✅ Queued!';
            btn.style.backgroundColor = '#10b981';
            if (onDone) onDone(res.data);
            setTimeout(() => {
              btn.textContent = defaultLabel;
              btn.style.backgroundColor = defaultBg;
              btn.disabled = false;
            }, 2600);
          }
        }
      );
    } catch (e) {
      btn.textContent = '↻ Refresh Tab (F5)';
      btn.style.backgroundColor = '#f59e0b';
      btn.disabled = false;
    }
  }

  // ─── YouTube Helpers (Watch, Shorts, Playlists, Mixes) ─────────────

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

  function getPlaylistId() {
    try {
      const listId = new URLSearchParams(location.search).get('list');
      if (listId) return listId;
      // Also detect if YouTube's playlist panel is open in DOM
      const panelLink = document.querySelector(
        'ytd-playlist-panel-renderer:not([hidden]) a[href*="list="]'
      );
      if (panelLink) {
        const u = new URL(panelLink.href, location.origin);
        return u.searchParams.get('list');
      }
    } catch {}
    return null;
  }

  function getCurrentPlaylistIndex() {
    try {
      const idxParam = parseInt(new URLSearchParams(location.search).get('index') || '', 10);
      if (idxParam >= 1) return idxParam;

      const items = document.querySelectorAll('ytd-playlist-panel-video-renderer');
      for (let i = 0; i < items.length; i++) {
        if (items[i].hasAttribute('selected')) {
          return i + 1;
        }
      }
    } catch {}
    return 1;
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

  function getDetectedPlaylistTitle() {
    const selectors = [
      'ytd-playlist-panel-renderer:not([hidden]) .header yt-formatted-string.title',
      'ytd-playlist-panel-renderer:not([hidden]) h3.ytd-playlist-panel-renderer a',
      'ytd-playlist-panel-renderer:not([hidden]) #header-description h3',
      'yt-page-header-view-model h1 span',
      'ytd-playlist-header-renderer #text',
    ];
    for (const sel of selectors) {
      const el = document.querySelector(sel);
      if (el && el.textContent.trim()) {
        return el.textContent.trim();
      }
    }
    return getYouTubeTitle() || 'Playlist';
  }

  // Instant 0ms scrape of playlist items already rendered in YouTube's DOM!
  function scrapeVisibleDomPlaylistEntries() {
    const entries = [];
    const seen = new Set();

    // 1. Watch page side panel items
    const panelItems = document.querySelectorAll(
      'ytd-playlist-panel-renderer:not([hidden]) ytd-playlist-panel-video-renderer'
    );
    panelItems.forEach((row, i) => {
      const a = row.querySelector('a#wc-endpoint[href*="watch?v="], a[href*="watch?v="]');
      const titleEl = row.querySelector('#video-title');
      if (!a) return;
      try {
        const u = new URL(a.getAttribute('href'), location.origin);
        const vid = u.searchParams.get('v');
        if (!vid || seen.has(vid)) return;
        seen.add(vid);
        const idxEl = row.querySelector('#index');
        const parsedIdx = idxEl ? parseInt(idxEl.textContent.trim(), 10) : NaN;
        entries.push({
          index: Number.isFinite(parsedIdx) && parsedIdx > 0 ? parsedIdx : entries.length + 1 || i + 1,
          url: `https://www.youtube.com/watch?v=${encodeURIComponent(vid)}`,
          title: (titleEl?.textContent || titleEl?.getAttribute('title') || `Video ${entries.length + 1}`).trim(),
        });
      } catch {}
    });

    if (entries.length > 0) return entries;

    // 2. Dedicated /playlist page items
    const pageItems = document.querySelectorAll('ytd-playlist-video-renderer');
    pageItems.forEach((row) => {
      const a = row.querySelector('a#video-title[href*="watch?v="], a[href*="watch?v="]');
      if (!a) return;
      try {
        const u = new URL(a.getAttribute('href'), location.origin);
        const vid = u.searchParams.get('v');
        if (!vid || seen.has(vid)) return;
        seen.add(vid);
        const idxEl = row.querySelector('#index');
        const parsedIdx = idxEl ? parseInt(idxEl.textContent.trim(), 10) : NaN;
        entries.push({
          index: Number.isFinite(parsedIdx) && parsedIdx > 0 ? parsedIdx : entries.length + 1,
          url: `https://www.youtube.com/watch?v=${encodeURIComponent(vid)}`,
          title: (a.textContent || a.getAttribute('title') || `Video ${entries.length + 1}`).trim(),
        });
      } catch {}
    });

    return entries;
  }

  // ─── Smart Playlist Popover Card ───────────────────────────────────

  function closePlaylistPopover() {
    const existing = document.getElementById(PLAYLIST_POPOVER_ID);
    if (existing) existing.remove();
  }

  function openPlaylistPopover(anchorEl) {
    const existing = document.getElementById(PLAYLIST_POPOVER_ID);
    if (existing) {
      existing.remove();
      return;
    }

    const listId = getPlaylistId();
    const isMix = Boolean(listId && listId.startsWith('RD'));
    const currentVideoIdx = isYouTubeWatchOrShorts() ? getCurrentPlaylistIndex() : 1;
    const plTitle = getDetectedPlaylistTitle();

    let entries = scrapeVisibleDomPlaylistEntries();
    let checkedSet = new Set();
    let manualChecklistMode = false;
    let activePreset = isMix ? 10 : 10; // Sensible fast default: 10 videos (user can click 5/10/25/50/All or type any number)

    const card = dom(
      'div',
      {
        position: 'fixed',
        width: '375px',
        maxWidth: 'calc(100vw - 20px)',
        backgroundColor: '#0f172a',
        color: '#f8fafc',
        border: '1px solid #334155',
        borderRadius: '14px',
        boxShadow: '0 20px 45px rgba(0, 0, 0, 0.75)',
        padding: '14px',
        zIndex: '2147483647',
        fontFamily: '"Roboto", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
        fontSize: '12.5px',
        display: 'flex',
        flexDirection: 'column',
        gap: '11px',
      },
      { id: PLAYLIST_POPOVER_ID }
    );

    card.addEventListener('click', (e) => e.stopPropagation());
    card.addEventListener('mousedown', (e) => e.stopPropagation());

    // 1. Header
    const titleSpan = dom(
      'div',
      {
        fontWeight: '700',
        fontSize: '13.5px',
        color: '#f8fafc',
        whiteSpace: 'nowrap',
        overflow: 'hidden',
        textOverflow: 'ellipsis',
      },
      { text: `📑 ${plTitle}`, title: plTitle }
    );

    const subHeader = dom(
      'div',
      { fontSize: '11px', color: '#94a3b8', marginTop: '2px' },
      {
        text:
          entries.length > 0
            ? `${entries.length} videos visible on page •Saves to subfolder`
            : 'Choose quality & how many videos to grab',
      }
    );

    const closeBtn = dom(
      'button',
      {
        background: 'transparent',
        border: 'none',
        color: '#94a3b8',
        fontSize: '16px',
        cursor: 'pointer',
        padding: '2px 6px',
        borderRadius: '6px',
        lineHeight: '1',
      },
      {
        text: '✕',
        title: 'Close',
        onClick: (e) => {
          e.preventDefault();
          closePlaylistPopover();
        },
      }
    );

    const headerRow = dom(
      'div',
      {
        display: 'flex',
        justifyContent: 'space-between',
        alignItems: 'flex-start',
        gap: '8px',
        paddingBottom: '8px',
        borderBottom: '1px solid #1e293b',
      },
      {},
      [dom('div', { minWidth: '0', flex: '1' }, {}, [titleSpan, subHeader]), closeBtn]
    );

    // 2. Resolution / Audio-Only Selector
    const qualLabel = dom(
      'div',
      { fontSize: '11px', fontWeight: '600', color: '#94a3b8', marginBottom: '4px' },
      { text: 'FORMAT — VIDEO RESOLUTION OR AUDIO ONLY' }
    );
    const qualSelect = createQualitySelect({ fullLabels: true, standalone: true });

    // 3. Start Position + Custom Count / Presets
    const startInput = dom(
      'input',
      {
        width: '64px',
        backgroundColor: '#1e293b',
        color: '#f8fafc',
        border: '1px solid #475569',
        borderRadius: '6px',
        padding: '5px 7px',
        fontSize: '12.5px',
        fontWeight: '700',
        outline: 'none',
      },
      { type: 'number', min: '1', value: '1' }
    );

    const countInput = dom(
      'input',
      {
        width: '74px',
        backgroundColor: '#1e293b',
        color: '#f8fafc',
        border: '1px solid #475569',
        borderRadius: '6px',
        padding: '5px 7px',
        fontSize: '12.5px',
        fontWeight: '700',
        outline: 'none',
      },
      { type: 'number', min: '0', value: String(activePreset), title: '0 = Download all videos from Start #' }
    );

    // Quick Start helper if watching video #k > 1 inside playlist
    let startFromBar = null;
    if (isYouTubeWatchOrShorts() && currentVideoIdx > 1) {
      const btnFrom1 = dom(
        'button',
        {
          flex: '1',
          padding: '5px 8px',
          borderRadius: '6px',
          border: '1px solid #3b82f6',
          backgroundColor: 'rgba(59, 130, 246, 0.2)',
          color: '#93c5fd',
          fontSize: '11.5px',
          fontWeight: '600',
          cursor: 'pointer',
        },
        { text: 'Start at #1' }
      );
      const btnFromCur = dom(
        'button',
        {
          flex: '1',
          padding: '5px 8px',
          borderRadius: '6px',
          border: '1px solid #334155',
          backgroundColor: '#1e293b',
          color: '#cbd5e1',
          fontSize: '11.5px',
          fontWeight: '600',
          cursor: 'pointer',
        },
        { text: `From Current (#${currentVideoIdx})` }
      );

      const setStartHighlight = (fromCurrent) => {
        btnFrom1.style.borderColor = fromCurrent ? '#334155' : '#3b82f6';
        btnFrom1.style.backgroundColor = fromCurrent ? '#1e293b' : 'rgba(59, 130, 246, 0.2)';
        btnFrom1.style.color = fromCurrent ? '#cbd5e1' : '#93c5fd';

        btnFromCur.style.borderColor = fromCurrent ? '#3b82f6' : '#334155';
        btnFromCur.style.backgroundColor = fromCurrent ? 'rgba(59, 130, 246, 0.2)' : '#1e293b';
        btnFromCur.style.color = fromCurrent ? '#93c5fd' : '#cbd5e1';
      };

      btnFrom1.addEventListener('click', () => {
        startInput.value = '1';
        manualChecklistMode = false;
        setStartHighlight(false);
        syncRangeToChecklistAndButton();
      });
      btnFromCur.addEventListener('click', () => {
        startInput.value = String(currentVideoIdx);
        manualChecklistMode = false;
        setStartHighlight(true);
        syncRangeToChecklistAndButton();
      });

      startFromBar = dom('div', { display: 'flex', gap: '6px' }, {}, [btnFrom1, btnFromCur]);
    }

    // Quick Count Preset Chips: 5 | 10 | 25 | 50 | All
    const presetDefs = [
      { label: '5 vids', val: 5 },
      { label: '10 vids', val: 10 },
      { label: '25 vids', val: 25 },
      { label: '50 vids', val: 50 },
      { label: isMix ? 'Mix 25' : 'All', val: isMix ? 25 : 0 },
    ];
    const presetChipEls = [];

    function refreshPresetChips() {
      const currentVal = parseInt(countInput.value || '0', 10) || 0;
      presetChipEls.forEach(({ btn, val }) => {
        const isSel = !manualChecklistMode && currentVal === val;
        btn.style.backgroundColor = isSel ? '#2563eb' : '#1e293b';
        btn.style.borderColor = isSel ? '#3b82f6' : '#334155';
        btn.style.color = isSel ? '#ffffff' : '#cbd5e1';
      });
    }

    const chipsRow = dom('div', { display: 'flex', gap: '5px', flexWrap: 'wrap' });
    presetDefs.forEach(({ label, val }) => {
      const chip = dom(
        'button',
        {
          flex: '1',
          minWidth: '54px',
          padding: '5px 6px',
          borderRadius: '6px',
          border: '1px solid #334155',
          backgroundColor: '#1e293b',
          color: '#cbd5e1',
          fontSize: '11.5px',
          fontWeight: '600',
          cursor: 'pointer',
        },
        {
          text: label,
          onClick: () => {
            countInput.value = String(val);
            manualChecklistMode = false;
            syncRangeToChecklistAndButton();
          },
        }
      );
      presetChipEls.push({ btn: chip, val });
      chipsRow.appendChild(chip);
    });

    const customInputsRow = dom(
      'div',
      {
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        gap: '8px',
        backgroundColor: '#1e293b',
        padding: '7px 10px',
        borderRadius: '8px',
        border: '1px solid #334155',
      },
      {},
      [
        dom('div', { display: 'flex', alignItems: 'center', gap: '6px' }, {}, [
          dom('span', { color: '#cbd5e1', fontSize: '12px' }, { text: 'Start #:' }),
          startInput,
        ]),
        dom('div', { display: 'flex', alignItems: 'center', gap: '6px' }, {}, [
          dom('span', { color: '#cbd5e1', fontSize: '12px' }, { text: 'Video Count:' }),
          countInput,
        ]),
      ]
    );

    const countSection = dom('div', { display: 'flex', flexDirection: 'column', gap: '6px' }, {}, [
      dom(
        'div',
        { fontSize: '11px', fontWeight: '600', color: '#94a3b8' },
        { text: 'HOW MANY VIDEOS TO DOWNLOAD (0 = ALL)' }
      ),
      startFromBar,
      chipsRow,
      customInputsRow,
    ]);

    // 4. Interactive Video Checklist Section
    const checklistContainer = dom('div', {
      maxHeight: '165px',
      overflowY: 'auto',
      display: 'flex',
      flexDirection: 'column',
      gap: '3px',
      backgroundColor: '#090d16',
      border: '1px solid #1e293b',
      borderRadius: '8px',
      padding: '6px',
    });

    const fetchBtn = dom(
      'button',
      {
        backgroundColor: '#1e293b',
        color: '#93c5fd',
        border: '1px solid #334155',
        borderRadius: '6px',
        padding: '3px 8px',
        fontSize: '11px',
        fontWeight: '600',
        cursor: 'pointer',
      },
      {
        text: entries.length > 0 ? '🔄 Scan Full List' : '📋 Load Video List',
        title: 'Fetch complete playlist entries with yt-dlp so you can tick exact videos',
      }
    );

    const selectAllBtn = dom(
      'button',
      {
        backgroundColor: 'transparent',
        color: '#94a3b8',
        border: 'none',
        fontSize: '11px',
        cursor: 'pointer',
        textDecoration: 'underline',
      },
      { text: 'All / None' }
    );

    const checklistHeader = dom(
      'div',
      { display: 'flex', justifyContent: 'space-between', alignItems: 'center' },
      {},
      [
        dom(
          'span',
          { fontSize: '11px', fontWeight: '600', color: '#94a3b8' },
          { text: 'SELECT SPECIFIC VIDEOS (OPTIONAL)' }
        ),
        dom('div', { display: 'flex', alignItems: 'center', gap: '8px' }, {}, [selectAllBtn, fetchBtn]),
      ]
    );

    // Primary Download Playlist Button & Single Video Button
    const primaryPlBtn = dom(
      'button',
      {
        flex: '1',
        backgroundColor: '#0284c7',
        color: '#ffffff',
        border: 'none',
        borderRadius: '8px',
        padding: '9px 12px',
        fontSize: '13px',
        fontWeight: '700',
        cursor: 'pointer',
      },
      { text: '📥 Queue Playlist' }
    );

    let singleVideoBtn = null;
    if (isYouTubeWatchOrShorts()) {
      singleVideoBtn = dom(
        'button',
        {
          backgroundColor: '#1e293b',
          color: '#e2e8f0',
          border: '1px solid #475569',
          borderRadius: '8px',
          padding: '9px 10px',
          fontSize: '12px',
          fontWeight: '600',
          cursor: 'pointer',
          whiteSpace: 'nowrap',
        },
        {
          text: '⚡ 1 Video Only',
          title: 'Ignore playlist and download ONLY the currently playing video',
          onClick: () => {
            sendDownloadRequest({
              btn: singleVideoBtn,
              defaultLabel: '⚡ 1 Video Only',
              defaultBg: '#1e293b',
              targetUrl: location.href,
              title: getYouTubeTitle(),
              quality: qualSelect.value,
              playlist: false,
            });
          },
        }
      );
    }

    function updatePrimaryButtonLabel() {
      const badge = qualityBadgeLabel(qualSelect.value);
      if (manualChecklistMode && entries.length > 0) {
        const n = checkedSet.size;
        primaryPlBtn.textContent = n > 0 ? `📥 Queue ${n} Selected (${badge})` : 'Select at least 1 video';
        primaryPlBtn.disabled = n === 0;
        primaryPlBtn.style.opacity = n === 0 ? '0.55' : '1';
        return;
      }
      primaryPlBtn.disabled = false;
      primaryPlBtn.style.opacity = '1';
      const st = Math.max(1, parseInt(startInput.value || '1', 10) || 1);
      const cnt = Math.max(0, parseInt(countInput.value || '0', 10) || 0);
      if (cnt === 0) {
        primaryPlBtn.textContent = st > 1 ? `📥 Queue All from #${st} (${badge})` : `📥 Queue Entire Playlist (${badge})`;
      } else {
        const endNum = st + cnt - 1;
        primaryPlBtn.textContent = `📥 Queue ${cnt} Videos #${st}–#${endNum} (${badge})`;
      }
    }

    function renderChecklistRows() {
      while (checklistContainer.firstChild) {
        checklistContainer.removeChild(checklistContainer.firstChild);
      }
      if (entries.length === 0) {
        checklistContainer.appendChild(
          dom(
            'div',
            { color: '#64748b', fontSize: '11.5px', textAlign: 'center', padding: '10px 6px' },
            { text: 'Click "Load Video List" to cherry-pick individual videos, or click Queue below!' }
          )
        );
        return;
      }

      entries.forEach((entry, idx) => {
        const num = entry.index || idx + 1;
        const cb = dom(
          'input',
          { cursor: 'pointer', accentColor: '#3b82f6', flexShrink: '0' },
          { type: 'checkbox' }
        );
        cb.checked = checkedSet.has(entry.url);

        const row = dom(
          'label',
          {
            display: 'flex',
            alignItems: 'center',
            gap: '7px',
            padding: '4px 6px',
            borderRadius: '5px',
            cursor: 'pointer',
            backgroundColor: cb.checked ? 'rgba(59, 130, 246, 0.12)' : 'transparent',
          },
          {},
          [
            cb,
            dom(
              'span',
              { color: '#64748b', fontSize: '11px', fontWeight: '700', minWidth: '24px' },
              { text: `#${num}` }
            ),
            dom(
              'span',
              {
                color: '#e2e8f0',
                fontSize: '11.5px',
                whiteSpace: 'nowrap',
                overflow: 'hidden',
                textOverflow: 'ellipsis',
                flex: '1',
              },
              { text: entry.title, title: entry.title }
            ),
          ]
        );

        cb.addEventListener('change', () => {
          manualChecklistMode = true;
          if (cb.checked) {
            checkedSet.add(entry.url);
          } else {
            checkedSet.delete(entry.url);
          }
          row.style.backgroundColor = cb.checked ? 'rgba(59, 130, 246, 0.12)' : 'transparent';
          refreshPresetChips();
          updatePrimaryButtonLabel();
        });

        checklistContainer.appendChild(row);
      });
    }

    function syncRangeToChecklistAndButton() {
      const st = Math.max(1, parseInt(startInput.value || '1', 10) || 1);
      const cnt = Math.max(0, parseInt(countInput.value || '0', 10) || 0);
      const endNum = cnt > 0 ? st + cnt - 1 : Infinity;

      checkedSet = new Set();
      entries.forEach((entry, i) => {
        const num = entry.index || i + 1;
        if (num >= st && num <= endNum) {
          checkedSet.add(entry.url);
        }
      });

      refreshPresetChips();
      renderChecklistRows();
      updatePrimaryButtonLabel();
    }

    startInput.addEventListener('input', () => {
      manualChecklistMode = false;
      syncRangeToChecklistAndButton();
    });
    countInput.addEventListener('input', () => {
      manualChecklistMode = false;
      syncRangeToChecklistAndButton();
    });
    qualSelect.addEventListener('change', updatePrimaryButtonLabel);

    selectAllBtn.addEventListener('click', (e) => {
      e.preventDefault();
      if (entries.length === 0) return;
      manualChecklistMode = true;
      if (checkedSet.size === entries.length) {
        checkedSet.clear();
      } else {
        entries.forEach((item) => checkedSet.add(item.url));
      }
      refreshPresetChips();
      renderChecklistRows();
      updatePrimaryButtonLabel();
    });

    fetchBtn.addEventListener('click', (e) => {
      e.preventDefault();
      fetchBtn.textContent = '⏳ Scanning...';
      fetchBtn.disabled = true;
      chrome.runtime.sendMessage(
        {
          type: 'PREVIEW_PLAYLIST',
          payload: {
            url: location.href,
            pageUrl: location.href,
            title: plTitle,
            playlistStart: 1,
            playlistCount: isMix ? 50 : 250,
          },
        },
        (res) => {
          fetchBtn.disabled = false;
          if (chrome.runtime.lastError || !res || !res.success) {
            fetchBtn.textContent = '❌ Scan Failed';
            fetchBtn.title = res?.error || chrome.runtime.lastError?.message || 'Error';
            setTimeout(() => {
              fetchBtn.textContent = '🔄 Retry Scan';
            }, 2500);
            return;
          }
          entries = Array.isArray(res.data?.entries) ? res.data.entries : [];
          if (res.data?.playlistTitle) {
            titleSpan.textContent = `📑 ${res.data.playlistTitle}`;
          }
          subHeader.textContent = `${entries.length} videos loaded via GrabAll • Tick or pick count`;
          fetchBtn.textContent = `✓ ${entries.length} Loaded`;
          manualChecklistMode = false;
          syncRangeToChecklistAndButton();
        }
      );
    });

    primaryPlBtn.addEventListener('click', (e) => {
      e.preventDefault();
      const st = Math.max(1, parseInt(startInput.value || '1', 10) || 1);
      const cnt = Math.max(0, parseInt(countInput.value || '0', 10) || 0);

      // Fast path: if user cherry-picked items OR all requested range items are already loaded in entries!
      let directItems = null;
      if (manualChecklistMode && entries.length > 0) {
        directItems = entries.filter((item) => checkedSet.has(item.url));
      } else if (
        entries.length > 0 &&
        cnt > 0 &&
        entries.filter((it, idx) => {
          const n = it.index || idx + 1;
          return n >= st && n <= st + cnt - 1;
        }).length === cnt
      ) {
        directItems = entries.filter((it, idx) => {
          const n = it.index || idx + 1;
          return n >= st && n <= st + cnt - 1;
        });
      }

      const defaultTxt = primaryPlBtn.textContent;
      sendDownloadRequest({
        btn: primaryPlBtn,
        defaultLabel: defaultTxt,
        defaultBg: '#0284c7',
        targetUrl: location.href,
        title: plTitle,
        quality: qualSelect.value,
        playlist: true,
        playlistStart: st,
        playlistCount: cnt,
        playlistItems: directItems,
        onDone: () => {
          setTimeout(closePlaylistPopover, 900);
        },
      });
    });

    const footerActions = dom(
      'div',
      { display: 'flex', gap: '8px', paddingTop: '4px' },
      {},
      [singleVideoBtn, primaryPlBtn]
    );

    card.appendChild(headerRow);
    card.appendChild(dom('div', {}, {}, [qualLabel, qualSelect]));
    card.appendChild(countSection);
    card.appendChild(dom('div', { display: 'flex', flexDirection: 'column', gap: '5px' }, {}, [checklistHeader, checklistContainer]));
    card.appendChild(footerActions);

    document.body.appendChild(card);
    syncRangeToChecklistAndButton();

    // Position anchored near clicked button, clamped cleanly inside viewport
    const rect = anchorEl ? anchorEl.getBoundingClientRect() : { bottom: 90, left: 40, top: 60, right: 200 };
    const cardRect = card.getBoundingClientRect();
    let top = rect.bottom + 8;
    if (top + cardRect.height > window.innerHeight - 12) {
      top = Math.max(12, rect.top - cardRect.height - 8);
    }
    let left = rect.left;
    if (left + cardRect.width > window.innerWidth - 12) {
      left = Math.max(12, window.innerWidth - cardRect.width - 14);
    }
    card.style.top = `${Math.max(10, top)}px`;
    card.style.left = `${Math.max(10, left)}px`;
  }

  document.addEventListener('click', (e) => {
    const pop = document.getElementById(PLAYLIST_POPOVER_ID);
    if (pop && !pop.contains(e.target)) {
      closePlaylistPopover();
    }
  });

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') closePlaylistPopover();
  });

  // ─── Action Pills (Watch Page, Playlist Page, Sidebar Panel) ───────

  function buildActionPill({ id, singleLabel, isFloating, purePlaylistPage = false, hasPlaylist = false, titleFn }) {
    const wrapper = dom(
      'div',
      {
        display: 'inline-flex',
        alignItems: 'center',
        height: '36px',
        borderRadius: '18px',
        overflow: 'hidden',
        flexShrink: '0',
        boxShadow: isFloating ? '0 4px 16px rgba(15, 23, 42, 0.65)' : '0 1px 4px rgba(0, 0, 0, 0.25)',
        marginLeft: isFloating ? '0' : '8px',
        verticalAlign: 'middle',
        zIndex: isFloating ? '2147483646' : '999',
      },
      { id }
    );
    wrapper.dataset.hasPlaylist = String(Boolean(hasPlaylist || purePlaylistPage));

    if (isFloating) {
      Object.assign(wrapper.style, {
        position: 'fixed',
        bottom: '20px',
        right: '20px',
      });
    }

    const select = createQualitySelect({ compact: false });
    wrapper.appendChild(select);

    if (!purePlaylistPage) {
      const btn = dom(
        'button',
        {
          backgroundColor: '#3b82f6',
          color: '#ffffff',
          border: 'none',
          borderRadius: hasPlaylist ? '0' : '0 18px 18px 0',
          padding: '0 13px',
          fontSize: '13px',
          fontWeight: '700',
          fontFamily: '"Roboto", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
          cursor: 'pointer',
          height: '36px',
          display: 'inline-flex',
          alignItems: 'center',
          whiteSpace: 'nowrap',
        },
        {
          text: singleLabel,
          title: 'Download ONLY this video in the selected resolution / audio format',
          onMouseEnter: () => {
            if (!btn.disabled) btn.style.backgroundColor = '#2563eb';
          },
          onMouseLeave: () => {
            if (!btn.disabled) btn.style.backgroundColor = '#3b82f6';
          },
          onClick: (e) => {
            e.preventDefault();
            e.stopPropagation();
            sendDownloadRequest({
              btn,
              defaultLabel: singleLabel,
              targetUrl: location.href,
              title: titleFn(),
              quality: select.value,
              playlist: false,
            });
          },
        }
      );
      wrapper.appendChild(btn);
    }

    if (hasPlaylist || purePlaylistPage) {
      const plLabel = purePlaylistPage ? '📑 Playlist Options ▾' : '📑 Playlist ▾';
      const plBtn = dom(
        'button',
        {
          backgroundColor: '#0284c7',
          color: '#ffffff',
          border: 'none',
          borderLeft: purePlaylistPage ? 'none' : '1px solid rgba(255, 255, 255, 0.25)',
          borderRadius: '0 18px 18px 0',
          padding: '0 13px',
          fontSize: '12.5px',
          fontWeight: '700',
          fontFamily: '"Roboto", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
          cursor: 'pointer',
          height: '36px',
          display: 'inline-flex',
          alignItems: 'center',
          whiteSpace: 'nowrap',
        },
        {
          text: plLabel,
          title: 'Choose how many playlist videos to download, custom range, checklist & format',
          onMouseEnter: () => {
            if (!plBtn.disabled) plBtn.style.backgroundColor = '#0369a1';
          },
          onMouseLeave: () => {
            if (!plBtn.disabled) plBtn.style.backgroundColor = '#0284c7';
          },
          onClick: (e) => {
            e.preventDefault();
            e.stopPropagation();
            openPlaylistPopover(plBtn);
          },
        }
      );
      wrapper.appendChild(plBtn);
    }

    return wrapper;
  }

  // Injects a prominent "📑 Grab Playlist ▾" button directly inside YouTube's right-side Playlist Panel header too!
  function injectPlaylistPanelHeaderButton() {
    if (!isYouTubeWatchOrShorts() || !getPlaylistId()) {
      const old = document.getElementById(YT_PANEL_BTN_ID);
      if (old) old.remove();
      return;
    }
    if (document.getElementById(YT_PANEL_BTN_ID)) return;

    const panelHeader =
      document.querySelector('ytd-playlist-panel-renderer:not([hidden]) #playlist-actions #top-level-buttons-computed') ||
      document.querySelector('ytd-playlist-panel-renderer:not([hidden]) #playlist-actions') ||
      document.querySelector('ytd-playlist-panel-renderer:not([hidden]) .header');

    if (!panelHeader) return;

    const btn = dom(
      'button',
      {
        backgroundColor: '#0284c7',
        color: '#ffffff',
        border: 'none',
        borderRadius: '15px',
        padding: '0 11px',
        height: '30px',
        marginLeft: '6px',
        fontSize: '12px',
        fontWeight: '700',
        fontFamily: '"Roboto", -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
        cursor: 'pointer',
        display: 'inline-flex',
        alignItems: 'center',
        flexShrink: '0',
        whiteSpace: 'nowrap',
      },
      {
        id: YT_PANEL_BTN_ID,
        text: '📑 Grab Playlist ▾',
        title: 'Download custom count, range, or audio/video from this playlist',
        onClick: (e) => {
          e.preventDefault();
          e.stopPropagation();
          openPlaylistPopover(btn);
        },
      }
    );
    panelHeader.appendChild(btn);
  }

  function injectYouTubeWidget() {
    if (!isYouTubeWatchOrShorts() && !isYouTubePlaylistPage()) {
      removeYouTubeWidget();
      return;
    }

    injectPlaylistPanelHeaderButton();

    const hasPlaylist = Boolean(getPlaylistId());
    const existing = document.getElementById(YT_WIDGET_ID);
    if (existing) {
      // If SPA navigated between non-playlist and playlist video, rebuild pill immediately!
      if (existing.dataset.hasPlaylist === String(Boolean(hasPlaylist || isYouTubePlaylistPage()))) {
        return;
      }
      existing.remove();
    }

    // 1. Dedicated YouTube Playlist page (/playlist?list=...)
    if (isYouTubePlaylistPage()) {
      const plHeaderActions =
        document.querySelector('yt-page-header-view-model .ytFlexibleActionsViewModelActionRow') ||
        document.querySelector('ytd-playlist-header-renderer .immersive-header-content .metadata-action-bar') ||
        document.querySelector('ytd-playlist-header-renderer .play-menu');

      const pill = buildActionPill({
        id: YT_WIDGET_ID,
        singleLabel: '',
        isFloating: !plHeaderActions,
        purePlaylistPage: true,
        hasPlaylist: true,
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
        singleLabel: '⚡ GrabAll',
        isFloating: true,
        hasPlaylist: false,
        titleFn: getYouTubeTitle,
      });
      document.body.appendChild(floating);
      return;
    }

    // 3. YouTube Watch page (/watch?v=... [&list=...])
    // Prefer #owner (right next to Subscribe button) so YouTube's action-bar overflow never clips our Playlist button!
    const ownerBar = document.querySelector('ytd-watch-metadata #owner');
    const actionBar =
      ownerBar ||
      document.querySelector('ytd-watch-metadata #actions #top-level-buttons-computed') ||
      document.querySelector('#actions #top-level-buttons-computed') ||
      document.querySelector('ytd-watch-metadata #actions');

    const singleLabel = hasPlaylist ? '⚡ Video' : '⚡ GrabAll';

    if (actionBar) {
      const widget = buildActionPill({
        id: YT_WIDGET_ID,
        singleLabel,
        isFloating: false,
        hasPlaylist,
        titleFn: getYouTubeTitle,
      });
      actionBar.appendChild(widget);
    } else if (document.querySelector('video')) {
      const floating = buildActionPill({
        id: YT_WIDGET_ID,
        singleLabel,
        isFloating: true,
        hasPlaylist,
        titleFn: getYouTubeTitle,
      });
      document.body.appendChild(floating);
    }
  }

  function removeYouTubeWidget() {
    const existing = document.getElementById(YT_WIDGET_ID);
    if (existing) existing.remove();
    const panelBtn = document.getElementById(YT_PANEL_BTN_ID);
    if (panelBtn) panelBtn.remove();
  }

  // ─── IDM-Style Per-Video Hover Overlay (Feeds + YouTube Player) ────

  let hoverWidget = null;
  let hoverPlBtn = null;
  let hoverSingleBtn = null;
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

  function resolveVideoTarget(videoEl) {
    if (isYouTubeWatchOrShorts()) {
      return { url: location.href, title: getYouTubeTitle() };
    }

    const directSrc = videoEl.currentSrc || videoEl.src;
    if (directSrc && /^https?:\/\//i.test(directSrc) && !isYouTubeHost()) {
      const socialHosts = ['facebook.com', 'fb.watch', 'instagram.com', 'tiktok.com', 'x.com', 'twitter.com', 'reddit.com'];
      const onSocial = socialHosts.some((h) => location.hostname.includes(h));
      if (!onSocial) {
        return { url: directSrc, title: document.title };
      }
    }

    const permalinkPatterns = [
      /\/reel\/[A-Za-z0-9_-]+/,
      /\/reels\/[A-Za-z0-9_-]+/,
      /\/videos\/\d+/,
      /\/watch\/?\?v=\d+/,
      /\/watch\?v=[A-Za-z0-9_-]+/,
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

    if (isSingleVideoPermalinkPage() || (directSrc && /^https?:\/\//i.test(directSrc))) {
      return { url: location.href, title: document.title };
    }

    return { url: location.href, title: document.title };
  }

  function ensureHoverWidget() {
    if (hoverWidget && document.body.contains(hoverWidget)) {
      return hoverWidget;
    }

    const wrapper = dom(
      'div',
      {
        position: 'fixed',
        top: '-9999px',
        left: '-9999px',
        display: 'none',
        alignItems: 'center',
        height: '30px',
        borderRadius: '15px',
        overflow: 'hidden',
        boxShadow: '0 4px 14px rgba(0, 0, 0, 0.55)',
        zIndex: '2147483646',
        opacity: '0.96',
      },
      { id: HOVER_WIDGET_ID }
    );

    const select = createQualitySelect({ compact: true });

    const btn = dom(
      'button',
      {
        backgroundColor: '#3b82f6',
        color: '#ffffff',
        border: 'none',
        padding: '0 11px',
        fontSize: '12px',
        fontWeight: '700',
        fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif',
        cursor: 'pointer',
        height: '30px',
        display: 'inline-flex',
        alignItems: 'center',
        whiteSpace: 'nowrap',
      },
      {
        text: '⚡ Grab',
        title: 'Download this video only',
        onMouseEnter: () => {
          if (!btn.disabled) btn.style.backgroundColor = '#2563eb';
        },
        onMouseLeave: () => {
          if (!btn.disabled) btn.style.backgroundColor = '#3b82f6';
        },
        onClick: (e) => {
          e.preventDefault();
          e.stopPropagation();
          if (!hoveredVideo) return;
          const target = resolveVideoTarget(hoveredVideo);
          sendDownloadRequest({
            btn,
            defaultLabel: btn.dataset.baseLabel || '⚡ Grab',
            targetUrl: target.url,
            title: target.title,
            quality: select.value,
            playlist: false,
          });
        },
      }
    );

    const plBtn = dom(
      'button',
      {
        backgroundColor: '#0284c7',
        color: '#ffffff',
        border: 'none',
        borderLeft: '1px solid rgba(255, 255, 255, 0.25)',
        borderRadius: '0 15px 15px 0',
        padding: '0 10px',
        fontSize: '11.5px',
        fontWeight: '700',
        fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif',
        cursor: 'pointer',
        height: '30px',
        display: 'none',
        alignItems: 'center',
        whiteSpace: 'nowrap',
      },
      {
        text: '📑 Playlist ▾',
        title: 'Open Smart Playlist Downloader (custom video count, range & audio/video)',
        onClick: (e) => {
          e.preventDefault();
          e.stopPropagation();
          openPlaylistPopover(plBtn);
        },
      }
    );

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
    wrapper.appendChild(plBtn);
    document.body.appendChild(wrapper);
    hoverWidget = wrapper;
    hoverSingleBtn = btn;
    hoverPlBtn = plBtn;
    return wrapper;
  }

  function positionHoverWidget(videoEl) {
    const rect = videoEl.getBoundingClientRect();
    if (rect.width < 160 || rect.height < 100) return false;
    if (rect.bottom < 40 || rect.top > window.innerHeight - 40) return false;

    const widget = ensureHoverWidget();
    const showPlaylist = isYouTubeWatchOrShorts() && Boolean(getPlaylistId());
    if (hoverPlBtn && hoverSingleBtn) {
      hoverPlBtn.style.display = showPlaylist ? 'inline-flex' : 'none';
      hoverSingleBtn.style.borderRadius = showPlaylist ? '0' : '0 15px 15px 0';
      const lbl = showPlaylist ? '⚡ Video' : '⚡ Grab';
      hoverSingleBtn.dataset.baseLabel = lbl;
      if (!hoverSingleBtn.disabled) {
        hoverSingleBtn.textContent = lbl;
      }
    }

    widget.style.display = 'inline-flex';

    const top = Math.max(8, rect.top + 10);
    const widgetWidth = widget.offsetWidth || (showPlaylist ? 245 : 155);
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
    }, 650);
  }

  let lastMouseCheck = 0;
  document.addEventListener(
    'mousemove',
    (e) => {
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

      // Skip hover on YouTube Shorts where controls occupy top-right
      if (isYouTubeHost() && location.pathname.startsWith('/shorts/')) return;

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
      singleLabel: '⚡ GrabAll',
      isFloating: true,
      hasPlaylist: false,
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

    closePlaylistPopover();
    removeYouTubeWidget();
    removeFloatingWidget();

    if (isYouTubeWatchOrShorts() || isYouTubePlaylistPage()) {
      setTimeout(injectYouTubeWidget, 700);
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

      if (isYouTubeWatchOrShorts() || isYouTubePlaylistPage()) {
        injectYouTubeWidget();
      } else if (isYouTubeHost()) {
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
