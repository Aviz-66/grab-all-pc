// GrabAll Web Dashboard App

let tasks = [];
let currentFilter = 'all';
let currentCategory = 'all';
let searchQuery = '';
let sseConnection = null;
let defaultVideoCodec = 'h264';
let latestUpdateInfo = null;
let autoCheckUpdatesEnabled = true;

// DOM Elements
const queueListEl = document.getElementById('queue-list');
const queueEmptyEl = document.getElementById('queue-empty');
const queueHeaderEl = document.getElementById('queue-header');
const activeBadgeEl = document.getElementById('active-count-badge');
const speedBadgeEl = document.getElementById('speed-badge');

const countAllEl = document.getElementById('count-all');
const countDownloadingEl = document.getElementById('count-downloading');
const countQueuedEl = document.getElementById('count-queued');
const countCompletedEl = document.getElementById('count-completed');
const countErrorEl = document.getElementById('count-error');

// Sidebar counts (ids must match index.html)
const sideCountEls = {
  all: document.getElementById('count-side-all'),
  active: document.getElementById('count-side-active'),
  video: document.getElementById('count-side-videos'),
  audio: document.getElementById('count-side-audio'),
  archive: document.getElementById('count-side-archives'),
  program: document.getElementById('count-side-programs'),
  document: document.getElementById('count-side-documents'),
  image: document.getElementById('count-side-images'),
  completed: document.getElementById('count-side-completed'),
  failed: document.getElementById('count-side-failed'),
};

// Status bar (ids must match index.html)
const sbActiveEl = document.getElementById('sb-active');
const sbQueuedEl = document.getElementById('sb-queued');
const sbCompletedEl = document.getElementById('sb-completed');
const sbSpeedEl = document.getElementById('sb-speed');
const sbFolderEl = document.getElementById('sb-folder');
const sbYtdlpEl = document.getElementById('sb-ytdlp');
const sbFfmpegEl = document.getElementById('sb-ffmpeg');

const filterTabs = document.querySelectorAll('.filter-tab');
const sideItems = document.querySelectorAll('.side-item');
const toastEl = document.getElementById('toast');

// Modals
const modalAdd = document.getElementById('modal-add');
const modalSettings = document.getElementById('modal-settings');
const formAdd = document.getElementById('form-add');
const formSettings = document.getElementById('form-settings');

// Initialize
document.addEventListener('DOMContentLoaded', async () => {
  initSSE();
  initFilters();
  initModals();
  initSettings();
  await loadSettings();
  if (autoCheckUpdatesEnabled) {
    checkForUpdates(false);
  }
});

let fallbackPollTimer = null;

function stopFallbackPolling() {
  if (fallbackPollTimer) {
    clearInterval(fallbackPollTimer);
    fallbackPollTimer = null;
  }
}

// SSE Real-time updates
function initSSE() {
  if (sseConnection) {
    sseConnection.close();
  }

  sseConnection = new EventSource('/api/events');

  sseConnection.addEventListener('queue_init', (e) => {
    stopFallbackPolling();
    try {
      tasks = JSON.parse(e.data);
      renderQueue();
    } catch (err) {
      console.error('Failed to parse queue_init', err);
    }
  });

  sseConnection.addEventListener('queue_update', (e) => {
    stopFallbackPolling();
    try {
      tasks = JSON.parse(e.data);
      renderQueue();
    } catch (err) {
      console.error('Failed to parse queue_update', err);
    }
  });

  sseConnection.onerror = () => {
    if (!fallbackPollTimer) {
      fetchQueue();
      fallbackPollTimer = setInterval(fetchQueue, 1000);
    }
  };
}

async function fetchQueue() {
  try {
    const res = await fetch('/api/queue');
    if (res.ok) {
      tasks = await res.json();
      renderQueue();
    }
  } catch (err) {
    console.error('Fetch queue error', err);
  }
}

// Filters
function setFilter(status, category) {
  currentFilter = status;
  currentCategory = category;
  filterTabs.forEach((t) => t.classList.toggle('active', t.dataset.filter === status));
  sideItems.forEach((it) => {
    it.classList.toggle('active', it.dataset.status === status && it.dataset.category === category);
  });
  renderQueue();
}

function initFilters() {
  filterTabs.forEach((tab) => {
    tab.addEventListener('click', () => {
      // Top tabs filter by status only (category resets to All)
      setFilter(tab.dataset.filter, 'all');
    });
  });

  sideItems.forEach((item) => {
    item.addEventListener('click', () => {
      setFilter(item.dataset.status, item.dataset.category);
    });
  });

  document.getElementById('btn-clear-completed').addEventListener('click', async () => {
    try {
      const res = await fetch('/api/clear-completed', { method: 'POST' });
      if (res.ok) {
        showToast('Cleared completed downloads');
      }
    } catch (err) {
      showToast('Failed to clear completed', true);
    }
  });

  const searchInputEl = document.getElementById('search-input');
  if (searchInputEl) {
    searchInputEl.addEventListener('input', (e) => {
      searchQuery = e.target.value.trim().toLowerCase();
      renderQueue();
    });
  }

  const whenDoneEl = document.getElementById('when-done-select');
  if (whenDoneEl) {
    whenDoneEl.addEventListener('change', async (e) => {
      const action = e.target.value;
      try {
        const res = await fetch('/api/when-done', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ action }),
        });
        if (res.ok) {
          const data = await res.json();
          whenDoneEl.value = data.whenDoneAction || 'none';
          showToast(
            data.whenDoneAction && data.whenDoneAction !== 'none'
              ? `When done set: ${data.whenDoneAction.replace('_', ' ')}`
              : 'Post-download action cleared'
          );
        } else {
          showToast('Failed to update post-download action', true);
        }
      } catch (err) {
        showToast('Failed to update post-download action', true);
      }
    });
  }
}

