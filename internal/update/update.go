package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultGitHubAPIBase = "https://api.github.com"
	defaultRepoOwner     = "qwdingyu"
	defaultRepoName      = "vs-ai-proxy"
	appName              = "vs-ai-proxy"
)

type Options struct {
	CurrentVersion string
	TargetDir      string
	GOOS           string
	GOARCH         string
	Owner          string
	Repo           string
	APIBaseURL     string
	AuthToken      string
	HTTPClient     *http.Client
	ExecutablePath string
}

type CheckResult struct {
	CurrentVersion  string
	LatestVersion   string
	LatestTag       string
	ReleaseURL      string
	AssetName       string
	AssetURL        string
	ChecksumURL     string
	UpdateAvailable bool
}

type DownloadResult struct {
	CheckResult
	ArchivePath string
	BinaryPath  string
	SHA256      string
}

type SelfUpdateResult struct {
	DownloadResult
	ExecutablePath     string
	BackupPath         string
	StagedBinaryPath   string
	NeedsExternalApply bool
}

type releaseResponse struct {
	TagName    string         `json:"tag_name"`
	HTMLURL    string         `json:"html_url"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

func Check(ctx context.Context, opts Options) (CheckResult, error) {
	opts = normalizeOptions(opts)
	current := normalizeVersion(opts.CurrentVersion)
	if current == "" || current == "dev" {
		return CheckResult{CurrentVersion: opts.CurrentVersion}, errors.New("当前版本不是正式 release 版本，无法安全比较更新")
	}

	release, err := fetchLatestRelease(ctx, opts)
	if err != nil {
		return CheckResult{}, err
	}
	if release.TagName == "" {
		return CheckResult{}, errors.New("GitHub latest release 响应缺少 tag_name")
	}

	latest := normalizeVersion(release.TagName)
	asset, checksum := selectAssets(release.Assets, opts.GOOS, opts.GOARCH, latest)
	result := CheckResult{
		CurrentVersion:  opts.CurrentVersion,
		LatestVersion:   latest,
		LatestTag:       release.TagName,
		ReleaseURL:      release.HTMLURL,
		UpdateAvailable: compareVersions(latest, current) > 0,
	}
	if asset.Name != "" {
		result.AssetName = asset.Name
		result.AssetURL = asset.BrowserDownloadURL
	}
	if checksum.Name != "" {
		result.ChecksumURL = checksum.BrowserDownloadURL
	}
	if result.UpdateAvailable && result.AssetURL == "" {
		return result, fmt.Errorf("发现新版本 %s，但没有匹配 %s/%s 的发布资产", release.TagName, opts.GOOS, opts.GOARCH)
	}
	return result, nil
}

func Download(ctx context.Context, opts Options) (DownloadResult, error) {
	opts = normalizeOptions(opts)
	check, err := Check(ctx, opts)
	if err != nil {
		return DownloadResult{}, err
	}
	if !check.UpdateAvailable {
		return DownloadResult{CheckResult: check}, nil
	}

	if err := os.MkdirAll(opts.TargetDir, 0o755); err != nil {
		return DownloadResult{}, fmt.Errorf("创建更新目录失败: %w", err)
	}
	archivePath := filepath.Join(opts.TargetDir, check.AssetName)
	sha, err := downloadFile(ctx, opts.HTTPClient, check.AssetURL, archivePath)
	if err != nil {
		return DownloadResult{}, err
	}
	if check.ChecksumURL != "" {
		if err := verifyChecksum(ctx, opts.HTTPClient, check.ChecksumURL, check.AssetName, sha); err != nil {
			return DownloadResult{}, err
		}
	}

	binaryPath := filepath.Join(opts.TargetDir, binaryName(opts.GOOS))
	if err := extractBinary(archivePath, binaryPath, opts.GOOS); err != nil {
		return DownloadResult{}, err
	}
	return DownloadResult{CheckResult: check, ArchivePath: archivePath, BinaryPath: binaryPath, SHA256: sha}, nil
}

func SelfUpdate(ctx context.Context, opts Options) (SelfUpdateResult, error) {
	opts = normalizeOptions(opts)
	executablePath, err := executablePath(opts.ExecutablePath)
	if err != nil {
		return SelfUpdateResult{}, err
	}

	download, err := Download(ctx, opts)
	if err != nil {
		return SelfUpdateResult{}, err
	}
	result := SelfUpdateResult{DownloadResult: download, ExecutablePath: executablePath}
	if !download.UpdateAvailable {
		return result, nil
	}

	stagePath := executablePath + ".new"
	backupPath := executablePath + ".bak-" + time.Now().UTC().Format("20060102150405")
	if err := copyFile(download.BinaryPath, stagePath, 0o755); err != nil {
		return SelfUpdateResult{}, fmt.Errorf("准备新版二进制失败: %w", err)
	}
	result.StagedBinaryPath = stagePath
	result.BackupPath = backupPath

	if runtime.GOOS == "windows" {
		result.NeedsExternalApply = true
		return result, nil
	}

	if err := replaceExecutable(executablePath, stagePath, backupPath); err != nil {
		return SelfUpdateResult{}, err
	}
	return result, nil
}

func executablePath(override string) (string, error) {
	path := strings.TrimSpace(override)
	if path == "" {
		var err error
		path, err = os.Executable()
		if err != nil {
			return "", fmt.Errorf("获取当前可执行文件路径失败: %w", err)
		}
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != "" {
		path = resolved
	}
	return filepath.Clean(path), nil
}

func replaceExecutable(executablePath, stagePath, backupPath string) error {
	if err := os.Rename(executablePath, backupPath); err != nil {
		return fmt.Errorf("备份当前程序失败: %w", err)
	}
	if err := os.Rename(stagePath, executablePath); err != nil {
		_ = os.Rename(backupPath, executablePath)
		return fmt.Errorf("替换当前程序失败，已尝试回滚: %w", err)
	}
	return nil
}

func LaunchWindowsSelfUpdate(result SelfUpdateResult, args []string) error {
	if !result.NeedsExternalApply {
		return errors.New("当前更新不需要 Windows 延迟替换脚本")
	}
	scriptPath := filepath.Join(filepath.Dir(result.StagedBinaryPath), "vs-ai-proxy-self-update.ps1")
	quotedArgs := powershellStringArray(args)
	script := fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$pidToWait = %d
$exe = %s
$stage = %s
$backup = %s
$argsToPass = @(%s)
while (Get-Process -Id $pidToWait -ErrorAction SilentlyContinue) { Start-Sleep -Milliseconds 200 }
if (Test-Path $backup) { Remove-Item -Force $backup }
if (Test-Path $exe) { Move-Item -Force $exe $backup }
Move-Item -Force $stage $exe
Start-Process -FilePath $exe -ArgumentList $argsToPass -WorkingDirectory %s
`, os.Getpid(), psQuote(result.ExecutablePath), psQuote(result.StagedBinaryPath), psQuote(result.BackupPath), quotedArgs, psQuote(mustGetwd()))
	if err := os.WriteFile(scriptPath, []byte(script), 0o600); err != nil {
		return err
	}
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", scriptPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	return cmd.Start()
}

