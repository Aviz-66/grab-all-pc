// GrabAll Popup Script

let activeTab = null;

document.addEventListener('DOMContentLoaded', async () => {
  const [tab] = await chrome.tabs.query({ active: true, currentWindow: true });
  activeTab = tab;

  await loadConfig();
  await checkDaemonStatus();
  await loadDetectedMedia();

  initListeners();
});

async function loadConfig() {
  const cfg = await chrome.storage.local.get({
    authToken: '',
    interceptDownloads: false,
    daemonUrl: 'http://127.0.0.1:17890',
    preferredQuality: 'best',
    preferredVideoCodec: 'h264',
    preferredSpeedMode: 'turbo',
  });

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

  chrome.runtime.sendMessage({ type: 'CHECK_DAEMON' }, (response) => {
    if (chrome.runtime.lastError || !response || !response.online) {
      dot.className = 'dot offline';
      statusText.textContent = 'GrabAll Not Running';
    } else {
      dot.className = 'dot online';
      statusText.textContent = 'Connected';
      if (response.token) {
        document.getElementById('input-token').value = response.token;
      }
      if (openBtn) {
        openBtn.textContent = response.info?.nativeGui ? '⚡ Open GrabAll Window' : '🌐 Open Web Dashboard';
      }
    }
  });
}

async function loadDetectedMedia() {
  if (!activeTab || !activeTab.id) return;

  const container = document.getElementById('media-container');

  chrome.runtime.sendMessage(
    { type: 'GET_TAB_MEDIA', tabId: activeTab.id },
    (response) => {
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
          <button class="btn btn-sm btn-primary btn-dl-stream">Download</button>
        `;

        div.querySelector('.btn-dl-stream').addEventListener('click', () => {
          const quality = document.getElementById('select-quality').value;
          const videoCodec = document.getElementById('select-video-codec')?.value || 'h264';
          const speedMode = document.getElementById('select-speed-mode')?.value || 'turbo';
          sendDownload(item.url, activeTab.title, engine, quality, false, 1, 0, speedMode, videoCodec);
        });

        container.appendChild(div);
      });
    }
  );
}

function initListeners() {
  // Persist quality selection
  document.getElementById('select-quality').addEventListener('change', async (e) => {
    await chrome.storage.local.set({ preferredQuality: e.target.value });
  });

  // Persist video codec selection
  const codecSel = document.getElementById('select-video-codec');
  if (codecSel) {
    codecSel.addEventListener('change', async (e) => {
      await chrome.storage.local.set({ preferredVideoCodec: e.target.value });
    });
  }

  // Persist speed mode selection (Turbo 6x vs Watch Live 1x)
  const speedSel = document.getElementById('select-speed-mode');
  if (speedSel) {
    speedSel.addEventListener('change', async (e) => {
      await chrome.storage.local.set({ preferredSpeedMode: e.target.value });
    });
  }

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

  // Intercept downloads toggle
  document.getElementById('check-intercept').addEventListener('change', async (e) => {
    await chrome.storage.local.set({ interceptDownloads: e.target.checked });
    showAlert('Download interception ' + (e.target.checked ? 'enabled' : 'disabled'), false);
  });

  // Save Token
  document.getElementById('btn-save-token').addEventListener('click', async () => {
    const token = document.getElementById('input-token').value.trim();
    await chrome.storage.local.set({ authToken: token });
    showAlert('Auth token saved!', false);
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
      },
    },
    (res) => {
      if (chrome.runtime.lastError || !res || !res.success) {
        const msg = res?.error || chrome.runtime.lastError?.message || 'Failed to connect to daemon';
        showAlert('Error: ' + msg, true);
      } else if (res.data?.playlist) {
        showAlert(`Queued ${res.data.count} videos from playlist!`, false);
      } else {
        showAlert('Download queued successfully!', false);
      }
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

function escapeHtml(str) {
  if (!str) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}