// Media category of a task (drives the sidebar Library categories)
function taskCategory(task) {
  const isAudio = task.quality && String(task.quality).startsWith('audio');
  if (isAudio) return 'audio';
  const name = String(task.fileName || task.url || '').toLowerCase();
  if (/\.(mp4|webm|mkv|mov|avi|m3u8|mpd)$/i.test(name) || task.engine === 'ytdlp') return 'video';
  if (/\.(zip|rar|7z|tar|gz|tgz|iso|img)$/i.test(name)) return 'archive';
  if (/\.(exe|msi|apk|dmg|pkg)$/i.test(name)) return 'program';
  if (/\.(pdf|docx?|txt|epub|xlsx?|pptx?)$/i.test(name)) return 'document';
  if (/\.(jpe?g|png|gif|webp|bmp|svg|avif)$/i.test(name)) return 'image';
  return 'other';
}

function updateCounts() {
  let all = tasks.length;
  let downloading = 0;
  let queued = 0;
  let completed = 0;
  let error = 0;
  const byCategory = { video: 0, audio: 0, archive: 0, program: 0, document: 0, image: 0 };

  for (const t of tasks) {
    if (t.status === 'downloading') downloading++;
    else if (t.status === 'queued') queued++;
    else if (t.status === 'completed') completed++;
    else if (t.status === 'error' || t.status === 'canceled') error++;
    const cat = taskCategory(t);
    if (byCategory[cat] !== undefined) byCategory[cat]++;
  }

  countAllEl.textContent = all;
  countDownloadingEl.textContent = downloading;
  countQueuedEl.textContent = queued;
  countCompletedEl.textContent = completed;
  countErrorEl.textContent = error;

  if (sideCountEls.all) sideCountEls.all.textContent = all;
  if (sideCountEls.active) sideCountEls.active.textContent = downloading + queued;
  if (sideCountEls.video) sideCountEls.video.textContent = byCategory.video;
  if (sideCountEls.audio) sideCountEls.audio.textContent = byCategory.audio;
  if (sideCountEls.archive) sideCountEls.archive.textContent = byCategory.archive;
  if (sideCountEls.program) sideCountEls.program.textContent = byCategory.program;
  if (sideCountEls.document) sideCountEls.document.textContent = byCategory.document;
  if (sideCountEls.image) sideCountEls.image.textContent = byCategory.image;
  if (sideCountEls.completed) sideCountEls.completed.textContent = completed;
  if (sideCountEls.failed) sideCountEls.failed.textContent = error;

  if (sbActiveEl) sbActiveEl.textContent = downloading;
  if (sbQueuedEl) sbQueuedEl.textContent = queued;
  if (sbCompletedEl) sbCompletedEl.textContent = completed;

  activeBadgeEl.textContent = `${downloading} Active`;

  // Aggregate active speeds
  const activeSpeeds = tasks
    .filter((t) => t.status === 'downloading' && t.speed)
    .map((t) => t.speed);

  const speedText = activeSpeeds.length > 0 ? activeSpeeds.join(', ') : '0 KB/s';
  speedBadgeEl.textContent = speedText;
  if (sbSpeedEl) sbSpeedEl.textContent = speedText;
}

function renderSegmentMapHtml(task) {
  if (
    task.status !== 'downloading' ||
    !Array.isArray(task.segments) ||
    task.segments.length === 0 ||
    !(task.totalBytes > 0)
  ) {
    return '';
  }
  const activeCount = task.segments.filter((s) => s.active).length;
  const blocks = task.segments
    .map((seg) => {
      const start = seg.start || 0;
      const cur = Math.min(seg.current || 0, (seg.end || 0) + 1);
      const done = Math.max(0, cur - start);
      if (done <= 0) return '';
      const leftPct = Math.max(0, Math.min(100, (start / task.totalBytes) * 100));
      const widthPct = Math.max(0.2, Math.min(100 - leftPct, (done / task.totalBytes) * 100));
      const cls = seg.active ? 'segment-map-fill active' : 'segment-map-fill';
      return `<span class="${cls}" style="left:${leftPct.toFixed(2)}%;width:${widthPct.toFixed(2)}%;"></span>`;
    })
    .join('');
  if (!blocks) return '';
  return `<div class="segment-map-bar" title="Connections: ${activeCount} active / ${task.segments.length} segments">${blocks}</div>`;
}