func normalizeOptions(opts Options) Options {
	if opts.GOOS == "" {
		opts.GOOS = runtime.GOOS
	}
	if opts.GOARCH == "" {
		opts.GOARCH = runtime.GOARCH
	}
	if opts.Owner == "" {
		opts.Owner = defaultRepoOwner
	}
	if opts.Repo == "" {
		opts.Repo = defaultRepoName
	}
	if opts.APIBaseURL == "" {
		opts.APIBaseURL = defaultGitHubAPIBase
	}
	if opts.AuthToken == "" {
		opts.AuthToken = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	}
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 60 * time.Second}
	}
	if opts.TargetDir == "" {
		opts.TargetDir = "."
	}
	return opts
}

func fetchLatestRelease(ctx context.Context, opts Options) (releaseResponse, error) {
	apiBase := strings.TrimRight(opts.APIBaseURL, "/")
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", apiBase, opts.Owner, opts.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return releaseResponse{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", appName+"-updater")
	if opts.AuthToken != "" {
		req.Header.Set("Authorization", "Bearer "+opts.AuthToken)
	}
	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return releaseResponse{}, fmt.Errorf("请求 GitHub Release 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		message := strings.TrimSpace(string(body))
		if resp.StatusCode == http.StatusForbidden && opts.AuthToken == "" && strings.Contains(strings.ToLower(message), "rate limit") {
			return releaseResponse{}, errors.New("GitHub API 匿名访问已触发限流；请稍后重试，或设置 GITHUB_TOKEN 后再执行更新检查")
		}
		return releaseResponse{}, fmt.Errorf("GitHub Release API 返回 %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var release releaseResponse
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return releaseResponse{}, fmt.Errorf("解析 GitHub Release 响应失败: %w", err)
	}
	return release, nil
}

