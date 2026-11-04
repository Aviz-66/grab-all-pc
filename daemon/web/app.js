// GrabAll Web Dashboard App

let tasks = [];
let currentFilter = 'all';
let sseConnection = null;

// DOM Elements
const queueListEl = document.getElementById('queue-list');
const queueEmptyEl = document.getElementById('queue-empty');
const activeBadgeEl = document.getElementById('active-count-badge');
const speedBadgeEl = document.getElementById('speed-badge');

const countAllEl = document.getElementById('count-all');
const countDownloadingEl = document.getElementById('count-downloading');
const countQueuedEl = document.getElementById('count-queued');
const countCompletedEl = document.getElementById('count-completed');
const countErrorEl = document.getElementById('count-error');

const filterTabs = document.querySelectorAll('.filter-tab');
const toastEl = document.getElementById('toast');

// Modals
const modalAdd = document.getElementById('modal-add');
const modalSettings = document.getElementById('modal-settings');
const formAdd = document.getElementById('form-add');
const formSettings = document.getElementById('form-settings');

// Initialize
document.addEventListener('DOMContentLoaded', () => {
  initSSE();
  initFilters();
  initModals();
  initSettings();
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
function initFilters() {
  filterTabs.forEach((tab) => {
    tab.addEventListener('click', () => {
      filterTabs.forEach((t) => t.classList.remove('active'));
      tab.classList.add('active');
      currentFilter = tab.dataset.filter;
      renderQueue();
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
}

function updateCounts() {
  let all = tasks.length;
  let downloading = 0;
  let queued = 0;
  let completed = 0;
  let error = 0;

  for (const t of tasks) {
    if (t.status === 'downloading') downloading++;
    else if (t.status === 'queued') queued++;
    else if (t.status === 'completed') completed++;
    else if (t.status === 'error' || t.status === 'canceled') error++;
  }

  countAllEl.textContent = all;
  countDownloadingEl.textContent = downloading;
  countQueuedEl.textContent = queued;
  countCompletedEl.textContent = completed;
  countErrorEl.textContent = error;

  activeBadgeEl.textContent = `${downloading} Active`;

  // Aggregate active speeds
  const activeSpeeds = tasks
    .filter((t) => t.status === 'downloading' && t.speed)
    .map((t) => t.speed);

  speedBadgeEl.textContent = activeSpeeds.length > 0 ? activeSpeeds.join(', ') : '0 KB/s';
}

function renderQueue() {
  updateCounts();

  const filtered = tasks.filter((t) => {
    if (currentFilter === 'all') return true;
    if (currentFilter === 'downloading') return t.status === 'downloading';
    if (currentFilter === 'queued') return t.status === 'queued';
    if (currentFilter === 'completed') return t.status === 'completed';
    if (currentFilter === 'error') return t.status === 'error' || t.status === 'canceled';
    return true;
  });

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
            ${task.quality && task.quality !== 'best' ? `<span class="engine-tag">${escapeHtml(qualityLabel(task.quality))}</span>` : ''}
            <span class="task-url" style="max-width:300px;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;">${escapeHtml(task.url)}</span>
          </div>
        </div>
      </div>
      <span class="task-status-badge status-${task.status}">${task.status}</span>
    </div>

    <div class="task-error-text" style="${task.errorMessage ? '' : 'display:none;'}">⚠️ ${escapeHtml(task.errorMessage || '')}</div>

    <div class="progress-container">
      <div class="progress-bar-bg">
        <div class="progress-bar-fill ${task.status}" style="width: ${task.progress.toFixed(1)}%;"></div>
      </div>
    </div>

    <div class="task-footer">
      <div class="task-stats">
        <span class="stat-progress"><strong>${task.progress.toFixed(1)}%</strong></span>
        <span class="stat-size">${task.totalBytes > 0 ? `${formatBytes(task.downloadedBytes)} / ${formatBytes(task.totalBytes)}` : formatBytes(task.downloadedBytes)}</span>
        <span class="stat-speed">${task.speed ? `⚡ ${escapeHtml(task.speed)}` : ''}</span>
        <span class="stat-eta">${task.eta ? `⏱️ ${escapeHtml(task.eta)}` : ''}</span>
      </div>
      <div class="task-actions">
        ${getTaskButtons(task)}
      </div>
    </div>
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

  const statProgress = card.querySelector('.stat-progress');
  if (statProgress) {
    statProgress.innerHTML = `<strong>${task.progress.toFixed(1)}%</strong>`;
  }

  const statSize = card.querySelector('.stat-size');
  if (statSize) {
    statSize.textContent = task.totalBytes > 0 ? `${formatBytes(task.downloadedBytes)} / ${formatBytes(task.totalBytes)}` : formatBytes(task.downloadedBytes);
  }

  const statSpeed = card.querySelector('.stat-speed');
  if (statSpeed) {
    statSpeed.textContent = task.speed ? `⚡ ${task.speed}` : '';
  }

  const statEta = card.querySelector('.stat-eta');
  if (statEta) {
    statEta.textContent = task.eta ? `⏱️ ${task.eta}` : '';
  }

  if (card.dataset.status !== task.status) {
    card.dataset.status = task.status;
    const actions = card.querySelector('.task-actions');
    if (actions) {
      actions.innerHTML = getTaskButtons(task);
      attachTaskActionEvents(card, task);
    }
  }
}

function getTaskButtons(task) {
  let buttons = '';
  if (task.status === 'downloading') {
    buttons += `<button class="btn btn-sm btn-secondary btn-action-pause" title="Pause">⏸️ Pause</button>`;
    buttons += `<button class="btn btn-sm btn-secondary btn-action-cancel" title="Cancel">⏹️ Cancel</button>`;
  } else if (task.status === 'paused' || task.status === 'error' || task.status === 'canceled') {
    buttons += `<button class="btn btn-sm btn-primary btn-action-resume" title="Resume">▶️ Resume</button>`;
  } else if (task.status === 'completed') {
    buttons += `<button class="btn btn-sm btn-secondary btn-action-folder" title="Show in Folder">📂 Folder</button>`;
  }

  buttons += `<button class="btn btn-sm btn-ghost btn-action-delete" title="Delete">🗑️</button>`;
  return buttons;
}

function attachTaskActionEvents(card, task) {
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
  document.getElementById('btn-add-modal').addEventListener('click', () => {
    modalAdd.classList.remove('hidden');
    document.getElementById('input-url').focus();
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
    const fileName = document.getElementById('input-filename').value.trim();
    const playlist = document.getElementById('input-playlist')?.checked || false;

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
        body: JSON.stringify({ url, engine, quality, fileName, playlist }),
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

    try {
      const res = await fetch('/api/config', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ downloadDir, maxConcurrent, defaultEngine }),
      });
      if (res.ok) {
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
}

async function loadSettings() {
  try {
    const res = await fetch('/api/config');
    if (!res.ok) return;
    const cfg = await res.json();

    document.getElementById('setting-download-dir').value = cfg.downloadDir || '';
    document.getElementById('setting-max-concurrent').value = cfg.maxConcurrent || 2;
    document.getElementById('setting-default-engine').value = cfg.defaultEngine || 'auto';
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
  } catch (err) {
    console.error('Failed to load settings', err);
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