function renderQueue() {
  updateCounts();

  const searchTokens = searchQuery ? searchQuery.split(/\s+/).filter(Boolean) : [];
  const filtered = tasks.filter((t) => {
    let statusOk = true;
    if (currentFilter === 'downloading') statusOk = t.status === 'downloading';
    else if (currentFilter === 'queued') statusOk = t.status === 'queued';
    else if (currentFilter === 'active') statusOk = t.status === 'downloading' || t.status === 'queued';
    else if (currentFilter === 'completed') statusOk = t.status === 'completed';
    else if (currentFilter === 'error') statusOk = t.status === 'error' || t.status === 'canceled';
    if (!statusOk) return false;

    if (currentCategory !== 'all' && taskCategory(t) !== currentCategory) return false;

    if (searchTokens.length === 0) return true;
    const haystack = [
      t.title || '',
      t.fileName || '',
      t.url || '',
      t.subDir || '',
      t.quality || '',
      t.status || '',
      t.errorMessage || '',
    ].join('\n').toLowerCase();
    return searchTokens.every((tok) => haystack.includes(tok));
  });

  if (queueHeaderEl) {
    queueHeaderEl.style.display = filtered.length === 0 ? 'none' : 'grid';
  }

  if (filtered.length === 0) {
    queueEmptyEl.style.display = 'block';
    queueListEl.innerHTML = '';
    return;
  }

  queueEmptyEl.style.display = 'none';

  // Build card elements or update existing
  const fragment = document.createDocumentFragment();

  filtered.forEach((task) => {
    let existingCard = document.getElementById(`task-${task.id}`);
    if (existingCard) {
      updateTaskCard(existingCard, task);
      fragment.appendChild(existingCard);
    } else {
      const card = createTaskCard(task);
      fragment.appendChild(card);
    }
  });

  queueListEl.innerHTML = '';
  queueListEl.appendChild(fragment);
}

function createTaskCard(task) {
  const card = document.createElement('div');
  card.className = 'task-card';
  card.id = `task-${task.id}`;
  card.dataset.status = task.status;

  const isAudio = task.quality && task.quality.startsWith('audio');
  const isVideo = !isAudio && (task.engine === 'ytdlp' || /\.(mp4|webm|mkv|m3u8|mpd)$/i.test(task.fileName || task.url));
  const icon = isAudio ? '🎵' : (isVideo ? '🎬' : '📄');

  card.innerHTML = `
    <div class="task-header">
      <div class="task-title-group">
        <span class="task-icon">${icon}</span>
        <div class="task-title-wrap">
          <div class="task-title" title="${escapeHtml(task.title || task.url)}">${escapeHtml(task.title || task.fileName || task.url)}</div>
          <div class="task-meta">
            <span class="engine-tag">${escapeHtml(task.engine)}</span>
            ${task.engine === 'ytdlp' && !isAudio && task.videoCodec ? `<span class="engine-tag">${escapeHtml(codecLabel(task.videoCodec))}</span>` : ''}
            <span class="task-url" style="max-width:300px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;">${escapeHtml(task.url)}</span>
          </div>
        </div>
      </div>
    </div>

    <span class="stat-size">${task.totalBytes > 0 ? formatBytes(task.totalBytes) : '—'}</span>

    <div class="progress-container">
      <div class="progress-head">
        <span class="task-status-badge status-${task.status}">${task.status}</span>
        <span class="stat-progress"><strong>${task.progress.toFixed(1)}%</strong></span>
      </div>
      <div class="progress-bar-bg">
        <div class="progress-bar-fill ${task.status}" style="width: ${task.progress.toFixed(1)}%;"></div>
      </div>
      <div class="segment-map-container">${renderSegmentMapHtml(task)}</div>
      <span class="stat-downloaded">${task.totalBytes > 0 ? `${formatBytes(task.downloadedBytes)} / ${formatBytes(task.totalBytes)}` : formatBytes(task.downloadedBytes)}</span>
    </div>

    <span class="stat-speed">${task.speed ? `⚡ ${escapeHtml(task.speed)}` : '—'}</span>
    <span class="stat-eta">${task.eta ? `⏱️ ${escapeHtml(task.eta)}` : '—'}</span>
    <span class="stat-quality">${task.quality ? escapeHtml(qualityLabel(task.quality)) : '—'}</span>

    <div class="task-actions">
      ${getTaskButtons(task)}
    </div>

    <div class="task-error-text" style="${task.errorMessage ? '' : 'display:none;'}">⚠️ ${escapeHtml(task.errorMessage || '')}</div>
  `;

  attachTaskActionEvents(card, task);
  return card;
}