func selectAssets(assets []releaseAsset, goos, goarch, version string) (releaseAsset, releaseAsset) {
	alias := platformAlias(goos, goarch)
	if alias == "" {
		return releaseAsset{}, releaseAsset{}
	}
	version = strings.TrimPrefix(version, "v")
	prefix := fmt.Sprintf("%s-v%s-%s", appName, version, alias)
	var asset releaseAsset
	var checksum releaseAsset
	for _, candidate := range assets {
		name := strings.TrimSpace(candidate.Name)
		if strings.HasPrefix(name, prefix) && (strings.HasSuffix(name, ".tar.gz") || strings.HasSuffix(name, ".zip")) {
			asset = candidate
		}
		if strings.EqualFold(name, "checksums.txt") || strings.EqualFold(name, "SHA256SUMS") || strings.HasSuffix(strings.ToLower(name), ".sha256") {
			checksum = candidate
		}
	}
	return asset, checksum
}

func platformAlias(goos, goarch string) string {
	switch goos + "/" + goarch {
	case "darwin/amd64":
		return "macos-x64"
	case "darwin/arm64":
		return "macos-arm64"
	case "linux/amd64":
		return "linux-x64"
	case "linux/arm64":
		return "linux-arm64"
	case "windows/amd64":
		return "windows-x64"
	default:
		return ""
	}
}

func normalizeVersion(version string) string {
	version = strings.TrimSpace(version)
	version = strings.TrimPrefix(version, "refs/tags/")
	version = strings.TrimPrefix(version, "v")
	if version == "" {
		return ""
	}
	return version
}

func compareVersions(left, right string) int {
	leftParts := parseVersion(left)
	rightParts := parseVersion(right)
	for i := 0; i < len(leftParts) || i < len(rightParts); i++ {
		var leftValue, rightValue int
		if i < len(leftParts) {
			leftValue = leftParts[i]
		}
		if i < len(rightParts) {
			rightValue = rightParts[i]
		}
		if leftValue > rightValue {
			return 1
		}
		if leftValue < rightValue {
			return -1
		}
	}
	return 0
}

func parseVersion(version string) []int {
	version = normalizeVersion(version)
	if cut := strings.IndexAny(version, "-+"); cut >= 0 {
		version = version[:cut]
	}
	parts := strings.Split(version, ".")
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 {
			out = append(out, 0)
			continue
		}
		out = append(out, value)
	}
	return out
}

