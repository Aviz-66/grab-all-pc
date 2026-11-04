# ⚡ GrabAll — Ultra-Lean Download Manager

A lightweight download manager powered by **yt-dlp**, built for potato PCs (1GB RAM).  
Single Go binary (~7.5MB) + Brave/Chrome extension. No Electron, no Python runtime, no frameworks.

## Architecture

```
┌─────────────────────┐       HTTP (localhost:17890)       ┌─────────────────────┐
│   Brave Extension   │  ──────────────────────────────▶   │   Go Daemon (7.5MB) │
│  (catches videos +  │   URL + Cookies + UA + Token       │  ┌───────────────┐  │
│   cookies + UA)     │                                    │  │  Queue Manager │  │
└─────────────────────┘                                    │  │  SSE Broadcast │  │
                                                           │  │  Config/Auth   │  │
┌─────────────────────┐       SSE (real-time updates)      │  └───────┬───────┘  │
│   Web Dashboard     │  ◀──────────────────────────────   │          │          │
│  (localhost:17890)  │                                    │    ┌─────┴─────┐    │
│  served by daemon   │                                    │    │  Engines  │    │
└─────────────────────┘                                    │    │ ┌───────┐ │    │
                                                           │    │ │Direct │ │    │
                                                           │    │ │HTTP   │ │    │
                                                           │    │ └───────┘ │    │
                                                           │    │ ┌───────┐ │    │
                                                           │    │ │yt-dlp │ │    │
                                                           │    │ └───────┘ │    │
                                                           │    └───────────┘    │
                                                           └─────────────────────┘
```

## RAM Usage

| Component | Memory |
|-----------|--------|
| Go daemon (idle) | ~12 MB |
| yt-dlp (per download) | ~60-100 MB |
| ffmpeg (merge spike) | ~30 MB, brief |
| Web UI | 0 MB extra (uses your browser) |
| **Total idle** | **~12 MB** |

## Quick Start

### 1. Build the daemon

```bash
# Linux/macOS
make build

# Windows cross-compile
make windows
```

Or directly:
```bash
go build -ldflags="-s -w" -o graball ./daemon
```

### 2. Install yt-dlp (one-time)

```bash
make ytdlp
# or
./scripts/setup_ytdlp.sh
# or click "Install yt-dlp" in the web dashboard Settings
```

### 3. Run GrabAll

- **On Windows (`graball.exe`)**: Just **double-click `graball.exe`**!
  - Opens a real **Native Win32 IDM-style Desktop Window** (~14 MB RAM, **zero black CMD window**).
  - Lives in the **Windows System Tray** (right-click tray icon or **Options → Start with Windows** to auto-launch silently on boot).
  - Pops up a native **IDM Mini Progress Dialog** whenever a download starts (with `Open File` / `Open Folder` on completion).
- **On Linux / macOS (`./graball`)**: Runs as a lightweight ~12 MB daemon serving the embedded Web Dashboard on `http://127.0.0.1:17890`.

### 4. Load the Brave/Chrome extension (Zero Token Copy-Paste!)

1. Open `brave://extensions` (or `chrome://extensions`)
2. Enable **Developer mode** (top-right toggle)
3. Click **Load unpacked** → select the `extension/` folder
4. **Done!** The extension **auto-pairs** with `graball.exe` automatically — no token copy-pasting needed.

### 5. Download stuff

- **Hover any `<video>`** (Facebook, TikTok, Instagram, X/Twitter, Reddit, HTML5 videos) → pick resolution/audio quality and click **⚡ Grab**
- **YouTube pages & playlists** → injected **⚡ GrabAll** button under watch pages + **📑 Grab Playlist** button (auto-organizes playlist items into `Downloads/<Playlist Title>/01 - ...`)
- **Right-click any link / page** → download link, current page video, or entire playlist
- **Extension popup** shows detected `.m3u8` / `.mp4` streams with quality/audio selector (`Best`, `4K`, `1080p`, `720p`, `MP3`, `M4A`, etc.)
- **Native Win32 App (Windows) / Web Dashboard (`127.0.0.1:17890`)** for live progress, pause/resume/cancel, **Open Folder**, and 1-click **yt-dlp + QuickJS + FFmpeg** installers