function updateTaskCard(card, task) {
  const titleEl = card.querySelector('.task-title');
  if (titleEl) {
    const newTitle = task.title || task.fileName || task.url;
    titleEl.textContent = newTitle;
    titleEl.title = newTitle;
  }

  const errEl = card.querySelector('.task-error-text');
  if (errEl) {
    if (task.errorMessage) {
      errEl.style.display = 'block';
      errEl.textContent = `⚠️ ${task.errorMessage}`;
    } else {
      errEl.style.display = 'none';
      errEl.textContent = '';
    }
  }

  const badge = card.querySelector('.task-status-badge');
  if (badge) {
    badge.className = `task-status-badge status-${task.status}`;
    badge.textContent = task.status;
  }

  const fill = card.querySelector('.progress-bar-fill');
  if (fill) {
    fill.className = `progress-bar-fill ${task.status}`;
    fill.style.width = `${task.progress.toFixed(1)}%`;
  }

  const segContainer = card.querySelector('.segment-map-container');
  if (segContainer) {
    segContainer.innerHTML = renderSegmentMapHtml(task);
  }

  const statProgress = card.querySelector('.stat-progress');
  if (statProgress) {
    statProgress.innerHTML = `<strong>${task.progress.toFixed(1)}%</strong>`;
  }

  const statSize = card.querySelector('.stat-size');
  if (statSize) {
    statSize.textContent = task.totalBytes > 0 ? formatBytes(task.totalBytes) : '—';
  }

  const statDownloaded = card.querySelector('.stat-downloaded');
  if (statDownloaded) {
    statDownloaded.textContent = task.totalBytes > 0 ? `${formatBytes(task.downloadedBytes)} / ${formatBytes(task.totalBytes)}` : formatBytes(task.downloadedBytes);
  }

  const statSpeed = card.querySelector('.stat-speed');
  if (statSpeed) {
    statSpeed.textContent = task.speed ? `⚡ ${task.speed}` : '—';
  }

  const statEta = card.querySelector('.stat-eta');
  if (statEta) {
    statEta.textContent = task.eta ? `⏱️ ${task.eta}` : '—';
  }

  const modeKey = `${task.status}|${task.speedMode || ''}|${task.speedLimitKBps || 0}`;
  if (card.dataset.status !== modeKey) {
    card.dataset.status = modeKey;
    const actions = card.querySelector('.task-actions');
    if (actions) {
      actions.innerHTML = getTaskButtons(task);
      attachTaskActionEvents(card, task);
    }
  }
}

function getTaskButtons(task) {
  let buttons = '';
  if (task.status !== 'completed') {
    const isRegular = task.speedMode === 'regular';
    buttons += isRegular
      ? `<button class="btn btn-sm btn-secondary btn-action-mode" data-next="turbo" title="Switch to 6x Turbo Multi-Connection Speed">⚡ Turbo</button>`
      : `<button class="btn btn-sm btn-secondary btn-action-mode" data-next="regular" title="Switch to Regular 1-Stream Mode (Watch While Downloading)">🎬 Live</button>`;
    const limKBps = Number(task.speedLimitKBps) || 0;
    const limLabel = limKBps > 0 ? `🚦 ${formatBytes(limKBps * 1024)}/s` : '🚦 Limit';
    buttons += `<button class="btn btn-sm btn-secondary btn-action-limit" title="Set Download Speed Limit (KB/s)">${limLabel}</button>`;
  }
  if (task.status === 'downloading') {
    buttons += `<button class="btn btn-sm btn-secondary btn-action-pause" title="Pause">⏸️ Pause</button>`;
    buttons += `<button class="btn btn-sm btn-secondary btn-action-cancel" title="Cancel">⏹️ Cancel</button>`;
  } else if (task.status === 'paused' || task.status === 'error' || task.status === 'canceled') {
    buttons += `<button class="btn btn-sm btn-primary btn-action-resume" title="Resume">▶️ Resume</button>`;
  } else if (task.status === 'completed') {
    buttons += `<button class="btn btn-sm btn-primary btn-action-open-file" title="Open Downloaded File">▶️ Open</button>`;
    buttons += `<button class="btn btn-sm btn-secondary btn-action-folder" title="Show in Folder">📂 Folder</button>`;
  }

  buttons += `<button class="btn btn-sm btn-ghost btn-action-delete" title="Delete">🗑️</button>`;
  return buttons;
}

function attachTaskActionEvents(card, task) {
  const btnMode = card.querySelector('.btn-action-mode');
  if (btnMode) {
    btnMode.onclick = () => {
      const next = btnMode.getAttribute('data-next') || 'regular';
      doAction('/api/speed-mode', { id: task.id, speedMode: next });
      showToast(next === 'regular' ? 'Switched to Regular Mode (Watch Live)' : 'Switched to 6x Turbo Speed');
    };
  }

  const btnLimit = card.querySelector('.btn-action-limit');
  if (btnLimit) {
    btnLimit.onclick = () => {
      const curLim = Number(task.speedLimitKBps) || 0;
      const entered = prompt('Enter speed limit in KB/s for this download (0 = Unlimited, e.g. 1024 for 1 MB/s):', String(curLim));
      if (entered === null) return;
      const parsed = Math.max(0, parseInt(entered.trim(), 10) || 0);
      doAction('/api/speed-limit', { id: task.id, speedLimitKBps: parsed });
      showToast(parsed > 0 ? `Task limit set to ${formatBytes(parsed * 1024)}/s` : 'Task speed limit removed');
    };
  }

  const btnPause = card.querySelector('.btn-action-pause');
  if (btnPause) {
    btnPause.onclick = () => doAction('/api/pause', { id: task.id });
  }

  const btnResume = card.querySelector('.btn-action-resume');
  if (btnResume) {
    btnResume.onclick = () => doAction('/api/resume', { id: task.id });
  }

  const btnCancel = card.querySelector('.btn-action-cancel');
  if (btnCancel) {
    btnCancel.onclick = () => doAction('/api/cancel', { id: task.id });
  }

  const btnOpenFile = card.querySelector('.btn-action-open-file');
  if (btnOpenFile) {
    btnOpenFile.onclick = () => doAction('/api/open-file', { id: task.id });
  }

  const btnFolder = card.querySelector('.btn-action-folder');
  if (btnFolder) {
    btnFolder.onclick = () => doAction('/api/open-folder', { id: task.id });
  }

  const btnDelete = card.querySelector('.btn-action-delete');
  if (btnDelete) {
    btnDelete.onclick = () => {
      const deleteFile = confirm('Delete downloaded file from disk as well?');
      doAction('/api/delete', { id: task.id, deleteFile });
    };
  }
}

