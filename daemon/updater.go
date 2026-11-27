package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	AppVersion = "2.5.0"

	defaultUpdateManifestURL = "https://raw.githubusercontent.com/Avishka-55/graball-website/main/latest.json"
	defaultUpdateReleasesURL = "https://api.github.com/repos/Avishka-55/graball-website/releases?per_page=30"
	defaultReleaseTagURLBase = "https://github.com/Avishka-55/graball-website/releases/tag/"

	updateCheckCacheTTL = 10 * time.Minute
	maxUpdateEntryBytes = 128 << 20 // 128 MiB safety cap per extracted entry
)

// UpdateInfo describes the latest PC release metadata relative to the running binary.
type UpdateInfo struct {
	CurrentVersion  string `json:"currentVersion"`
	LatestVersion   string `json:"latestVersion"`
	Tag             string `json:"tag"`
	PublishedAt     string `json:"publishedAt,omitempty"`
	DownloadURL     string `json:"downloadUrl"`
	FileName        string `json:"fileName"`
	SHA256          string `json:"sha256,omitempty"`
	SizeBytes       int64  `json:"sizeBytes,omitempty"`
	ReleaseURL      string `json:"releaseUrl,omitempty"`
	UpdateAvailable bool   `json:"updateAvailable"`
	CanAutoUpdate   bool   `json:"canAutoUpdate"`
}

// Updater checks the public graball-website repository for PC releases and applies self-updates.
type Updater struct {
	manifestURL           string
	releasesURL           string
	releaseTagURLBase     string
	currentVersion        string
	goos                  string
	goarch                string
	httpClient            *http.Client
	allowInsecureDownload bool

	mu       sync.Mutex
	cached   *UpdateInfo
	cachedAt time.Time
	applying bool
}

// NewUpdater creates an Updater configured for the running OS/architecture.
func NewUpdater() *Updater {
	return &Updater{
		manifestURL:       defaultUpdateManifestURL,
		releasesURL:       defaultUpdateReleasesURL,
		releaseTagURLBase: defaultReleaseTagURLBase,
		currentVersion:    AppVersion,
		goos:              runtime.GOOS,
		goarch:            runtime.GOARCH,
		httpClient:        &http.Client{Timeout: 20 * time.Second},
	}
}

func normalizeVersionString(v string) string {
	s := strings.TrimSpace(v)
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "pc-") {
		s = s[3:]
	}
	s = strings.TrimLeft(s, "vV")
	if idx := strings.IndexAny(s, "-+"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

func parseVersionParts(v string) []int {
	norm := normalizeVersionString(v)
	if norm == "" {
		return nil
	}
	rawParts := strings.Split(norm, ".")
	parts := make([]int, len(rawParts))
	for i, p := range rawParts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 0 {
			n = 0
		}
		parts[i] = n
	}
	return parts
}

// compareSemver compares two version strings (handling "pc-v2.2.8", "v2.2.9", "2.3.0").
// Returns -1 if a < b, 0 if a == b, and 1 if a > b.
func compareSemver(a, b string) int {
	pa := parseVersionParts(a)
	pb := parseVersionParts(b)
	maxLen := len(pa)
	if len(pb) > maxLen {
		maxLen = len(pb)
	}
	for i := 0; i < maxLen; i++ {
		var va, vb int
		if i < len(pa) {
			va = pa[i]
		}
		if i < len(pb) {
			vb = pb[i]
		}
		if va < vb {
			return -1
		}
		if va > vb {
			return 1
		}
	}
	return 0
}

func platformDownloadKey(goos, goarch string) string {
	switch goos {
	case "windows":
		return "windows"
	case "linux":
		switch goarch {
		case "amd64", "arm64":
			return "linux_" + goarch
		}
	case "darwin":
		switch goarch {
		case "amd64", "arm64":
			return "macos_" + goarch
		}
	}
	return ""
}

func platformAssetName(goos, goarch string) string {
	switch goos {
	case "windows":
		return "graball-windows-amd64.zip"
	case "linux":
		if goarch == "amd64" || goarch == "arm64" {
			return fmt.Sprintf("graball-linux-%s.tar.gz", goarch)
		}
	case "darwin":
		if goarch == "amd64" || goarch == "arm64" {
			return fmt.Sprintf("graball-darwin-%s.tar.gz", goarch)
		}
	}
	return ""
}

func targetBinaryName(goos string) string {
	if goos == "windows" {
		return "graball.exe"
	}
	return "graball"
}

func currentExecutablePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != "" {
		return resolved, nil
	}
	return exe, nil
}

func canReplaceCurrentExecutable() bool {
	exePath, err := currentExecutablePath()
	if err != nil || exePath == "" {
		return false
	}
	return isDirWritable(filepath.Dir(exePath))
}

func isAllowedUpdateURL(rawURL string, allowInsecure bool) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return false
	}
	if allowInsecure && (u.Scheme == "http" || u.Scheme == "https") {
		return true
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "github.com" || host == "githubusercontent.com" || strings.HasSuffix(host, ".githubusercontent.com")
}

func isSafeArchiveSubpath(baseDir, relPath string) (string, bool) {
	cleanedRel := strings.ReplaceAll(strings.TrimSpace(relPath), "\\", "/")
	if cleanedRel == "" || strings.HasPrefix(cleanedRel, "/") {
		return "", false
	}
	if len(cleanedRel) >= 2 && cleanedRel[1] == ':' {
		return "", false
	}
	cleanSlash := path.Clean(cleanedRel)
	if cleanSlash == "." || cleanSlash == ".." || strings.HasPrefix(cleanSlash, "../") {
		return "", false
	}
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		return "", false
	}
	joined := filepath.Join(absBase, filepath.FromSlash(cleanSlash))
	relBack, err := filepath.Rel(absBase, joined)
	if err != nil || relBack == "." || relBack == ".." || strings.HasPrefix(relBack, ".."+string(filepath.Separator)) {
		return "", false
	}
	return joined, true
}

func cloneUpdateInfo(src *UpdateInfo) *UpdateInfo {
	if src == nil {
		return nil
	}
	cp := *src
	return &cp
}

// CheckForUpdate queries graball-website for the latest PC release (ignoring Android releases).
func (u *Updater) CheckForUpdate(ctx context.Context, force bool) (*UpdateInfo, error) {
	u.mu.Lock()
	if !force && u.cached != nil && time.Since(u.cachedAt) < updateCheckCacheTTL {
		cp := cloneUpdateInfo(u.cached)
		u.mu.Unlock()
		return cp, nil
	}
	u.mu.Unlock()

	var info *UpdateInfo
	var primaryErr, fallbackErr error

	info, primaryErr = u.fetchFromLatestJSON(ctx)
	if primaryErr != nil || info == nil || info.LatestVersion == "" || info.DownloadURL == "" {
		fallbackInfo, fErr := u.fetchFromGitHubReleases(ctx)
		if fErr == nil && fallbackInfo != nil && fallbackInfo.LatestVersion != "" && fallbackInfo.DownloadURL != "" {
			info = fallbackInfo
		} else {
			fallbackErr = fErr
		}
	}

	if info == nil || info.LatestVersion == "" || info.DownloadURL == "" {
		if primaryErr != nil && fallbackErr != nil {
			return nil, fmt.Errorf("update check failed (%v; fallback: %v)", primaryErr, fallbackErr)
		}
		if fallbackErr != nil {
			return nil, fallbackErr
		}
		if primaryErr != nil {
			return nil, primaryErr
		}
		return nil, fmt.Errorf("no PC release asset available for %s/%s", u.goos, u.goarch)
	}

	info.CurrentVersion = normalizeVersionString(u.currentVersion)
	info.LatestVersion = normalizeVersionString(info.LatestVersion)
	info.UpdateAvailable = compareSemver(info.LatestVersion, info.CurrentVersion) > 0 && info.DownloadURL != ""
	info.CanAutoUpdate = canReplaceCurrentExecutable()
	if info.ReleaseURL == "" && info.Tag != "" {
		info.ReleaseURL = strings.TrimRight(u.releaseTagURLBase, "/") + "/" + url.PathEscape(info.Tag)
	}

	u.mu.Lock()
	u.cached = cloneUpdateInfo(info)
	u.cachedAt = time.Now()
	u.mu.Unlock()

	return cloneUpdateInfo(info), nil
}

type websiteLatestManifest struct {
	PC struct {
		Version     string `json:"version"`
		Tag         string `json:"tag"`
		PublishedAt string `json:"published_at"`
		Downloads   map[string]struct {
			File      string `json:"file"`
			SHA256    string `json:"sha256"`
			SizeBytes int64  `json:"size_bytes"`
			URL       string `json:"url"`
		} `json:"downloads"`
	} `json:"pc"`
}

