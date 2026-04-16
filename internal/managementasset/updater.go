package managementasset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/util"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v6/sdk/config"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/singleflight"
)

const (
	defaultManagementReleaseURL  = "https://api.github.com/repos/router-for-me/Cli-Proxy-API-Management-Center/releases/latest"
	defaultManagementFallbackURL = "https://cpamc.router-for.me/"
	managementAssetName          = "management.html"
	httpUserAgent                = "CLIProxyAPI-management-updater"
	managementSyncMinInterval    = 30 * time.Second
	updateCheckInterval          = 3 * time.Hour
	maxAssetDownloadSize         = 50 << 20 // 10 MB safety limit for management asset downloads
)

var defaultRepoAssetCandidates = []string{
	managementAssetName,
	"static/" + managementAssetName,
	"public/" + managementAssetName,
	"dist/" + managementAssetName,
	"web/" + managementAssetName,
}

// ManagementFileName exposes the control panel asset filename.
const ManagementFileName = managementAssetName

var (
	lastUpdateCheckMu   sync.Mutex
	lastUpdateCheckTime time.Time
	currentConfigPtr    atomic.Pointer[config.Config]
	schedulerOnce       sync.Once
	schedulerConfigPath atomic.Value
	sfGroup             singleflight.Group
)

// SetCurrentConfig stores the latest configuration snapshot for management asset decisions.
func SetCurrentConfig(cfg *config.Config) {
	if cfg == nil {
		currentConfigPtr.Store(nil)
		return
	}
	currentConfigPtr.Store(cfg)
}

// StartAutoUpdater launches a background goroutine that periodically ensures the management asset is up to date.
// It respects the disable-control-panel flag on every iteration and supports hot-reloaded configurations.
func StartAutoUpdater(ctx context.Context, configFilePath string) {
	configFilePath = strings.TrimSpace(configFilePath)
	if configFilePath == "" {
		log.Debug("management asset auto-updater skipped: empty config path")
		return
	}

	schedulerConfigPath.Store(configFilePath)

	schedulerOnce.Do(func() {
		go runAutoUpdater(ctx)
	})
}