async function doAction(url, body) {
  try {
    const res = await fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (!res.ok) {
      showToast('Action failed', true);
    }
  } catch (err) {
    showToast('Network error', true);
  }
}

// Modals Setup
function initModals() {
  const qualitySelect = document.getElementById('input-quality');
  const origQualityLabels = new Map();
  if (qualitySelect) {
    Array.from(qualitySelect.options).forEach((opt) => {
      origQualityLabels.set(opt.value, opt.textContent);
    });
  }

  function resetPreviewUI() {
    const box = document.getElementById('add-preview-box');
    if (box) {
      box.classList.add('hidden');
      box.innerHTML = '';
    }
    if (qualitySelect) {
      Array.from(qualitySelect.options).forEach((opt) => {
        if (origQualityLabels.has(opt.value)) {
          opt.textContent = origQualityLabels.get(opt.value);
        }
      });
    }
  }

  document.getElementById('btn-add-modal').addEventListener('click', () => {
    const codecSelect = document.getElementById('input-video-codec');
    if (codecSelect) {
      codecSelect.value = defaultVideoCodec || 'h264';
    }
    resetPreviewUI();
    modalAdd.classList.remove('hidden');
    document.getElementById('input-url').focus();
  });

  document.getElementById('btn-preview-url')?.addEventListener('click', async () => {
    const url = document.getElementById('input-url').value.trim();
    const videoCodec = document.getElementById('input-video-codec')?.value || defaultVideoCodec || 'h264';
    const box = document.getElementById('add-preview-box');
    const btn = document.getElementById('btn-preview-url');
    if (!url || !box) {
      showToast('Paste a video URL first', true);
      return;
    }

    if (btn) {
      btn.disabled = true;
      btn.textContent = 'Checking...';
    }
    box.classList.remove('hidden');
    box.innerHTML = '<div class="preview-meta">Fetching video quality & size metadata...</div>';

    try {
      const res = await fetch('/api/media/preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ url, videoCodec }),
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok || !data.preview) {
        box.innerHTML = `<div class="preview-meta" style="color:var(--danger);">${escapeHtml(data.error || 'Preview unavailable for this URL')}</div>`;
        return;
      }

      const prev = data.preview;
      const sizeMap = new Map();
      (prev.options || []).forEach((opt) => {
        sizeMap.set(opt.quality, opt);
      });

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

      const metaParts = [];
      if (prev.uploader) metaParts.push(prev.uploader);
      if (prev.durationText) metaParts.push(`⏱️ ${prev.durationText}`);
      if (Array.isArray(prev.chapters) && prev.chapters.length > 0) {
        metaParts.push(`🏷️ ${prev.chapters.length} chapters`);
      }
      const subCount = (prev.subtitleLangs?.length || 0) + (prev.autoSubtitleLangs?.length || 0);
      if (subCount > 0) {
        metaParts.push(`💬 Subtitles available`);
      }

      const curQual = qualitySelect ? qualitySelect.value : 'best';
      const pillsHtml = (prev.options || [])
        .map((opt) => {
          const sz = opt.sizeBytes > 0 ? `${opt.approx ? '~' : ''}${formatBytes(opt.sizeBytes)}` : 'size N/A';
          const codecTag = opt.videoCodec ? ` • ${opt.videoCodec}` : '';
          const activeCls = opt.quality === curQual ? ' active' : '';
          return `<button type="button" class="preview-pill${activeCls}" data-q="${escapeHtml(opt.quality)}">${escapeHtml(opt.label)} — <strong>${escapeHtml(sz)}</strong>${escapeHtml(codecTag)}</button>`;
        })
        .join('');

      box.innerHTML = `
        <div class="preview-title">${escapeHtml(prev.title || url)}</div>
        <div class="preview-meta">${escapeHtml(metaParts.join('  •  '))}</div>
        ${pillsHtml ? `<div class="preview-pills">${pillsHtml}</div>` : ''}
      `;

      box.querySelectorAll('.preview-pill').forEach((pill) => {
        pill.addEventListener('click', () => {
          const q = pill.getAttribute('data-q') || 'best';
          if (qualitySelect) {
            qualitySelect.value = q;
          }
          box.querySelectorAll('.preview-pill').forEach((p) => p.classList.remove('active'));
          pill.classList.add('active');
        });
      });
    } catch (err) {
      box.innerHTML = '<div class="preview-meta" style="color:var(--danger);">Network error while fetching preview</div>';
    } finally {
      if (btn) {
        btn.disabled = false;
        btn.textContent = '🔍 Preview Size';
      }
    }
  });

  document.querySelectorAll('[data-close]').forEach((btn) => {
    btn.addEventListener('click', () => {
      const targetId = btn.dataset.close;
      document.getElementById(targetId).classList.add('hidden');
    });
  });

  formAdd.addEventListener('submit', async (e) => {
    e.preventDefault();
    const url = document.getElementById('input-url').value.trim();
    const engine = document.getElementById('input-engine').value;
    const quality = document.getElementById('input-quality').value;
    const videoCodec = document.getElementById('input-video-codec')?.value || defaultVideoCodec || 'h264';
    const fileName = document.getElementById('input-filename').value.trim();
    const playlist = document.getElementById('input-playlist')?.checked || false;
    const embedSubs = document.getElementById('input-embed-subs')?.checked || false;
    const subLangs = (document.getElementById('input-sub-langs')?.value || 'en').trim() || 'en';
    const embedChapters = document.getElementById('input-embed-chapters')?.checked || false;

    if (!url) return;

    const submitBtn = document.getElementById('btn-submit-add');
    if (submitBtn) {
      submitBtn.disabled = true;
      submitBtn.textContent = playlist ? 'Scanning Playlist...' : 'Adding...';
    }

    try {
      const res = await fetch('/api/add', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          url,
          engine,
          quality,
          videoCodec,
          fileName,
          playlist,
          embedSubs,
          subLangs,
          embedChapters,
        }),
      });
      const data = await res.json();
      if (res.ok) {
        if (data.playlist) {
          showToast(`Queued ${data.count} videos from "${data.playlistTitle || 'Playlist'}"!`);
        } else {
          showToast('Download added!');
        }
        modalAdd.classList.add('hidden');
        formAdd.reset();
        resetPreviewUI();
      } else {
        showToast(data.error || 'Failed to add download', true);
      }
    } catch (err) {
      showToast('Error adding download', true);
    } finally {
      if (submitBtn) {
        submitBtn.disabled = false;
        submitBtn.textContent = 'Start Download';
      }
    }
  });
}