func (u *Updater) doGet(ctx context.Context, targetURL string, accept string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "GrabAll-PC/"+normalizeVersionString(u.currentVersion))
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, targetURL)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

func (u *Updater) fetchFromLatestJSON(ctx context.Context) (*UpdateInfo, error) {
	if strings.TrimSpace(u.manifestURL) == "" {
		return nil, fmt.Errorf("manifest URL is empty")
	}
	body, err := u.doGet(ctx, u.manifestURL, "application/json")
	if err != nil {
		return nil, err
	}

	var manifest websiteLatestManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("invalid latest.json: %w", err)
	}

	ver := normalizeVersionString(manifest.PC.Version)
	if ver == "" {
		ver = normalizeVersionString(manifest.PC.Tag)
	}
	if ver == "" {
		return nil, fmt.Errorf("latest.json has no pc.version")
	}

	key := platformDownloadKey(u.goos, u.goarch)
	if key == "" {
		return nil, fmt.Errorf("unsupported platform %s/%s for auto-update", u.goos, u.goarch)
	}
	dl, ok := manifest.PC.Downloads[key]
	if !ok || strings.TrimSpace(dl.URL) == "" {
		return nil, fmt.Errorf("latest.json missing download entry for %s", key)
	}

	return &UpdateInfo{
		LatestVersion: ver,
		Tag:           strings.TrimSpace(manifest.PC.Tag),
		PublishedAt:   strings.TrimSpace(manifest.PC.PublishedAt),
		DownloadURL:   strings.TrimSpace(dl.URL),
		FileName:      strings.TrimSpace(dl.File),
		SHA256:        strings.TrimPrefix(strings.ToLower(strings.TrimSpace(dl.SHA256)), "sha256:"),
		SizeBytes:     dl.SizeBytes,
	}, nil
}