func runAutoUpdater(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	ticker := time.NewTicker(updateCheckInterval)
	defer ticker.Stop()

	runOnce := func() {
		cfg := currentConfigPtr.Load()
		if cfg == nil {
			log.Debug("management asset auto-updater skipped: config not yet available")
			return
		}
		if cfg.RemoteManagement.DisableControlPanel {
			log.Debug("management asset auto-updater skipped: control panel disabled")
			return
		}
		if cfg.RemoteManagement.DisableAutoUpdatePanel {
			log.Debug("management asset auto-updater skipped: disable-auto-update-panel is enabled")
			return
		}

		configPath, _ := schedulerConfigPath.Load().(string)
		staticDir := StaticDir(configPath)
		EnsureLatestManagementHTML(ctx, staticDir, cfg.ProxyURL, cfg.RemoteManagement.PanelGitHubRepository)
	}

	runOnce()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

func newHTTPClient(proxyURL string) *http.Client {
	client := &http.Client{Timeout: 15 * time.Second}

	sdkCfg := &sdkconfig.SDKConfig{ProxyURL: strings.TrimSpace(proxyURL)}
	util.SetProxy(sdkCfg, client)

	return client
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

type releaseResponse struct {
	Assets []releaseAsset `json:"assets"`
}

type repoSource struct {
	ReleaseURL       string
	RawAssetURLs     []string
	DisplayReference string
}

// StaticDir resolves the directory that stores the management control panel asset.
func StaticDir(configFilePath string) string {
	if override := strings.TrimSpace(os.Getenv("MANAGEMENT_STATIC_PATH")); override != "" {
		cleaned := filepath.Clean(override)
		if strings.EqualFold(filepath.Base(cleaned), managementAssetName) {
			return filepath.Dir(cleaned)
		}
		return cleaned
	}

	if writable := util.WritablePath(); writable != "" {
		return filepath.Join(writable, "static")
	}

	configFilePath = strings.TrimSpace(configFilePath)
	if configFilePath == "" {
		return ""
	}

	base := filepath.Dir(configFilePath)
	fileInfo, err := os.Stat(configFilePath)
	if err == nil {
		if fileInfo.IsDir() {
			base = configFilePath
		}
	}

	return filepath.Join(base, "static")
}

// FilePath resolves the absolute path to the management control panel asset.
func FilePath(configFilePath string) string {
	if override := strings.TrimSpace(os.Getenv("MANAGEMENT_STATIC_PATH")); override != "" {
		cleaned := filepath.Clean(override)
		if strings.EqualFold(filepath.Base(cleaned), managementAssetName) {
			return cleaned
		}
		return filepath.Join(cleaned, ManagementFileName)
	}

	dir := StaticDir(configFilePath)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, ManagementFileName)
}

// EnsureLatestManagementHTML checks the latest management.html asset and updates the local copy when needed.
// It coalesces concurrent sync attempts and returns whether the asset exists after the sync attempt.
func EnsureLatestManagementHTML(ctx context.Context, staticDir string, proxyURL string, panelRepository string) bool {
	if ctx == nil {
		ctx = context.Background()
	}

	staticDir = strings.TrimSpace(staticDir)
	if staticDir == "" {
		log.Debug("management asset sync skipped: empty static directory")
		return false
	}
	localPath := filepath.Join(staticDir, managementAssetName)

	_, _, _ = sfGroup.Do(localPath, func() (interface{}, error) {
		lastUpdateCheckMu.Lock()
		now := time.Now()
		timeSinceLastAttempt := now.Sub(lastUpdateCheckTime)
		if !lastUpdateCheckTime.IsZero() && timeSinceLastAttempt < managementSyncMinInterval {
			lastUpdateCheckMu.Unlock()
			log.Debugf(
				"management asset sync skipped by throttle: last attempt %v ago (interval %v)",
				timeSinceLastAttempt.Round(time.Second),
				managementSyncMinInterval,
			)
			return nil, nil
		}
		lastUpdateCheckTime = now
		lastUpdateCheckMu.Unlock()

		localFileMissing := false
		if _, errStat := os.Stat(localPath); errStat != nil {
			if errors.Is(errStat, os.ErrNotExist) {
				localFileMissing = true
			} else {
				log.WithError(errStat).Debug("failed to stat local management asset")
			}
		}

		if errMkdirAll := os.MkdirAll(staticDir, 0o755); errMkdirAll != nil {
			log.WithError(errMkdirAll).Warn("failed to prepare static directory for management asset")
			return nil, nil
		}

		repoSource := resolveRepoSource(panelRepository)
		client := newHTTPClient(proxyURL)

		localHash, err := fileSHA256(localPath)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				log.WithError(err).Debug("failed to read local management asset hash")
			}
			localHash = ""
		}

		updated, usedFallback := trySyncFromRepoSource(ctx, client, localPath, localHash, repoSource)
		if updated {
			return nil, nil
		}
		if localFileMissing {
			if usedFallback {
				return nil, nil
			}
			if ensureFallbackManagementHTML(ctx, client, localPath) {
				return nil, nil
			}
		}
		return nil, nil
	})

	_, err := os.Stat(localPath)
	return err == nil
}

func ensureFallbackManagementHTML(ctx context.Context, client *http.Client, localPath string) bool {
	data, downloadedHash, err := downloadAsset(ctx, client, defaultManagementFallbackURL)
	if err != nil {
		log.WithError(err).Warn("failed to download fallback management control panel page")
		return false
	}

	log.Warnf("management asset downloaded from fallback URL without digest verification (hash=%s) — "+
		"enable verified GitHub updates by keeping disable-auto-update-panel set to false", downloadedHash)

	if err = atomicWriteFile(localPath, data); err != nil {
		log.WithError(err).Warn("failed to persist fallback management control panel page")
		return false
	}

	log.Infof("management asset updated from fallback page successfully (hash=%s)", downloadedHash)
	return true
}