// Settings
function initSettings() {
  const btnSettings = document.getElementById('btn-settings-modal');
  btnSettings.addEventListener('click', async () => {
    await loadSettings();
    modalSettings.classList.remove('hidden');
  });

  formSettings.addEventListener('submit', async (e) => {
    e.preventDefault();
    const downloadDir = document.getElementById('setting-download-dir').value.trim();
    const maxConcurrent = parseInt(document.getElementById('setting-max-concurrent').value, 10);
    const defaultEngine = document.getElementById('setting-default-engine').value;
    const videoCodec = document.getElementById('setting-video-codec')?.value || 'h264';
    const speedLimitKBps = Math.max(0, parseInt(document.getElementById('setting-speed-limit')?.value || '0', 10) || 0);
    const autoCheckUpdates = document.getElementById('setting-auto-update')?.checked !== false;

    try {
      const res = await fetch('/api/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ downloadDir, maxConcurrent, defaultEngine, videoCodec, speedLimitKBps, autoCheckUpdates }),
      });
      if (res.ok) {
        defaultVideoCodec = videoCodec;
        autoCheckUpdatesEnabled = autoCheckUpdates;
        showToast('Settings saved successfully');
        modalSettings.classList.add('hidden');
      } else {
        showToast('Failed to save settings', true);
      }
    } catch (err) {
      showToast('Error saving settings', true);
    }
  });

  document.getElementById('btn-copy-token').addEventListener('click', () => {
    const token = document.getElementById('setting-token').value;
    navigator.clipboard.writeText(token).then(() => {
      showToast('Auth token copied to clipboard!');
    });
  });

  document.getElementById('btn-install-ytdlp').addEventListener('click', async () => {
    const btn = document.getElementById('btn-install-ytdlp');
    btn.disabled = true;
    btn.textContent = 'Downloading yt-dlp...';

    try {
      const res = await fetch('/api/install-ytdlp', { method: 'POST' });
      const data = await res.json();
      if (res.ok) {
        showToast('yt-dlp (+ QuickJS) installed successfully!');
        await loadSettings();
      } else {
        showToast(data.error || 'Failed to install yt-dlp', true);
      }
    } catch (err) {
      showToast('Error installing yt-dlp', true);
    } finally {
      btn.disabled = false;
      btn.textContent = 'Install / Update yt-dlp';
    }
  });

  document.getElementById('btn-install-ffmpeg').addEventListener('click', async () => {
    const btn = document.getElementById('btn-install-ffmpeg');
    btn.disabled = true;
    btn.textContent = 'Downloading FFmpeg...';

    try {
      const res = await fetch('/api/install-ffmpeg', { method: 'POST' });
      const data = await res.json();
      if (res.ok) {
        showToast('FFmpeg installed successfully!');
        await loadSettings();
      } else {
        showToast(data.error || 'Failed to install FFmpeg', true);
      }
    } catch (err) {
      showToast('Error installing FFmpeg', true);
    } finally {
      btn.disabled = false;
      btn.textContent = 'Install FFmpeg';
    }
  });

  document.getElementById('btn-check-update')?.addEventListener('click', () => {
    checkForUpdates(true);
  });

  document.getElementById('btn-apply-update')?.addEventListener('click', () => {
    applyAppUpdate();
  });

  document.getElementById('btn-banner-update')?.addEventListener('click', () => {
    applyAppUpdate();
  });

  document.getElementById('btn-banner-dismiss')?.addEventListener('click', () => {
    document.getElementById('update-banner')?.classList.add('hidden');
  });
}

