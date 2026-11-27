// GrabAll Popup Script

const VALID_STATUSES = new Set([
  'downloading',
  'queued',
  'paused',
  'completed',
  'error',
  'canceled',
]);

function sanitizeStatus(status) {
  const s = String(status || '').toLowerCase().trim();
  return VALID_STATUSES.has(s) ? s : 'queued';
}

function escapeHtml(str) {
  if (!str) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}

function formatBytes(bytes) {
  const num = Number(bytes);
  if (!Number.isFinite(num) || num <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  const exp = Math.min(Math.floor(Math.log(num) / Math.log(1024)), units.length - 1);
  const val = num / Math.pow(1024, exp);
  return exp === 0 ? `${Math.round(val)} B` : `${val.toFixed(1)} ${units[exp]}`;
}

function getTaskIcon(task) {
  if (!task) return '📄';
  const qual = String(task.quality || '').toLowerCase();
  const name = String(task.fileName || task.title || task.url || '').toLowerCase();
  if (qual.startsWith('audio') || /\.(mp3|m4a|opus|flac|wav|aac|ogg)(\?|#|$)/.test(name)) {
    return '🎵';
  }
  if (task.engine === 'ytdlp' || /\.(mp4|mkv|webm|mov|avi|flv|ts|m3u8|mpd)(\?|#|$)/.test(name)) {
    return '🎬';
  }
  if (/\.(zip|rar|7z|tar|gz|bz2|xz|iso|tgz)(\?|#|$)/.test(name)) {
    return '📦';
  }
  if (/\.(exe|msi|apk|deb|rpm|dmg)(\?|#|$)/.test(name)) {
    return '💻';
  }
  if (/\.(jpg|jpeg|png|gif|webp|svg)(\?|#|$)/.test(name)) {
    return '🖼️';
  }
  return '📄';
}

function sortTasksForPopup(list) {
  const priority = {
    downloading: 0,
    queued: 1,
    paused: 2,
    error: 3,
    canceled: 3,
    completed: 4,
  };
  return [...(Array.isArray(list) ? list : [])].sort((a, b) => {
    const sa = sanitizeStatus(a?.status);
    const sb = sanitizeStatus(b?.status);
    const pa = priority[sa] ?? 5;
    const pb = priority[sb] ?? 5;
    if (pa !== pb) return pa - pb;
    const ta = Date.parse(a?.updatedAt || a?.createdAt || '') || 0;
    const tb = Date.parse(b?.updatedAt || b?.createdAt || '') || 0;
    return tb - ta;
  });
}

function filterTasksForPopup(list, filter = 'all', query = '') {
  const items = Array.isArray(list) ? list : [];
  const f = String(filter || 'all').toLowerCase();
  const tokens = String(query || '').trim().toLowerCase().split(/\s+/).filter(Boolean);

  return items.filter((t) => {
    if (!t) return false;
    const st = sanitizeStatus(t.status);
    if (f === 'active') {
      if (st !== 'downloading' && st !== 'queued' && st !== 'paused') return false;
    } else if (f === 'completed') {
      if (st !== 'completed') return false;
    }

    if (tokens.length === 0) return true;
    const haystack = [
      t.title || '',
      t.fileName || '',
      t.url || '',
      t.quality || '',
      t.status || '',
      t.errorMessage || '',
    ].join('\n').toLowerCase();
    return tokens.every((tok) => haystack.includes(tok));
  });
}

function renderSegmentStripHtml(task) {
  if (
    !task ||
    sanitizeStatus(task.status) !== 'downloading' ||
    !Array.isArray(task.segments) ||
    task.segments.length === 0 ||
    !(Number(task.totalBytes) > 0)
  ) {
    return '';
  }
  const totalBytes = Number(task.totalBytes);
  const activeCount = task.segments.filter((s) => s && s.active).length;
  const blocks = task.segments
    .map((seg) => {
      if (!seg) return '';
      const start = Math.max(0, Number(seg.start) || 0);
      const end = Number(seg.end) || 0;
      const cur = Math.min(Number(seg.current) || 0, end + 1);
      const done = Math.max(0, cur - start);
      if (done <= 0) return '';
      const leftPct = Math.max(0, Math.min(100, (start / totalBytes) * 100));
      const widthPct = Math.max(0.4, Math.min(100 - leftPct, (done / totalBytes) * 100));
      const cls = seg.active ? 'dl-seg-fill active' : 'dl-seg-fill';
      return `<span class="${cls}" style="left:${leftPct.toFixed(2)}%;width:${widthPct.toFixed(2)}%;"></span>`;
    })
    .join('');

  if (!blocks) return '';
  return `<div class="dl-seg-bar" title="Connections: ${activeCount} active / ${task.segments.length} segments">${blocks}</div>`;
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    sanitizeStatus,
    escapeHtml,
    formatBytes,
    getTaskIcon,
    sortTasksForPopup,
    filterTasksForPopup,
    renderSegmentStripHtml,
  };
}

// Browser Popup Runtime
if (typeof document !== 'undefined') {
  let activeTab = null;
  let daemonOnline = false;
  let daemonUrl = 'http://127.0.0.1:17890';
  let daemonToken = '';
  let tasks = [];
  let currentTab = 'capture';
  let dlFilter = 'all';
  let dlSearch = '';
  let sseConn = null;
  let pollTimer = null;
  const cardNodes = new Map();

  function stopFallbackPoll() {
    if (pollTimer) {
      clearInterval(pollTimer);
      pollTimer = null;
    }
  }

  function cleanupLiveConnections() {
    if (sseConn) {
      sseConn.close();
      sseConn = null;
    }
    stopFallbackPoll();
  }

  function startFallbackPoll() {
    if (pollTimer) return;
    pollTimer = setInterval(fetchQueueOnce, 1500);
  }

  function switchTab(tabName) {
    currentTab = tabName || 'capture';
    document.querySelectorAll('.tab-btn').forEach((btn) => {
      const active = btn.dataset.tab === currentTab;
      btn.classList.toggle('active', active);
      btn.setAttribute('aria-selected', active ? 'true' : 'false');
    });
    document.querySelectorAll('.tab-panel').forEach((panel) => {
      panel.classList.toggle('active', panel.id === `panel-${currentTab}`);
    });
    if (currentTab === 'downloads') {
      renderDownloadsView();
    } else if (currentTab === 'settings' && daemonOnline) {
      refreshDaemonSettings();
    }
  }

  function renderPopupCookiesStatus(signedIn, count) {
    const el = document.getElementById('popup-cookies-status');
    if (!el) return;
    if (signedIn) {
      el.style.color = '#34d399';
      el.textContent = `Signed In (${count || 0})`;
    } else {
      el.style.color = '#fbbf24';
      el.textContent = 'Not Signed In';
    }
  }

  function renderPopupYtDlpStatus(info) {
    if (!info) return;
    const statusEl = document.getElementById('popup-ytdlp-status');
    const progWrap = document.getElementById('popup-ytdlp-prog-wrap');
    const progFill = document.getElementById('popup-ytdlp-prog-fill');
    const btnCheck = document.getElementById('btn-popup-check-ytdlp');
    const btnUpdate = document.getElementById('btn-popup-update-ytdlp');

    const banner = document.getElementById('ytdlp-popup-banner');
    const bannerText = document.getElementById('ytdlp-popup-banner-text');
    const bannerBtn = document.getElementById('btn-popup-banner-update-ytdlp');
    const bannerProgWrap = document.getElementById('ytdlp-popup-banner-prog-wrap');
    const bannerProgFill = document.getElementById('ytdlp-popup-banner-prog-fill');

    const pct = Math.max(0, Math.min(100, Number(info.progress) || 0));

    if (info.updating) {
      const msg = info.statusText || 'Updating yt-dlp...';
      if (statusEl) {
        statusEl.style.color = '#60a5fa';
        statusEl.textContent = pct > 0 ? `Updating ${pct}%` : 'Updating...';
      }
      if (progWrap && progFill) {
        progWrap.style.display = 'block';
        progFill.style.width = `${Math.max(8, pct)}%`;
      }
      if (btnCheck) btnCheck.disabled = true;
      if (btnUpdate) {
        btnUpdate.disabled = true;
        btnUpdate.textContent = pct > 0 ? `⏳ ${pct}%...` : '⏳ Updating...';
      }
      if (banner && bannerText) {
        banner.classList.remove('hidden');
        bannerText.textContent = `⏳ ${msg}`;
        if (bannerBtn) {
          bannerBtn.disabled = true;
          bannerBtn.textContent = pct > 0 ? `${pct}%` : '...';
        }
        if (bannerProgWrap && bannerProgFill) {
          bannerProgWrap.style.display = 'block';
          bannerProgFill.style.width = `${Math.max(8, pct)}%`;
        }
      }
      return;
    }

    if (btnCheck) btnCheck.disabled = false;
    if (progWrap) progWrap.style.display = 'none';
    if (bannerProgWrap) bannerProgWrap.style.display = 'none';
    if (bannerBtn) {
      bannerBtn.disabled = false;
      bannerBtn.textContent = 'Update';
    }

    if (info.updateAvailable && info.latestVersion) {
      const range = info.currentVersion
        ? `${info.currentVersion} → ${info.latestVersion}`
        : info.latestVersion;
      if (statusEl) {
        statusEl.style.color = '#fbbf24';
        statusEl.textContent = `Update Found: ${range}`;
      }
      if (btnUpdate) {
        btnUpdate.disabled = false;
        btnUpdate.textContent = `⬇️ Update (${info.latestVersion})`;
      }
      if (banner && bannerText) {
        banner.classList.remove('hidden');
        bannerText.textContent = `🎬 yt-dlp Update Found: ${range}`;
      }
    } else if (info.installed) {
      if (statusEl) {
        statusEl.style.color = '#34d399';
        statusEl.textContent = info.currentVersion ? `v${info.currentVersion} (Up to date)` : 'Installed';
      }
      if (btnUpdate) {
        btnUpdate.disabled = false;
        btnUpdate.textContent = '⬇️ Reinstall / Update';
      }
      if (banner) banner.classList.add('hidden');
    } else {
      if (statusEl) {
        statusEl.style.color = '#fbbf24';
        statusEl.textContent = 'Not Installed';
      }
      if (btnUpdate) {
        btnUpdate.disabled = false;
        btnUpdate.textContent = '⬇️ Install yt-dlp';
      }
      if (banner) banner.classList.add('hidden');
    }
  }

  function refreshDaemonSettings() {
    chrome.runtime.sendMessage({ type: 'GET_DAEMON_CONFIG' }, (res) => {
      if (chrome.runtime.lastError || !res || !res.success || !res.config) return;
      const cfg = res.config;
      const wdSel = document.getElementById('select-when-done');
      if (wdSel) {
        wdSel.value = cfg.whenDoneAction || 'none';
      }
      const limSel = document.getElementById('select-global-limit');
      if (limSel) {
        limSel.value = String(cfg.speedLimitKBps || 0);
      }
      renderPopupCookiesStatus(Boolean(cfg.youtubeCookiesSynced), cfg.youtubeCookiesCount || 0);
      renderPopupYtDlpStatus({
        installed: Boolean(cfg.ytDlpAvailable),
        currentVersion: cfg.ytDlpVersion || '',
        latestVersion: cfg.ytDlpLatestVersion || '',
        updateAvailable: Boolean(cfg.ytDlpUpdateAvailable),
        updating: Boolean(cfg.ytDlpUpdating),
        statusText: cfg.ytDlpStatusText || '',
        progress: cfg.ytDlpProgress || 0,
      });
    });
  }

  function fetchQueueOnce() {
    chrome.runtime.sendMessage({ type: 'GET_QUEUE' }, (res) => {
      if (chrome.runtime.lastError || !res || !res.success) {
        daemonOnline = false;
        const dot = document.getElementById('daemon-dot');
        const statusText = document.getElementById('daemon-status-text');
        if (dot) dot.className = 'dot offline';
        if (statusText) statusText.textContent = 'GrabAll Not Running';
        renderDownloadsView();
        return;
      }
      daemonOnline = true;
      if (res.daemonUrl) daemonUrl = res.daemonUrl;
      if (res.token) daemonToken = res.token;
      tasks = Array.isArray(res.tasks) ? res.tasks : [];
      const dot = document.getElementById('daemon-dot');
      const statusText = document.getElementById('daemon-status-text');
      if (dot) dot.className = 'dot online';
      if (statusText) statusText.textContent = 'Connected';
      renderDownloadsView();
    });
  }

  function connectQueueSSE(url, token) {
    if (sseConn) {
      sseConn.close();
      sseConn = null;
    }
    const cleanUrl = String(url || daemonUrl).replace(/\/+$/, '');
    try {
      const qs = token ? `?token=${encodeURIComponent(token)}` : '';
      sseConn = new EventSource(`${cleanUrl}/api/events${qs}`);
    } catch (e) {
      startFallbackPoll();
      return;
    }

    const onQueueData = (e) => {
      stopFallbackPoll();
      daemonOnline = true;
      const dot = document.getElementById('daemon-dot');
      const statusText = document.getElementById('daemon-status-text');
      if (dot) dot.className = 'dot online';
      if (statusText) statusText.textContent = 'Connected';
      try {
        const parsed = JSON.parse(e.data);
        tasks = Array.isArray(parsed) ? parsed : [];
        renderDownloadsView();
      } catch (err) {
        // Ignore malformed frame
      }
    };

    sseConn.addEventListener('queue_init', onQueueData);
    sseConn.addEventListener('queue_update', onQueueData);
    sseConn.addEventListener('ytdlp_status', (e) => {
      try {
        renderPopupYtDlpStatus(JSON.parse(e.data));
      } catch (err) {
        // Ignore malformed frame
      }
    });
    sseConn.onerror = () => {
      startFallbackPoll();
    };
  }

  function reconcileChildOrder(container, desiredNodes) {
    const desiredSet = new Set(desiredNodes);
    Array.from(container.children).forEach((child) => {
      if (!desiredSet.has(child)) {
        container.removeChild(child);
      }
    });
    desiredNodes.forEach((node, idx) => {
      const currentAtIdx = container.children[idx];
      if (currentAtIdx !== node) {
        container.insertBefore(node, currentAtIdx || null);
      }
    });
  }

  function sendTaskAction(action, id, btnEl) {
    if (btnEl) btnEl.disabled = true;
    chrome.runtime.sendMessage(
      { type: 'TASK_ACTION', action, id, deleteFile: false },
      (res) => {
        if (btnEl) btnEl.disabled = false;
        if (chrome.runtime.lastError || !res || !res.success) {
          const msg = res?.error || chrome.runtime.lastError?.message || 'Action failed';
          showAlert('Error: ' + msg, true);
          return;
        }
        if (action === 'clear_completed') {
          showAlert('Cleared completed downloads', false);
        }
        fetchQueueOnce();
      }
    );
  }

  function buildCardActionsHtml(status) {
    switch (status) {
      case 'completed':
        return (
          '<button type="button" class="dl-btn dl-btn-primary" data-act="open_file">▶ Open</button>' +
          '<button type="button" class="dl-btn" data-act="open_folder">📂 Folder</button>' +
          '<button type="button" class="dl-btn dl-btn-danger" data-act="delete" title="Remove from list">Remove</button>'
        );
      case 'downloading':
        return (
          '<button type="button" class="dl-btn" data-act="pause">⏸ Pause</button>' +
          '<button type="button" class="dl-btn dl-btn-danger" data-act="cancel">✕ Cancel</button>'
        );
      case 'queued':
        return '<button type="button" class="dl-btn dl-btn-danger" data-act="cancel">✕ Cancel</button>';
      case 'paused':
        return (
          '<button type="button" class="dl-btn dl-btn-primary" data-act="resume">▶ Resume</button>' +
          '<button type="button" class="dl-btn" data-act="cancel">✕ Cancel</button>' +
          '<button type="button" class="dl-btn dl-btn-danger" data-act="delete">Remove</button>'
        );
      default:
        return (
          '<button type="button" class="dl-btn dl-btn-primary" data-act="resume">↻ Retry</button>' +
          '<button type="button" class="dl-btn dl-btn-danger" data-act="delete">Remove</button>'
        );
    }
  }

  function createDownloadCard(task) {
    const card = document.createElement('div');
    card.className = 'dl-card';
    card.id = `popup-task-${task.id}`;
    card.innerHTML = `
      <div class="dl-header">
        <div class="dl-title-wrap">
          <span class="dl-icon"></span>
          <div class="dl-title"></div>
        </div>
        <span class="dl-badge"></span>
      </div>
      <div class="dl-error" style="display:none;"></div>
      <div>
        <div class="dl-progress-bg">
          <div class="dl-progress-fill"></div>
        </div>
        <div class="dl-seg-container"></div>
      </div>
      <div class="dl-stats">
        <span class="dl-stat-left"></span>
        <span class="dl-stat-right"></span>
      </div>
      <div class="dl-actions"></div>
    `;
    updateDownloadCard(card, task);
    return card;
  }

  function updateDownloadCard(card, task) {
    const safeStatus = sanitizeStatus(task.status);
    const title = task.title || task.fileName || task.url || 'Download';
    const pctNum = Math.max(0, Math.min(100, Number(task.progress) || 0));
    const pctText = pctNum.toFixed(1) + '%';

    const iconEl = card.querySelector('.dl-icon');
    if (iconEl) iconEl.textContent = getTaskIcon(task);

    const titleEl = card.querySelector('.dl-title');
    if (titleEl) {
      titleEl.textContent = title;
      titleEl.title = title;
    }

    const badgeEl = card.querySelector('.dl-badge');
    if (badgeEl) {
      badgeEl.className = `dl-badge status-${safeStatus}`;
      badgeEl.textContent = safeStatus;
    }

    const errEl = card.querySelector('.dl-error');
    if (errEl) {
      if (task.errorMessage && (safeStatus === 'error' || safeStatus === 'canceled')) {
        errEl.style.display = 'block';
        errEl.textContent = `⚠ ${task.errorMessage}`;
        errEl.title = task.errorMessage;
      } else {
        errEl.style.display = 'none';
        errEl.textContent = '';
      }
    }

    const fillEl = card.querySelector('.dl-progress-fill');
    if (fillEl) {
      fillEl.className = `dl-progress-fill status-${safeStatus}`;
      fillEl.style.width = safeStatus === 'completed' ? '100%' : `${pctNum.toFixed(1)}%`;
    }

    const segEl = card.querySelector('.dl-seg-container');
    if (segEl) {
      segEl.innerHTML = renderSegmentStripHtml(task);
    }

    const sizeStr =
      Number(task.totalBytes) > 0
        ? `${formatBytes(task.downloadedBytes)} / ${formatBytes(task.totalBytes)}`
        : formatBytes(task.downloadedBytes);
    const statLeft = card.querySelector('.dl-stat-left');
    if (statLeft) {
      statLeft.textContent = safeStatus === 'completed' ? sizeStr : `${pctText} • ${sizeStr}`;
    }

    const statRight = card.querySelector('.dl-stat-right');
    if (statRight) {
      const rightParts = [];
      if (task.speed && safeStatus === 'downloading') rightParts.push(`⚡ ${task.speed}`);
      if (task.eta && safeStatus === 'downloading') rightParts.push(`⏱ ${task.eta}`);
      statRight.textContent = rightParts.join(' • ');
    }

    if (card.dataset.status !== safeStatus) {
      card.dataset.status = safeStatus;
      const actionsEl = card.querySelector('.dl-actions');
      if (actionsEl) {
        actionsEl.innerHTML = buildCardActionsHtml(safeStatus);
        actionsEl.querySelectorAll('.dl-btn').forEach((btn) => {
          btn.addEventListener('click', () => {
            sendTaskAction(btn.dataset.act, task.id, btn);
          });
        });
      }
    }
  }

  function renderDownloadsView() {
    let downloading = 0;
    let queued = 0;
    let paused = 0;
    let completed = 0;
    const activeSpeeds = [];

    for (const t of tasks) {
      if (!t) continue;
      const st = sanitizeStatus(t.status);
      if (st === 'downloading') {
        downloading++;
        if (t.speed) activeSpeeds.push(t.speed);
      } else if (st === 'queued') {
        queued++;
      } else if (st === 'paused') {
        paused++;
      } else if (st === 'completed') {
        completed++;
      }
    }

    const activeBadgeCount = downloading + queued;
    const badgeEl = document.getElementById('tab-active-badge');
    if (badgeEl) {
      badgeEl.textContent = String(activeBadgeCount);
      badgeEl.classList.toggle('hidden', !daemonOnline || activeBadgeCount === 0);
    }

    const cntAllEl = document.getElementById('dl-count-all');
    const cntActiveEl = document.getElementById('dl-count-active');
    const cntDoneEl = document.getElementById('dl-count-done');
    if (cntAllEl) cntAllEl.textContent = String(tasks.length);
    if (cntActiveEl) cntActiveEl.textContent = String(downloading + queued + paused);
    if (cntDoneEl) cntDoneEl.textContent = String(completed);

    const sortedAll = sortTasksForPopup(tasks);

    // Update Capture tab compact active banner
    const miniBanner = document.getElementById('active-mini-banner');
    if (miniBanner) {
      if (daemonOnline && activeBadgeCount > 0 && sortedAll.length > 0) {
        const topTask = sortedAll[0];
        const pct = Math.max(0, Math.min(100, Number(topTask.progress) || 0));
        const topName = topTask.title || topTask.fileName || topTask.url || 'Download';
        const speedBit = topTask.speed ? ` • ⚡ ${topTask.speed}` : '';
        const textEl = document.getElementById('mini-banner-text');
        const fillEl = document.getElementById('mini-banner-progress-fill');
        if (textEl) {
          textEl.textContent = `[${activeBadgeCount} active] ${topName} (${pct.toFixed(0)}%${speedBit})`;
        }
        if (fillEl) {
          fillEl.style.width = `${pct.toFixed(1)}%`;
        }
        miniBanner.classList.remove('hidden');
      } else {
        miniBanner.classList.add('hidden');
      }
    }

    // Update summary bar in Downloads tab
    const summaryEl = document.getElementById('dl-summary-text');
    if (summaryEl) {
      if (!daemonOnline) {
        summaryEl.textContent = 'GrabAll app is offline';
      } else if (downloading > 0) {
        const spdTxt = activeSpeeds.length > 0 ? ` • ⚡ ${activeSpeeds.join(', ')}` : '';
        const qTxt = queued > 0 ? ` • ${queued} queued` : '';
        summaryEl.textContent = `${downloading} downloading${spdTxt}${qTxt}`;
      } else if (queued > 0) {
        summaryEl.textContent = `${queued} queued`;
      } else if (tasks.length > 0) {
        summaryEl.textContent = `${tasks.length} download${tasks.length === 1 ? '' : 's'} • idle`;
      } else {
        summaryEl.textContent = 'No active downloads';
      }
    }

    const listEl = document.getElementById('downloads-list');
    if (!listEl) return;

    if (!daemonOnline) {
      cardNodes.clear();
      listEl.innerHTML = '';
      const emptyDiv = document.createElement('div');
      emptyDiv.className = 'dl-empty';
      emptyDiv.innerHTML =
        '<span>GrabAll app is not running or reachable.</span>' +
        '<button type="button" class="btn btn-secondary dl-retry-btn">↻ Retry Connection</button>';
      emptyDiv.querySelector('.dl-retry-btn')?.addEventListener('click', () => {
        checkDaemonStatus();
      });
      listEl.appendChild(emptyDiv);
      return;
    }

    const existingIds = new Set(tasks.map((t) => t && t.id).filter(Boolean));
    for (const key of cardNodes.keys()) {
      if (!existingIds.has(key)) {
        cardNodes.delete(key);
      }
    }

    const filtered = filterTasksForPopup(sortedAll, dlFilter, dlSearch);
    if (filtered.length === 0) {
      listEl.innerHTML = '';
      const emptyDiv = document.createElement('div');
      emptyDiv.className = 'dl-empty';
      emptyDiv.textContent =
        tasks.length === 0
          ? 'No downloads yet. Capture a video or link from any tab.'
          : 'No downloads match this filter.';
      listEl.appendChild(emptyDiv);
      return;
    }

    const visible = filtered.slice(0, 25);
    const desiredNodes = visible.map((task) => {
      let node = cardNodes.get(task.id);
      if (node) {
        updateDownloadCard(node, task);
      } else {
        node = createDownloadCard(task);
        cardNodes.set(task.id, node);
      }
      return node;
    });

    if (filtered.length > visible.length) {
      const hint = document.createElement('div');
      hint.className = 'dl-more-hint';
      hint.textContent = `Showing ${visible.length} of ${filtered.length} downloads (open GrabAll window for all)`;
      desiredNodes.push(hint);
    }

    reconcileChildOrder(listEl, desiredNodes);
  }

  async function loadConfig() {
    const cfg = await chrome.storage.local.get({
      authToken: '',
      interceptDownloads: false,
      daemonUrl: 'http://127.0.0.1:17890',
      preferredQuality: 'best',
      preferredVideoCodec: 'h264',
      preferredSpeedMode: 'turbo',
      preferredEmbedSubs: false,
      preferredSubLangs: 'en',
      preferredEmbedChapters: false,
    });

    daemonUrl = (cfg.daemonUrl || 'http://127.0.0.1:17890').replace(/\/+$/, '');
    daemonToken = cfg.authToken || '';

    const urlInput = document.getElementById('input-daemon-url');
    if (urlInput) urlInput.value = daemonUrl;

    document.getElementById('input-token').value = cfg.authToken;
    document.getElementById('check-intercept').checked = cfg.interceptDownloads;
    document.getElementById('select-quality').value = cfg.preferredQuality || 'best';

    const codecSel = document.getElementById('select-video-codec');
    if (codecSel) {
      codecSel.value = cfg.preferredVideoCodec || 'h264';
    }
    const speedSel = document.getElementById('select-speed-mode');
    if (speedSel) {
      speedSel.value = cfg.preferredSpeedMode === 'regular' ? 'regular' : 'turbo';
    }
    const subsCheck = document.getElementById('check-embed-subs');
    if (subsCheck) {
      subsCheck.checked = Boolean(cfg.preferredEmbedSubs);
    }
    const subLangInput = document.getElementById('input-sub-langs');
    if (subLangInput) {
      subLangInput.value = cfg.preferredSubLangs || 'en';
    }
    const chapCheck = document.getElementById('check-embed-chapters');
    if (chapCheck) {
      chapCheck.checked = Boolean(cfg.preferredEmbedChapters);
    }

    const hasList = activeTab && activeTab.url && activeTab.url.includes('list=');
    const isPurePl = activeTab && activeTab.url && activeTab.url.includes('/playlist?') && hasList;
    const plCheck = document.getElementById('check-playlist');
    const plRow = document.getElementById('playlist-range-row');
    if (isPurePl && plCheck) {
      plCheck.checked = true;
      if (plRow) plRow.style.display = 'flex';
      const dlBtn = document.getElementById('btn-download-page');
      if (dlBtn) dlBtn.textContent = '📑 Download Playlist Videos';
    } else if (hasList && plRow && plCheck?.checked) {
      plRow.style.display = 'flex';
    }
  }

  async function checkDaemonStatus() {
    const dot = document.getElementById('daemon-dot');
    const statusText = document.getElementById('daemon-status-text');
    const openBtn = document.getElementById('btn-open-dashboard');

    chrome.runtime.sendMessage(
      { type: 'CHECK_DAEMON', storeId: activeTab?.cookieStoreId || null },
      (response) => {
      if (chrome.runtime.lastError || !response || !response.online) {
        daemonOnline = false;
        cleanupLiveConnections();
        tasks = [];
        if (response?.daemonUrl) {
          daemonUrl = response.daemonUrl;
        }
        dot.className = 'dot offline';
        statusText.textContent = 'GrabAll Not Running';
        renderDownloadsView();
      } else {
        daemonOnline = true;
        if (response.daemonUrl) {
          daemonUrl = response.daemonUrl;
        }
        if (response.token) {
          daemonToken = response.token;
          document.getElementById('input-token').value = response.token;
        }
        dot.className = 'dot online';
        statusText.textContent = 'Connected';

        const limSel = document.getElementById('select-global-limit');
        if (limSel) {
          limSel.value = String(response.info?.speedLimitKBps || 0);
        }
        const wdSel = document.getElementById('select-when-done');
        if (wdSel) {
          wdSel.value = response.info?.whenDoneAction || 'none';
        }
        if (openBtn) {
          openBtn.textContent = response.info?.nativeGui ? '⚡ Open GrabAll Window' : '🌐 Open Web Dashboard';
        }
        if (response.info) {
          renderPopupCookiesStatus(
            Boolean(response.info.youtubeCookiesSynced),
            response.info.youtubeCookiesCount || 0
          );
          renderPopupYtDlpStatus({
            installed: Boolean(response.info.ytDlpAvailable),
            currentVersion: response.info.ytDlpVersion || '',
            latestVersion: response.info.ytDlpLatestVersion || '',
            updateAvailable: Boolean(response.info.ytDlpUpdateAvailable),
            updating: Boolean(response.info.ytDlpUpdating),
            statusText: response.info.ytDlpStatusText || '',
            progress: 0,
          });
        }

        fetchQueueOnce();
        connectQueueSSE(daemonUrl, daemonToken);
      }
    });
  }

  async function loadDetectedMedia() {
    if (!activeTab || !activeTab.id) return;
    const container = document.getElementById('media-container');

    chrome.runtime.sendMessage({ type: 'GET_TAB_MEDIA', tabId: activeTab.id }, (response) => {
      if (!response || !response.media || response.media.length === 0) {
        container.innerHTML = '<div class="empty-sniff">No video streams detected on this tab yet.</div>';
        return;
      }

      container.innerHTML = '';
      response.media.forEach((item) => {
        const div = document.createElement('div');
        div.className = 'media-item';

        const isStream = item.type.includes('m3u8') || item.type.includes('mpd');
        const engine = isStream ? 'ytdlp' : 'auto';

        div.innerHTML = `
          <div class="media-info">
            <span class="media-type">${escapeHtml(item.type)}</span>
            <div class="media-url" title="${escapeHtml(item.url)}">${escapeHtml(item.url)}</div>
          </div>
          <button type="button" class="btn btn-sm btn-primary btn-dl-stream">Download</button>
        `;

        div.querySelector('.btn-dl-stream').addEventListener('click', () => {
          const quality = document.getElementById('select-quality').value;
          const videoCodec = document.getElementById('select-video-codec')?.value || 'h264';
          const speedMode = document.getElementById('select-speed-mode')?.value || 'turbo';
          sendDownload(item.url, activeTab.title, engine, quality, false, 1, 0, speedMode, videoCodec);
        });

        container.appendChild(div);
      });
    });
  }

  function initListeners() {
    // Tab navigation
    document.querySelectorAll('.tab-btn').forEach((btn) => {
      btn.addEventListener('click', () => {
        switchTab(btn.dataset.tab || 'capture');
      });
    });

    // Mini active banner jumps straight to Downloads tab
    document.getElementById('active-mini-banner')?.addEventListener('click', () => {
      switchTab('downloads');
    });

    // Persist quality selection
    document.getElementById('select-quality').addEventListener('change', async (e) => {
      await chrome.storage.local.set({ preferredQuality: e.target.value });
    });

    // Persist video codec selection
    document.getElementById('select-video-codec')?.addEventListener('change', async (e) => {
      await chrome.storage.local.set({ preferredVideoCodec: e.target.value });
    });

    // Persist speed mode selection (Turbo 6x vs Watch Live 1x)
    document.getElementById('select-speed-mode')?.addEventListener('change', async (e) => {
      await chrome.storage.local.set({ preferredSpeedMode: e.target.value });
    });

    document.getElementById('check-embed-subs')?.addEventListener('change', async (e) => {
      await chrome.storage.local.set({ preferredEmbedSubs: e.target.checked });
    });

    document.getElementById('input-sub-langs')?.addEventListener('change', async (e) => {
      await chrome.storage.local.set({ preferredSubLangs: (e.target.value || 'en').trim() || 'en' });
    });

    document.getElementById('check-embed-chapters')?.addEventListener('change', async (e) => {
      await chrome.storage.local.set({ preferredEmbedChapters: e.target.checked });
    });

    const qualitySelect = document.getElementById('select-quality');
    const origQualityLabels = new Map();
    if (qualitySelect) {
      Array.from(qualitySelect.options).forEach((opt) => {
        origQualityLabels.set(opt.value, opt.textContent);
      });
    }

    document.getElementById('btn-preview-page')?.addEventListener('click', () => {
      if (!activeTab || !activeTab.url) return;
      const box = document.getElementById('page-preview-box');
      const btn = document.getElementById('btn-preview-page');
      const videoCodec = document.getElementById('select-video-codec')?.value || 'h264';
      if (!box) return;

      if (btn) {
        btn.disabled = true;
        btn.textContent = '⏳';
      }
      box.classList.remove('hidden');
      box.innerHTML = '<div class="popup-preview-meta">Fetching resolutions & sizes...</div>';

      chrome.runtime.sendMessage(
        {
          type: 'PREVIEW_MEDIA',
          payload: {
            url: activeTab.url,
            pageUrl: activeTab.url,
            videoCodec,
            storeId: activeTab?.cookieStoreId || null,
          },
        },
        (res) => {
          if (btn) {
            btn.disabled = false;
            btn.textContent = '🔍 Size';
          }
          if (chrome.runtime.lastError || !res || !res.success || !res.preview) {
            const msg = res?.error || chrome.runtime.lastError?.message || 'Preview unavailable';
            box.innerHTML = `<div class="popup-preview-meta" style="color:#fca5a5;">${escapeHtml(msg)}</div>`;
            return;
          }

          const prev = res.preview;
          const sizeMap = new Map();
          (prev.options || []).forEach((opt) => sizeMap.set(opt.quality, opt));

          if (qualitySelect) {
            Array.from(qualitySelect.options).forEach((optEl) => {
              const baseLabel = origQualityLabels.get(optEl.value) || optEl.textContent;
              const lookup = optEl.value.startsWith('audio-') ? 'audio-best' : optEl.value;
              const match = sizeMap.get(lookup);
              if (match && match.sizeBytes > 0) {
                const prefix = match.approx ? '~' : '';
                optEl.textContent = `${baseLabel} (${prefix}${formatBytes(match.sizeBytes)})`;
              } else {
                optEl.textContent = baseLabel;
              }
            });
          }

          const metaBits = [];
          if (prev.durationText) metaBits.push(`⏱ ${prev.durationText}`);
          if (Array.isArray(prev.chapters) && prev.chapters.length > 0) {
            metaBits.push(`🏷 ${prev.chapters.length} ch`);
          }
          const subCount = (prev.subtitleLangs?.length || 0) + (prev.autoSubtitleLangs?.length || 0);
          if (subCount > 0) {
            metaBits.push('💬 subs');
          }

          const curQual = qualitySelect ? qualitySelect.value : 'best';
          const pillsHtml = (prev.options || [])
            .map((opt) => {
              const sz = opt.sizeBytes > 0 ? `${opt.approx ? '~' : ''}${formatBytes(opt.sizeBytes)}` : 'N/A';
              const activeCls = opt.quality === curQual ? ' active' : '';
              return `<button type="button" class="popup-preview-pill${activeCls}" data-q="${escapeHtml(opt.quality)}">${escapeHtml(opt.label)}: <strong>${escapeHtml(sz)}</strong></button>`;
            })
            .join('');

          box.innerHTML = `
            <div class="popup-preview-title" title="${escapeHtml(prev.title || '')}">${escapeHtml(prev.title || activeTab.title || activeTab.url)}</div>
            <div class="popup-preview-meta">${escapeHtml(metaBits.join('  •  '))}</div>
            ${pillsHtml ? `<div class="popup-preview-pills">${pillsHtml}</div>` : ''}
          `;

          box.querySelectorAll('.popup-preview-pill').forEach((pill) => {
            pill.addEventListener('click', async () => {
              const q = pill.getAttribute('data-q') || 'best';
              if (qualitySelect) {
                qualitySelect.value = q;
                await chrome.storage.local.set({ preferredQuality: q });
              }
              box.querySelectorAll('.popup-preview-pill').forEach((p) => p.classList.remove('active'));
              pill.classList.add('active');
            });
          });
        }
      );
    });

    const plCheck = document.getElementById('check-playlist');
    const plRow = document.getElementById('playlist-range-row');
    if (plCheck && plRow) {
      plCheck.addEventListener('change', () => {
        plRow.style.display = plCheck.checked ? 'flex' : 'none';
        const dlBtn = document.getElementById('btn-download-page');
        if (dlBtn) {
          dlBtn.textContent = plCheck.checked ? '📑 Download Playlist Videos' : '🎬 Download Current Page Video';
        }
      });
    }

    // Download Current Page
    document.getElementById('btn-download-page').addEventListener('click', () => {
      if (!activeTab || !activeTab.url) return;
      const quality = document.getElementById('select-quality').value;
      const videoCodec = document.getElementById('select-video-codec')?.value || 'h264';
      const speedMode = document.getElementById('select-speed-mode')?.value || 'turbo';
      const playlist = document.getElementById('check-playlist')?.checked || false;
      const plStart = parseInt(document.getElementById('input-pl-start')?.value || '1', 10) || 1;
      const plCount = parseInt(document.getElementById('input-pl-count')?.value || '0', 10) || 0;
      sendDownload(activeTab.url, activeTab.title, 'ytdlp', quality, playlist, plStart, plCount, speedMode, videoCodec);
    });

    // Downloads Tab filters, search, clear done & global speed limit
    document.querySelectorAll('.dl-filter-btn').forEach((btn) => {
      btn.addEventListener('click', () => {
        document.querySelectorAll('.dl-filter-btn').forEach((b) => b.classList.remove('active'));
        btn.classList.add('active');
        dlFilter = btn.dataset.dlFilter || 'all';
        renderDownloadsView();
      });
    });

    document.getElementById('dl-search-input')?.addEventListener('input', (e) => {
      dlSearch = e.target.value || '';
      renderDownloadsView();
    });

    document.getElementById('btn-popup-clear-done')?.addEventListener('click', (e) => {
      sendTaskAction('clear_completed', '', e.currentTarget);
    });

    document.getElementById('select-global-limit')?.addEventListener('change', (e) => {
      const kbps = Math.max(0, parseInt(e.target.value, 10) || 0);
      chrome.runtime.sendMessage(
        { type: 'UPDATE_DAEMON_CONFIG', payload: { speedLimitKBps: kbps } },
        (res) => {
          if (chrome.runtime.lastError || !res || !res.success) {
            showAlert('Error updating speed cap: ' + (res?.error || 'daemon offline'), true);
            return;
          }
          showAlert(
            kbps > 0 ? `Speed cap set to ${formatBytes(kbps * 1024)}/s` : 'Global speed limit removed',
            false
          );
        }
      );
    });

    // Settings Tab controls
    document.getElementById('check-intercept').addEventListener('change', async (e) => {
      await chrome.storage.local.set({ interceptDownloads: e.target.checked });
      showAlert('Download interception ' + (e.target.checked ? 'enabled' : 'disabled'), false);
    });

    document.getElementById('select-when-done')?.addEventListener('change', (e) => {
      const action = e.target.value || 'none';
      chrome.runtime.sendMessage({ type: 'SET_WHEN_DONE', action }, (res) => {
        if (chrome.runtime.lastError || !res || !res.success) {
          showAlert('Error setting post-download action: ' + (res?.error || 'daemon offline'), true);
          return;
        }
        const applied = res.data?.whenDoneAction || action;
        e.target.value = applied;
        showAlert(
          applied === 'none'
            ? 'Post-download action cleared'
            : `When done set: ${applied.replace('_', ' ')}`,
          false
        );
      });
    });

    const triggerYtDlpUpdate = () => {
      renderPopupYtDlpStatus({
        updating: true,
        statusText: 'Starting yt-dlp update...',
        progress: 5,
      });
      chrome.runtime.sendMessage({ type: 'UPDATE_YTDLP', force: false }, (res) => {
        if (chrome.runtime.lastError || !res || !res.success) {
          showAlert('yt-dlp update error: ' + (res?.error || 'daemon offline'), true);
          refreshDaemonSettings();
          return;
        }
        if (res.data?.info) {
          renderPopupYtDlpStatus(res.data.info);
        } else {
          refreshDaemonSettings();
        }
        showAlert(
          res.data?.version ? `yt-dlp updated to v${res.data.version}!` : 'yt-dlp updated!',
          false
        );
      });
    };

    document.getElementById('btn-popup-check-ytdlp')?.addEventListener('click', (e) => {
      const btn = e.currentTarget;
      btn.disabled = true;
      btn.textContent = 'Checking...';
      chrome.runtime.sendMessage({ type: 'CHECK_YTDLP_UPDATE', force: true }, (res) => {
        btn.disabled = false;
        btn.textContent = '🔍 Check Update';
        if (chrome.runtime.lastError || !res || !res.success || !res.info) {
          showAlert('Update check failed: ' + (res?.error || 'daemon offline'), true);
          return;
        }
        renderPopupYtDlpStatus(res.info);
        if (res.info.updateAvailable) {
          showAlert(`yt-dlp update found: ${res.info.latestVersion}`, false);
        } else {
          showAlert(`yt-dlp is up to date (${res.info.currentVersion || res.info.latestVersion})`, false);
        }
      });
    });

    document.getElementById('btn-popup-update-ytdlp')?.addEventListener('click', triggerYtDlpUpdate);
    document.getElementById('btn-popup-banner-update-ytdlp')?.addEventListener('click', triggerYtDlpUpdate);

    document.getElementById('btn-popup-sync-cookies')?.addEventListener('click', (e) => {
      const btn = e.currentTarget;
      btn.disabled = true;
      btn.textContent = 'Syncing...';
      chrome.runtime.sendMessage(
        { type: 'SYNC_COOKIES_NOW', storeId: activeTab?.cookieStoreId || null },
        (res) => {
          btn.disabled = false;
          btn.textContent = '🔄 Sync Latest Cookies Now';
          if (chrome.runtime.lastError || !res || !res.success) {
            showAlert('Cookie sync failed: ' + (res?.error || 'daemon offline'), true);
            return;
          }
          renderPopupCookiesStatus(Boolean(res.signedIn), res.count || 0);
          showAlert(
            res.signedIn
              ? `Synced ${res.count || 0} signed-in YouTube cookies!`
              : 'No signed-in YouTube cookies found in this browser',
            !res.signedIn
          );
        }
      );
    });

    document.getElementById('btn-save-token').addEventListener('click', async () => {
      const nextUrl = (document.getElementById('input-daemon-url')?.value || 'http://127.0.0.1:17890')
        .trim()
        .replace(/\/+$/, '');
      const nextToken = document.getElementById('input-token').value.trim();
      daemonUrl = nextUrl || 'http://127.0.0.1:17890';
      daemonToken = nextToken;
      await chrome.storage.local.set({ daemonUrl, authToken: nextToken });
      showAlert('Settings saved! Reconnecting...', false);
      checkDaemonStatus();
    });

    // Open GrabAll Window (or Web Dashboard fallback on Linux/macOS)
    document.getElementById('btn-open-dashboard').addEventListener('click', () => {
      chrome.runtime.sendMessage({ type: 'OPEN_GRABALL_APP' }, async (res) => {
        if (res && res.nativeGui) {
          window.close();
        } else {
          const cfg = await chrome.storage.local.get({ daemonUrl: 'http://127.0.0.1:17890' });
          chrome.tabs.create({ url: res?.daemonUrl || cfg.daemonUrl });
        }
      });
    });
  }

  function sendDownload(url, title, engine, quality, playlist = false, playlistStart = 1, playlistCount = 0, speedMode = 'turbo', videoCodec = 'h264') {
    showAlert(playlist ? 'Scanning playlist...' : 'Sending to GrabAll...', false);
    const embedSubs = document.getElementById('check-embed-subs')?.checked || false;
    const subLangs = (document.getElementById('input-sub-langs')?.value || 'en').trim() || 'en';
    const embedChapters = document.getElementById('check-embed-chapters')?.checked || false;

    chrome.runtime.sendMessage(
      {
        type: 'SEND_DOWNLOAD',
        payload: {
          url: url,
          pageUrl: activeTab?.url,
          referer: activeTab?.url,
          title: title || '',
          engine: engine || 'auto',
          quality: quality || 'best',
          videoCodec: videoCodec || 'h264',
          speedMode: speedMode || 'turbo',
          playlist: Boolean(playlist),
          playlistStart: playlist ? playlistStart : 0,
          playlistCount: playlist ? playlistCount : 0,
          embedSubs: Boolean(embedSubs),
          subLangs: subLangs,
          embedChapters: Boolean(embedChapters),
          storeId: activeTab?.cookieStoreId || null,
        },
      },
      (res) => {
        if (chrome.runtime.lastError || !res || !res.success) {
          const msg = res?.error || chrome.runtime.lastError?.message || 'Failed to connect to daemon';
          showAlert('Error: ' + msg, true);
          return;
        }
        if (res.data?.playlist) {
          showAlert(`Queued ${res.data.count} videos from playlist!`, false);
        } else {
          showAlert('Download queued successfully!', false);
        }
        fetchQueueOnce();
        switchTab('downloads');
      }
    );
  }

  function showAlert(message, isError) {
    const alert = document.getElementById('status-alert');
    alert.textContent = message;
    alert.className = 'alert ' + (isError ? 'alert-danger' : 'alert-success');
    alert.style.display = 'block';

    setTimeout(() => {
      alert.style.display = 'none';
    }, 4000);
  }

  document.addEventListener('DOMContentLoaded', async () => {
    const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
    activeTab = tab;

    await loadConfig();
    await checkDaemonStatus();
    await loadDetectedMedia();

    initListeners();
  });

  window.addEventListener('unload', cleanupLiveConnections);
}