func trySyncFromRepoSource(ctx context.Context, client *http.Client, localPath string, localHash string, source repoSource) (bool, bool) {
	if updated, ok := trySyncFromRelease(ctx, client, localPath, localHash, source); ok {
		return updated, false
	}
	if updated, ok := trySyncFromRawURLs(ctx, client, localPath, localHash, source); ok {
		return updated, false
	}
	return false, false
}

func trySyncFromRelease(ctx context.Context, client *http.Client, localPath string, localHash string, source repoSource) (bool, bool) {
	if strings.TrimSpace(source.ReleaseURL) == "" {
		return false, false
	}

	asset, remoteHash, err := fetchLatestAsset(ctx, client, source.ReleaseURL)
	if err != nil {
		log.WithError(err).Warn("failed to fetch latest management release information")
		return false, false
	}

	if remoteHash != "" && localHash != "" && strings.EqualFold(remoteHash, localHash) {
		log.Debug("management asset is already up to date")
		return true, true
	}

	data, downloadedHash, err := downloadAsset(ctx, client, asset.BrowserDownloadURL)
	if err != nil {
		log.WithError(err).Warn("failed to download management asset from release")
		return false, false
	}

	if remoteHash != "" && !strings.EqualFold(remoteHash, downloadedHash) {
		log.Errorf("management asset digest mismatch: expected %s got %s — aborting update for safety", remoteHash, downloadedHash)
		return false, false
	}

	if err = atomicWriteFile(localPath, data); err != nil {
		log.WithError(err).Warn("failed to update management asset on disk")
		return false, false
	}

	log.Infof("management asset updated successfully from release (hash=%s)", downloadedHash)
	return true, true
}

func trySyncFromRawURLs(ctx context.Context, client *http.Client, localPath string, localHash string, source repoSource) (bool, bool) {
	for _, rawURL := range source.RawAssetURLs {
		data, downloadedHash, err := downloadAsset(ctx, client, rawURL)
		if err != nil {
			log.WithError(err).Debugf("failed to download management asset from repository path: %s", rawURL)
			continue
		}
		if localHash != "" && strings.EqualFold(localHash, downloadedHash) {
			log.Debugf("management asset is already up to date from repository path: %s", rawURL)
			return true, true
		}
		if err = atomicWriteFile(localPath, data); err != nil {
			log.WithError(err).Warn("failed to update management asset on disk")
			return false, true
		}
		log.Infof("management asset updated successfully from repository path %s (hash=%s)", rawURL, downloadedHash)
		return true, true
	}
	return false, false
}

func resolveRepoSource(repo string) repoSource {
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return repoSource{ReleaseURL: defaultManagementReleaseURL}
	}

	parsed, err := url.Parse(repo)
	if err != nil || parsed.Host == "" {
		return repoSource{ReleaseURL: defaultManagementReleaseURL}
	}

	host := strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")

	if host == "api.github.com" {
		releaseURL := parsed.String()
		if !strings.HasSuffix(strings.ToLower(parsed.Path), "/releases/latest") {
			releaseURL = strings.TrimSuffix(parsed.String(), "/") + "/releases/latest"
		}
		owner, repoName, ok := extractGitHubRepoFromAPIPath(parsed.Path)
		if !ok {
			return repoSource{ReleaseURL: releaseURL, DisplayReference: repo}
		}
		return repoSource{
			ReleaseURL:       releaseURL,
			RawAssetURLs:     buildGitHubRawAssetURLs(owner, repoName, "main"),
			DisplayReference: repo,
		}
	}

	if host == "github.com" {
		owner, repoName, ok := extractGitHubRepoFromWebPath(parsed.Path)
		if !ok {
			return repoSource{ReleaseURL: defaultManagementReleaseURL, DisplayReference: repo}
		}
		branch := extractGitHubBranchFromWebPath(parsed.Path)
		return repoSource{
			ReleaseURL:       fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repoName),
			RawAssetURLs:     buildGitHubRawAssetURLs(owner, repoName, branch),
			DisplayReference: repo,
		}
	}

	if strings.HasSuffix(strings.ToLower(parsed.Path), ".html") {
		return repoSource{RawAssetURLs: []string{parsed.String()}, DisplayReference: repo}
	}

	return repoSource{ReleaseURL: defaultManagementReleaseURL, DisplayReference: repo}
}

