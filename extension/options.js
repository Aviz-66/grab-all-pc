document.addEventListener('DOMContentLoaded', async () => {
  const cfg = await chrome.storage.local.get({
    daemonUrl: 'http://127.0.0.1:17890',
    authToken: '',
    interceptDownloads: false,
  });

  document.getElementById('opt-daemon-url').value = cfg.daemonUrl;
  document.getElementById('opt-token').value = cfg.authToken;
  document.getElementById('opt-intercept').checked = cfg.interceptDownloads;

  document.getElementById('btn-save').addEventListener('click', async () => {
    const daemonUrl = document.getElementById('opt-daemon-url').value.trim();
    const authToken = document.getElementById('opt-token').value.trim();
    const interceptDownloads = document.getElementById('opt-intercept').checked;

    await chrome.storage.local.set({ daemonUrl, authToken, interceptDownloads });

    const status = document.getElementById('status-msg');
    status.style.display = 'block';
    setTimeout(() => {
      status.style.display = 'none';
    }, 2500);
  });
});
