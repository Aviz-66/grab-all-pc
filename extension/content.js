// GrabAll Content Script - Media Sniffer & Quick Overlay

(function () {
  // Scans for video elements on page
  function scanMediaElements() {
    const mediaList = [];
    const videos = document.querySelectorAll('video, audio');

    videos.forEach((el) => {
      let src = el.currentSrc || el.src;
      if (src && !src.startsWith('blob:') && !src.startsWith('data:')) {
        mediaList.push({
          url: src,
          type: el.tagName.toLowerCase() === 'video' ? 'Video' : 'Audio',
        });
      }

      // Check nested <source> tags
      const sources = el.querySelectorAll('source');
      sources.forEach((s) => {
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

  // Monitor DOM for dynamically added video players
  const observer = new MutationObserver(() => {
    const found = scanMediaElements();
    if (found.length > 0) {
      found.forEach((item) => {
        chrome.runtime.sendMessage({
          type: 'MEDIA_DETECTED_FROM_DOM',
          media: item,
        }).catch(() => {});
      });
    }
  });

  if (document.body) {
    observer.observe(document.body, { childList: true, subtree: true });
  } else {
    document.addEventListener('DOMContentLoaded', () => {
      observer.observe(document.body, { childList: true, subtree: true });
    });
  }
})();