async function loadSettings() {
  try {
    const res = await fetch('/api/config');
    if (!res.ok) return;
    const cfg = await res.json();

    document.getElementById('setting-download-dir').value = cfg.downloadDir || '';
    document.getElementById('setting-max-concurrent').value = cfg.maxConcurrent || 2;
    document.getElementById('setting-default-engine').value = cfg.defaultEngine || 'auto';
    const speedLimEl = document.getElementById('setting-speed-limit');
    if (speedLimEl) {
      speedLimEl.value = cfg.speedLimitKBps || 0;
    }
    const whenDoneEl = document.getElementById('when-done-select');
    if (whenDoneEl) {
      whenDoneEl.value = cfg.whenDoneAction || 'none';
    }
    defaultVideoCodec = cfg.videoCodec || 'h264';
    const codecEl = document.getElementById('setting-video-codec');
    if (codecEl) {
      codecEl.value = defaultVideoCodec;
    }
    const addCodecEl = document.getElementById('input-video-codec');
    if (addCodecEl) {
      addCodecEl.value = defaultVideoCodec;
    }
    autoCheckUpdatesEnabled = cfg.autoCheckUpdates !== false;
    const autoUpdateEl = document.getElementById('setting-auto-update');
    if (autoUpdateEl) {
      autoUpdateEl.checked = autoCheckUpdatesEnabled;
    }
    const appVerEl = document.getElementById('app-version-text');
    if (appVerEl && !(latestUpdateInfo && latestUpdateInfo.updateAvailable)) {
      appVerEl.textContent = cfg.version ? `v${cfg.version}` : 'Ready';
    }
    document.getElementById('setting-token').value = cfg.authToken || '';

    const statusEl = document.getElementById('ytdlp-status-text');
    if (cfg.ytDlpAvailable) {
      statusEl.innerHTML = '<span style="color:var(--success)">✅ Installed & Ready</span>';
    } else {
      statusEl.innerHTML = '<span style="color:var(--warning)">⚠️ Not Installed</span>';
    }

    const ffmpegEl = document.getElementById('ffmpeg-status-text');
    if (ffmpegEl) {
      if (cfg.ffmpegAvailable) {
        ffmpegEl.innerHTML = '<span style="color:var(--success)">✅ Installed & Ready</span>';
      } else {
        ffmpegEl.innerHTML = '<span style="color:var(--warning)">⚠️ Optional (install for 1080p+ & MP3)</span>';
      }
    }

    // Status bar: download folder + engine readiness
    if (sbFolderEl) sbFolderEl.textContent = cfg.downloadDir || '—';
    if (sbYtdlpEl) {
      sbYtdlpEl.textContent = cfg.ytDlpAvailable ? 'Ready' : 'Not installed';
      sbYtdlpEl.className = cfg.ytDlpAvailable ? 'sb-ok' : 'sb-warn';
    }
    if (sbFfmpegEl) {
      sbFfmpegEl.textContent = cfg.ffmpegAvailable ? 'Ready' : 'Not installed';
      sbFfmpegEl.className = cfg.ffmpegAvailable ? 'sb-ok' : 'sb-warn';
    }
  } catch (err) {
    console.error('Failed to load settings', err);
  }
}