## How It Works

### Two download engines

| Engine | Used for | How |
|--------|----------|-----|
| **Direct HTTP** | `.zip`, `.exe`, `.pdf`, `.iso`, `.mp4` files | Single GET stream to disk with resume (`Range: bytes=N-`) |
| **yt-dlp** | YouTube, TikTok, Twitch, `.m3u8`, `.mpd`, 1400+ sites | Spawns `yt-dlp --cookies cookies.txt` with progress parsing |

Auto-detection picks the right engine based on the URL. Override manually in the add dialog or extension.

### Cookie handoff (the key trick)

1. Extension grabs cookies for the URL via `chrome.cookies.getAll()`
2. Sends them as JSON to the daemon over `localhost:17890/api/add`
3. Daemon writes a Netscape-format `cookies.txt` temp file
4. Passes `--cookies cookies.txt` to yt-dlp (or `Cookie:` header for direct)
5. **Deletes the temp cookie file after download completes**

This bypasses Chrome's encrypted cookie database entirely — no `--cookies-from-browser` needed.

### Security

- Daemon binds to `127.0.0.1` only (not exposed to LAN/internet)
- All API calls require an auth token (`X-Daemon-Token` header)
- Token is auto-generated on first run, stored in `config.json`
- Cookie files are temp-written with `0600` permissions and deleted after use

## CLI Flags

```
./graball [flags]

  -port int         Server port (default: 17890)
  -config string    Config file path (default: config.json)
  -data string      Queue persistence file (default: queue.json)
  -downloads string Download directory override
```

## API Endpoints

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/ping` | Health check (no auth required) |
| GET | `/api/queue` | Get all download tasks |
| POST | `/api/add` | Add new download `{url, engine, cookies, ...}` |
| POST | `/api/pause` | Pause task `{id}` |
| POST | `/api/resume` | Resume task `{id}` |
| POST | `/api/cancel` | Cancel task `{id}` |
| POST | `/api/delete` | Delete task `{id, deleteFile}` |
| POST | `/api/clear-completed` | Remove finished/failed tasks |
| GET/POST | `/api/config` | Read/update daemon settings |
| POST | `/api/install-ytdlp` | Download standalone yt-dlp binary |
| GET | `/api/events` | SSE stream for real-time queue updates |

## Project Structure

```
grab-all-pc/
├── daemon/                  # Go daemon source
│   ├── main.go              # Entry point, CLI flags, banner
│   ├── server.go            # HTTP server, SSE broker, API handlers
│   ├── queue.go             # Download queue manager, task lifecycle
│   ├── downloader_direct.go # Direct HTTP streaming engine (resume support)
│   ├── downloader_ytdlp.go  # yt-dlp spawner with progress parsing
│   ├── netscape_cookies.go  # Cookie format conversion
│   ├── config.go            # JSON config with auth token generation
│   └── web/                 # Embedded web dashboard (no external deps)
│       ├── index.html
│       ├── style.css
│       └── app.js
├── extension/               # Brave/Chrome MV3 extension
│   ├── manifest.json
│   ├── background.js        # Service worker: sniffing, context menus, cookie grab
│   ├── content.js           # DOM scanner for <video>/<audio> elements
│   ├── popup.html/js        # Extension popup UI
│   ├── options.html/js      # Settings page
│   └── icons/
├── scripts/
│   ├── build.sh             # Build script (Linux + Windows cross-compile)
│   └── setup_ytdlp.sh       # One-liner yt-dlp installer
├── Makefile
└── go.mod
```

## Tech Stack

- **Daemon**: Go stdlib only (`net/http`, `embed`, `encoding/json`). Zero dependencies. No CGO.
- **Extension**: Vanilla JS, Manifest V3. No bundler, no npm, no frameworks.
- **Web UI**: Plain HTML/CSS/JS embedded in the Go binary via `//go:embed`.
- **Download engine**: yt-dlp standalone binary + direct HTTP streaming.

## License

MIT