func downloadFile(ctx context.Context, client *http.Client, url, path string) (string, error) {
	tmpPath := path + ".tmp"
	if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", appName+"-updater")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载更新包失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载更新包返回 HTTP %d", resp.StatusCode)
	}
	file, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(file, hash), resp.Body)
	closeErr := file.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	sha := hex.EncodeToString(hash.Sum(nil))
	if err := os.Rename(tmpPath, path); err != nil {
		return "", err
	}
	return sha, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func verifyChecksum(ctx context.Context, client *http.Client, checksumURL, assetName, actualSHA string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, checksumURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", appName+"-updater")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("下载 checksum 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("下载 checksum 返回 HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		return err
	}
	expected := checksumForAsset(string(data), assetName)
	if expected == "" {
		return fmt.Errorf("checksum 文件中未找到 %s", assetName)
	}
	if !strings.EqualFold(expected, actualSHA) {
		return fmt.Errorf("checksum 不匹配: got %s want %s", actualSHA, expected)
	}
	return nil
}

func checksumForAsset(contents, assetName string) string {
	lines := strings.Split(contents, "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if strings.TrimPrefix(fields[len(fields)-1], "*") == assetName && isSHA256(fields[0]) {
			return strings.ToLower(fields[0])
		}
	}
	return ""
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func extractBinary(archivePath, binaryPath, goos string) error {
	if strings.HasSuffix(archivePath, ".tar.gz") {
		return extractTarGzBinary(archivePath, binaryPath)
	}
	if strings.HasSuffix(archivePath, ".zip") {
		return extractZipBinary(archivePath, binaryPath, binaryName(goos))
	}
	return fmt.Errorf("不支持的更新包格式: %s", filepath.Base(archivePath))
}

func extractTarGzBinary(archivePath, binaryPath string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gzipReader.Close()
	tarReader := tar.NewReader(gzipReader)
	candidates := []tarEntry{}
	for {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(header.Name)
		if base != appName {
			continue
		}
		data, err := io.ReadAll(tarReader)
		if err != nil {
			return err
		}
		candidates = append(candidates, tarEntry{Name: header.Name, Mode: header.FileInfo().Mode(), Data: data})
	}
	if len(candidates) == 0 {
		return fmt.Errorf("更新包中未找到 %s 可执行文件", appName)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	entry := candidates[0]
	tmpPath := binaryPath + ".tmp"
	mode := entry.Mode.Perm()
	if mode == 0 {
		mode = 0o755
	}
	if mode&0o111 == 0 {
		mode |= 0o755
	}
	if err := os.WriteFile(tmpPath, entry.Data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, binaryPath); err != nil {
		return err
	}
	return nil
}

type tarEntry struct {
	Name string
	Mode os.FileMode
	Data []byte
}

func extractZipBinary(archivePath, binaryPath, wantName string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer reader.Close()

	var candidates []zipEntry
	for _, file := range reader.File {
		if file.FileInfo().IsDir() || filepath.Base(file.Name) != wantName {
			continue
		}
		readCloser, err := file.Open()
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(readCloser)
		closeErr := readCloser.Close()
		if readErr != nil {
			return readErr
		}
		if closeErr != nil {
			return closeErr
		}
		candidates = append(candidates, zipEntry{Name: file.Name, Mode: file.FileInfo().Mode(), Data: data})
	}
	if len(candidates) == 0 {
		return fmt.Errorf("更新包中未找到 %s 可执行文件", wantName)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	entry := candidates[0]
	tmpPath := binaryPath + ".tmp"
	mode := entry.Mode.Perm()
	if mode == 0 {
		mode = 0o755
	}
	if mode&0o111 == 0 {
		mode |= 0o755
	}
	if err := os.WriteFile(tmpPath, entry.Data, mode); err != nil {
		return err
	}
	return os.Rename(tmpPath, binaryPath)
}

type zipEntry struct {
	Name string
	Mode os.FileMode
	Data []byte
}

func binaryName(goos string) string {
	if goos == "windows" {
		return appName + ".exe"
	}
	return appName
}

func psQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func powershellStringArray(values []string) string {
	if len(values) == 0 {
		return ""
	}
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, psQuote(value))
	}
	return strings.Join(quoted, ",")
}

func mustGetwd() string {
	dir, err := os.Getwd()
	if err != nil || dir == "" {
		return "."
	}
	return dir
}