func resolveReleaseURL(repo string) string {
	return resolveRepoSource(repo).ReleaseURL
}

func extractGitHubRepoFromWebPath(path string) (string, string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), true
}

func extractGitHubBranchFromWebPath(path string) string {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) >= 4 && strings.EqualFold(parts[2], "tree") && strings.TrimSpace(parts[3]) != "" {
		return strings.TrimSpace(parts[3])
	}
	if len(parts) >= 4 && strings.EqualFold(parts[2], "blob") && strings.TrimSpace(parts[3]) != "" {
		return strings.TrimSpace(parts[3])
	}
	return "main"
}

func extractGitHubRepoFromAPIPath(path string) (string, string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 3 || !strings.EqualFold(parts[0], "repos") || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func buildGitHubRawAssetURLs(owner string, repoName string, branch string) []string {
	owner = strings.TrimSpace(owner)
	repoName = strings.TrimSpace(repoName)
	branch = strings.TrimSpace(branch)
	if owner == "" || repoName == "" {
		return nil
	}
	if branch == "" {
		branch = "main"
	}
	urls := make([]string, 0, len(defaultRepoAssetCandidates)+1)
	for _, candidate := range defaultRepoAssetCandidates {
		candidate = strings.TrimLeft(strings.TrimSpace(candidate), "/")
		if candidate == "" {
			continue
		}
		urls = append(urls, fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s", owner, repoName, branch, candidate))
	}
	return urls
}

func fetchLatestAsset(ctx context.Context, client *http.Client, releaseURL string) (*releaseAsset, string, error) {
	if strings.TrimSpace(releaseURL) == "" {
		releaseURL = defaultManagementReleaseURL
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releaseURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", httpUserAgent)
	gitURL := strings.ToLower(strings.TrimSpace(os.Getenv("GITSTORE_GIT_URL")))
	if tok := strings.TrimSpace(os.Getenv("GITSTORE_GIT_TOKEN")); tok != "" && strings.Contains(gitURL, "github.com") {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("execute release request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("unexpected release status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var release releaseResponse
	if err = json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, "", fmt.Errorf("decode release response: %w", err)
	}

	for i := range release.Assets {
		asset := &release.Assets[i]
		if strings.EqualFold(asset.Name, managementAssetName) {
			remoteHash := parseDigest(asset.Digest)
			return asset, remoteHash, nil
		}
	}

	return nil, "", fmt.Errorf("management asset %s not found in latest release", managementAssetName)
}

func downloadAsset(ctx context.Context, client *http.Client, downloadURL string) ([]byte, string, error) {
	if strings.TrimSpace(downloadURL) == "" {
		return nil, "", fmt.Errorf("empty download url")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("create download request: %w", err)
	}
	req.Header.Set("User-Agent", httpUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("execute download request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("unexpected download status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAssetDownloadSize+1))
	if err != nil {
		return nil, "", fmt.Errorf("read download body: %w", err)
	}
	if int64(len(data)) > maxAssetDownloadSize {
		return nil, "", fmt.Errorf("download exceeds maximum allowed size of %d bytes", maxAssetDownloadSize)
	}

	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:]), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = file.Close()
	}()

	h := sha256.New()
	if _, err = io.Copy(h, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

func atomicWriteFile(path string, data []byte) error {
	tmpFile, err := os.CreateTemp(filepath.Dir(path), "management-*.html")
	if err != nil {
		return err
	}

	tmpName := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err = tmpFile.Write(data); err != nil {
		return err
	}

	if err = tmpFile.Chmod(0o644); err != nil {
		return err
	}

	if err = tmpFile.Close(); err != nil {
		return err
	}

	if err = os.Rename(tmpName, path); err != nil {
		return err
	}

	return nil
}

func parseDigest(digest string) string {
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return ""
	}

	if idx := strings.Index(digest, ":"); idx >= 0 {
		digest = digest[idx+1:]
	}

	return strings.ToLower(strings.TrimSpace(digest))
}