async function checkForUpdates(manual = false) {
  const btnCheck = document.getElementById('btn-check-update');
  const btnApply = document.getElementById('btn-apply-update');
  const appVerEl = document.getElementById('app-version-text');
  const banner = document.getElementById('update-banner');
  const bannerText = document.getElementById('update-banner-text');
  const bannerLink = document.getElementById('link-banner-release');

  if (btnCheck && manual) {
    btnCheck.disabled = true;
    btnCheck.textContent = 'Checking...';
  }

  try {
    const res = await fetch(`/api/update/check?force=${manual ? '1' : '0'}`);
    const data = await res.json();
    if (!res.ok) {
      if (manual) {
        showToast(data.error || 'Failed to check for updates', true);
      }
      return;
    }

    latestUpdateInfo = data;
    if (data.updateAvailable) {
      if (appVerEl) {
        appVerEl.innerHTML = `<span style="color:var(--primary)">v${escapeHtml(data.currentVersion)} &rarr; <strong>v${escapeHtml(data.latestVersion)} available</strong></span>`;
      }
      if (btnApply) {
        btnApply.classList.remove('hidden');
        btnApply.disabled = !data.canAutoUpdate;
        if (!data.canAutoUpdate) {
          btnApply.title = 'Install folder is read-only — download from Release Notes';
        }
      }
      if (banner && bannerText) {
        bannerText.textContent = `GrabAll PC v${data.latestVersion} is available (current v${data.currentVersion})`;
        if (bannerLink) {
          if (data.releaseUrl) {
            bannerLink.href = data.releaseUrl;
            bannerLink.style.display = '';
          } else {
            bannerLink.style.display = 'none';
          }
        }
        const bannerUpdateBtn = document.getElementById('btn-banner-update');
        if (bannerUpdateBtn) {
          bannerUpdateBtn.style.display = data.canAutoUpdate ? '' : 'none';
        }
        banner.classList.remove('hidden');
      }
    } else {
      if (appVerEl) {
        appVerEl.innerHTML = `<span style="color:var(--success)">v${escapeHtml(data.currentVersion)} (Up to date)</span>`;
      }
      if (btnApply) {
        btnApply.classList.add('hidden');
      }
      if (banner) {
        banner.classList.add('hidden');
      }
      if (manual) {
        showToast(`GrabAll PC is up to date (v${data.currentVersion})`);
      }
    }
  } catch (err) {
    if (manual) {
      showToast('Error checking for updates', true);
    }
  } finally {
    if (btnCheck && manual) {
      btnCheck.disabled = false;
      btnCheck.textContent = 'Check for Updates';
    }
  }
}

async function applyAppUpdate() {
  const btnBanner = document.getElementById('btn-banner-update');
  const btnModal = document.getElementById('btn-apply-update');
  const btnCheck = document.getElementById('btn-check-update');

  [btnBanner, btnModal, btnCheck].forEach((b) => {
    if (b) b.disabled = true;
  });
  if (btnBanner) btnBanner.textContent = 'Updating...';
  if (btnModal) btnModal.textContent = 'Updating...';

  try {
    const res = await fetch('/api/update/apply', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ force: false }),
    });
    const data = await res.json();

    if (!res.ok) {
      showToast(data.error || 'Update failed', true);
      [btnBanner, btnModal, btnCheck].forEach((b) => {
        if (b) b.disabled = false;
      });
      if (btnBanner) btnBanner.textContent = 'Update & Restart';
      if (btnModal) btnModal.textContent = 'Update & Restart';
      return;
    }

    if (data.upToDate) {
      showToast(`Already up to date (v${data.version || ''})`);
      [btnBanner, btnModal, btnCheck].forEach((b) => {
        if (b) b.disabled = false;
      });
      if (btnBanner) btnBanner.textContent = 'Update & Restart';
      if (btnModal) btnModal.textContent = 'Update & Restart';
      return;
    }

    showToast('Update installed! Restarting GrabAll...');
    let attempts = 0;
    const pollRestart = setInterval(async () => {
      attempts++;
      if (attempts > 25) {
        clearInterval(pollRestart);
        location.reload();
        return;
      }
      try {
        const ping = await fetch('/api/ping', { cache: 'no-store' });
        if (ping.ok) {
          const pingData = await ping.json();
          if (!latestUpdateInfo || pingData.version === latestUpdateInfo.latestVersion || attempts >= 3) {
            clearInterval(pollRestart);
            location.reload();
          }
        }
      } catch {
        // Still restarting
      }
    }, 1200);
  } catch (err) {
    showToast('Error installing update', true);
    [btnBanner, btnModal, btnCheck].forEach((b) => {
      if (b) b.disabled = false;
    });
    if (btnBanner) btnBanner.textContent = 'Update & Restart';
    if (btnModal) btnModal.textContent = 'Update & Restart';
  }
}

// Utilities
function showToast(msg, isError = false) {
  toastEl.textContent = msg;
  toastEl.style.borderColor = isError ? 'var(--danger)' : 'var(--primary)';
  toastEl.classList.remove('hidden');
  setTimeout(() => {
    toastEl.classList.add('hidden');
  }, 3000);
}

function escapeHtml(str) {
  if (!str) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

function formatBytes(bytes) {
  if (!bytes || bytes <= 0) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
}

function qualityLabel(q) {
  const labels = {
    'best': 'Best',
    '2160': '4K',
    '1080': '1080p',
    '720': '720p',
    '480': '480p',
    '360': '360p',
    'audio-best': '🎵 Audio',
    'audio-mp3': '🎵 MP3',
    'audio-m4a': '🎵 M4A',
    'audio-opus': '🎵 Opus',
  };
  return labels[q] || q;
}

function codecLabel(c) {
  const labels = {
    'h264': 'H.264',
    'vp9': 'VP9',
    'av1': 'AV1',
    'auto': 'Auto Codec',
  };
  return labels[String(c || '').toLowerCase()] || c;
}