type githubReleaseEntry struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	HTMLURL     string `json:"html_url"`
	Draft       bool   `json:"draft"`
	Prerelease  bool   `json:"prerelease"`
	PublishedAt string `json:"published_at"`
	Assets      []struct {
		Name               string `json:"name"`
		Size               int64  `json:"size"`
		Digest             string `json:"digest"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

func (u *Updater) fetchFromGitHubReleases(ctx context.Context) (*UpdateInfo, error) {
	if strings.TrimSpace(u.releasesURL) == "" {
		return nil, fmt.Errorf("releases API URL is empty")
	}
	body, err := u.doGet(ctx, u.releasesURL, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}

	var releases []githubReleaseEntry
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, fmt.Errorf("invalid GitHub releases response: %w", err)
	}

	wantAsset := platformAssetName(u.goos, u.goarch)
	if wantAsset == "" {
		return nil, fmt.Errorf("unsupported platform %s/%s for auto-update", u.goos, u.goarch)
	}

	var best *UpdateInfo
	for _, rel := range releases {
		if rel.Draft || rel.Prerelease {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(rel.TagName)), "pc-v") {
			continue
		}
		ver := normalizeVersionString(rel.TagName)
		if ver == "" {
			continue
		}

		for _, asset := range rel.Assets {
			if strings.EqualFold(strings.TrimSpace(asset.Name), wantAsset) && strings.TrimSpace(asset.BrowserDownloadURL) != "" {
				if best == nil || compareSemver(ver, best.LatestVersion) > 0 {
					best = &UpdateInfo{
						LatestVersion: ver,
						Tag:           strings.TrimSpace(rel.TagName),
						PublishedAt:   strings.TrimSpace(rel.PublishedAt),
						DownloadURL:   strings.TrimSpace(asset.BrowserDownloadURL),
						FileName:      strings.TrimSpace(asset.Name),
						SHA256:        strings.TrimPrefix(strings.ToLower(strings.TrimSpace(asset.Digest)), "sha256:"),
						SizeBytes:     asset.Size,
						ReleaseURL:    strings.TrimSpace(rel.HTMLURL),
					}
				}
				break
			}
		}
	}

	if best == nil {
		return nil, fmt.Errorf("no pc-v* release found with asset %s", wantAsset)
	}
	return best, nil
}

// ApplyUpdate downloads, verifies, extracts, and swaps the running executable in-place.
func (u *Updater) ApplyUpdate(ctx context.Context, info *UpdateInfo, onProgress func(stage string, downloaded, total int64)) error {
	exePath, err := currentExecutablePath()
	if err != nil {
		return fmt.Errorf("cannot locate running executable: %w", err)
	}
	return u.applyUpdateToPath(ctx, info, exePath, onProgress)
}

func (u *Updater) applyUpdateToPath(ctx context.Context, info *UpdateInfo, targetExePath string, onProgress func(stage string, downloaded, total int64)) error {
	if info == nil || strings.TrimSpace(info.DownloadURL) == "" {
		return fmt.Errorf("missing update download URL")
	}
	if !isAllowedUpdateURL(info.DownloadURL, u.allowInsecureDownload) {
		return fmt.Errorf("untrusted update URL host: %s", info.DownloadURL)
	}

	u.mu.Lock()
	if u.applying {
		u.mu.Unlock()
		return fmt.Errorf("an update is already in progress")
	}
	u.applying = true
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.applying = false
		u.mu.Unlock()
	}()

	exeDir := filepath.Dir(targetExePath)
	if !isDirWritable(exeDir) {
		return fmt.Errorf("install folder %s is not writable", exeDir)
	}

	archiveTmp := filepath.Join(exeDir, ".graball_update_archive.tmp")
	_ = os.Remove(archiveTmp)
	defer os.Remove(archiveTmp)

	if onProgress != nil {
		onProgress("downloading", 0, info.SizeBytes)
	}

	if err := u.downloadArchiveWithSHA256(ctx, info, archiveTmp, onProgress); err != nil {
		return err
	}

	if onProgress != nil {
		onProgress("extracting", 0, 0)
	}

	newExePath := targetExePath + ".new"
	_ = os.Remove(newExePath)
	if err := u.extractArchive(archiveTmp, info.FileName, info.DownloadURL, newExePath, exeDir); err != nil {
		_ = os.Remove(newExePath)
		return err
	}

	fi, err := os.Stat(newExePath)
	if err != nil || fi.IsDir() || fi.Size() == 0 {
		_ = os.Remove(newExePath)
		return fmt.Errorf("extracted update binary is missing or empty")
	}

	oldExePath := targetExePath + ".old"
	_ = removeFileWithRetry(oldExePath, 4, 50*time.Millisecond)

	if err := renameFileWithRetry(targetExePath, oldExePath, 6, 80*time.Millisecond); err != nil {
		_ = os.Remove(newExePath)
		return fmt.Errorf("failed to stage old binary for replacement: %w", err)
	}

	if err := renameFileWithRetry(newExePath, targetExePath, 6, 80*time.Millisecond); err != nil {
		// Roll back original executable if moving the new one into place failed
		_ = renameFileWithRetry(oldExePath, targetExePath, 6, 80*time.Millisecond)
		_ = os.Remove(newExePath)
		return fmt.Errorf("failed to install updated binary: %w", err)
	}

	if u.goos != "windows" {
		_ = removeFileWithRetry(oldExePath, 2, 20*time.Millisecond)
	}

	if onProgress != nil {
		onProgress("restarting", 0, 0)
	}
	return nil
}

func (u *Updater) downloadArchiveWithSHA256(ctx context.Context, info *UpdateInfo, destPath string, onProgress func(stage string, downloaded, total int64)) error {
	dlClient := &http.Client{
		Timeout:   5 * time.Minute,
		Transport: u.httpClient.Transport,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.DownloadURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "GrabAll-PC/"+normalizeVersionString(u.currentVersion))

	resp, err := dlClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download update: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("update download returned HTTP %d", resp.StatusCode)
	}

	outFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("failed to create temp archive: %w", err)
	}
	defer outFile.Close()

	totalBytes := resp.ContentLength
	if totalBytes <= 0 && info.SizeBytes > 0 {
		totalBytes = info.SizeBytes
	}

	hasher := sha256.New()
	buf := make([]byte, 32*1024)
	var downloaded int64
	lastTick := time.Now()

	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, wErr := outFile.Write(buf[:n]); wErr != nil {
				return fmt.Errorf("failed writing update archive: %w", wErr)
			}
			_, _ = hasher.Write(buf[:n])
			downloaded += int64(n)
			if onProgress != nil && time.Since(lastTick) >= 150*time.Millisecond {
				onProgress("downloading", downloaded, totalBytes)
				lastTick = time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("update download interrupted: %w", readErr)
		}
	}

	if err := outFile.Close(); err != nil {
		return err
	}

	if onProgress != nil {
		onProgress("downloading", downloaded, totalBytes)
	}

	expectedHash := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(info.SHA256)), "sha256:")
	if expectedHash != "" {
		actualHash := hex.EncodeToString(hasher.Sum(nil))
		if actualHash != expectedHash {
			return fmt.Errorf("SHA-256 verification failed (expected %s, got %s)", expectedHash, actualHash)
		}
	}

	return nil
}

func (u *Updater) extractArchive(archivePath, fileName, downloadURL, newExePath, exeDir string) error {
	extDir := filepath.Join(exeDir, "extension")
	hasExtDir := false
	if st, err := os.Stat(extDir); err == nil && st.IsDir() {
		hasExtDir = true
	}

	wantBin := targetBinaryName(u.goos)
	foundBinary := false

	processEntry := func(rawName string, r io.Reader) error {
		cleaned := strings.ReplaceAll(strings.TrimSpace(rawName), "\\", "/")
		cleaned = strings.TrimPrefix(cleaned, "./")
		if cleaned == "" || strings.HasSuffix(cleaned, "/") {
			return nil
		}

		// Validate path stays inside exeDir before matching
		if _, safe := isSafeArchiveSubpath(exeDir, cleaned); !safe {
			return fmt.Errorf("unsafe archive entry path rejected: %s", rawName)
		}

		if strings.EqualFold(path.Base(cleaned), wantBin) && !strings.Contains(cleaned, "/") {
			if err := writeLimitedFile(newExePath, r, 0755); err != nil {
				return fmt.Errorf("failed writing updated binary: %w", err)
			}
			foundBinary = true
			return nil
		}

		if hasExtDir && strings.HasPrefix(strings.ToLower(cleaned), "extension/") {
			relExt := cleaned[len("extension/"):]
			if destExtFile, ok := isSafeArchiveSubpath(extDir, relExt); ok {
				if mkErr := os.MkdirAll(filepath.Dir(destExtFile), 0755); mkErr == nil {
					_ = writeLimitedFile(destExtFile, r, 0644)
				}
			}
		}
		return nil
	}

	hint := strings.ToLower(fileName + " " + downloadURL)
	if strings.Contains(hint, ".zip") {
		if err := extractZipEntries(archivePath, processEntry); err != nil {
			return err
		}
	} else {
		if err := extractTarGzEntries(archivePath, processEntry); err != nil {
			return err
		}
	}

	if !foundBinary {
		return fmt.Errorf("update archive did not contain %s", wantBin)
	}
	return nil
}

func writeLimitedFile(destPath string, r io.Reader, mode fs.FileMode) error {
	f, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer f.Close()

	if _, err := io.Copy(f, io.LimitReader(r, maxUpdateEntryBytes)); err != nil {
		return err
	}
	return f.Close()
}

func extractZipEntries(zipPath string, fn func(name string, r io.Reader) error) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("failed to open zip archive: %w", err)
	}
	defer zr.Close()

	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		procErr := fn(f.Name, rc)
		_ = rc.Close()
		if procErr != nil {
			return procErr
		}
	}
	return nil
}

func extractTarGzEntries(tgzPath string, fn func(name string, r io.Reader) error) error {
	f, err := os.Open(tgzPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gzr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("failed to open tar.gz archive: %w", err)
	}
	defer gzr.Close()

	tr := tar.NewReader(gzr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("corrupt tar archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if err := fn(hdr.Name, tr); err != nil {
			return err
		}
	}
	return nil
}

func cleanupOldBinaryAt(exePath string) {
	if exePath == "" {
		return
	}
	_ = removeFileWithRetry(exePath+".old", 6, 150*time.Millisecond)
	_ = removeFileWithRetry(exePath+".new", 3, 50*time.Millisecond)
}

func cleanupOldUpdatedBinary() {
	exePath, err := currentExecutablePath()
	if err != nil {
		return
	}
	cleanupOldBinaryAt(exePath)
}

// buildRestartArgs filters any prior --wait-pid flags and appends --wait-pid=<oldPID>.
func buildRestartArgs(origArgs []string, oldPID int) []string {
	out := make([]string, 0, len(origArgs)+2)
	skipNext := false
	for _, arg := range origArgs {
		if skipNext {
			skipNext = false
			continue
		}
		if arg == "-wait-pid" || arg == "--wait-pid" {
			skipNext = true
			continue
		}
		if strings.HasPrefix(arg, "-wait-pid=") || strings.HasPrefix(arg, "--wait-pid=") {
			continue
		}
		out = append(out, arg)
	}
	if oldPID > 0 {
		out = append(out, "--wait-pid", strconv.Itoa(oldPID))
	}
	return out
}
