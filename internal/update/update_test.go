package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		left  string
		right string
		want  int
	}{
		{left: "0.2.13", right: "0.2.12", want: 1},
		{left: "v0.2.13", right: "0.2.13", want: 0},
		{left: "0.2.9", right: "0.2.10", want: -1},
		{left: "1.0.0", right: "0.9.9", want: 1},
	}
	for _, tt := range tests {
		got := compareVersions(tt.left, tt.right)
		if got != tt.want {
			t.Fatalf("compareVersions(%q, %q) = %d, want %d", tt.left, tt.right, got, tt.want)
		}
	}
}

func TestCheckSelectsMatchingReleaseAsset(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/qwdingyu/vs-ai-proxy/releases/latest" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"tag_name":"v0.2.13",
			"html_url":"https://github.com/qwdingyu/vs-ai-proxy/releases/tag/v0.2.13",
			"assets":[
				{"name":"vs-ai-proxy-v0.2.13-linux-x64.tar.gz","browser_download_url":"https://example.invalid/linux"},
				{"name":"vs-ai-proxy-v0.2.13-macos-arm64.tar.gz","browser_download_url":"https://example.invalid/macos"},
				{"name":"checksums.txt","browser_download_url":"https://example.invalid/checksums"}
			]
		}`))
	}))
	defer server.Close()

	result, err := Check(context.Background(), Options{
		CurrentVersion: "0.2.12",
		APIBaseURL:     server.URL,
		GOOS:           "linux",
		GOARCH:         "amd64",
	})
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !result.UpdateAvailable {
		t.Fatalf("UpdateAvailable = false, want true")
	}
	if result.AssetName != "vs-ai-proxy-v0.2.13-linux-x64.tar.gz" {
		t.Fatalf("AssetName = %q", result.AssetName)
	}
}

func TestCheckCanUseStaticManifestWithRelativeAssetURLs(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/updates/latest.json" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"tag_name":"v0.2.13",
			"html_url":"https://intranet.example/vs-ai-proxy/v0.2.13",
			"assets":[
				{"name":"vs-ai-proxy-v0.2.13-windows-x64.exe.zip","browser_download_url":"./vs-ai-proxy-v0.2.13-windows-x64.exe.zip"},
				{"name":"checksums.txt","browser_download_url":"checksums.txt"}
			]
		}`))
	}))
	defer server.Close()

	result, err := Check(context.Background(), Options{
		CurrentVersion: "0.2.12",
		ManifestURL:    server.URL + "/updates/latest.json",
		GOOS:           "windows",
		GOARCH:         "amd64",
	})
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !result.UpdateAvailable {
		t.Fatalf("UpdateAvailable = false, want true")
	}
	if result.AssetURL != server.URL+"/updates/vs-ai-proxy-v0.2.13-windows-x64.exe.zip" {
		t.Fatalf("AssetURL = %q, want manifest-relative asset URL", result.AssetURL)
	}
	if result.ChecksumURL != server.URL+"/updates/checksums.txt" {
		t.Fatalf("ChecksumURL = %q, want manifest-relative checksum URL", result.ChecksumURL)
	}
}

func TestCheckUsesManifestURLFromEnvironment(t *testing.T) {
	var requested bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = true
		_, _ = w.Write([]byte(`{
			"tag_name":"v0.2.13",
			"html_url":"https://intranet.example/vs-ai-proxy/v0.2.13",
			"assets":[
				{"name":"vs-ai-proxy-v0.2.13-linux-x64.tar.gz","browser_download_url":"https://intranet.example/vs-ai-proxy/v0.2.13/linux"},
				{"name":"checksums.txt","browser_download_url":"https://intranet.example/vs-ai-proxy/v0.2.13/checksums"}
			]
		}`))
	}))
	defer server.Close()
	t.Setenv("VS_AI_PROXY_UPDATE_MANIFEST_URL", server.URL+"/latest.json")

	result, err := Check(context.Background(), Options{
		CurrentVersion: "0.2.12",
		GOOS:           "linux",
		GOARCH:         "amd64",
	})
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if !requested || result.AssetName != "vs-ai-proxy-v0.2.13-linux-x64.tar.gz" {
		t.Fatalf("requested=%v result=%#v, want environment manifest source", requested, result)
	}
}

func TestCheckRetriesWhenLatestReleaseAssetIsTemporarilyMissing(t *testing.T) {
	oldWait := releaseAssetWait
	releaseAssetWait = time.Millisecond
	t.Cleanup(func() { releaseAssetWait = oldWait })

	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/qwdingyu/vs-ai-proxy/releases/latest" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		call := atomic.AddInt32(&calls, 1)
		if call == 1 {
			_, _ = w.Write([]byte(`{"tag_name":"v0.2.47","html_url":"https://example.invalid/release","assets":[]}`))
			return
		}
		_, _ = w.Write([]byte(`{"tag_name":"v0.2.47","html_url":"https://example.invalid/release","assets":[{"name":"vs-ai-proxy-v0.2.47-windows-x64.exe.zip","browser_download_url":"https://example.invalid/windows"},{"name":"checksums.txt","browser_download_url":"https://example.invalid/checksums"}]}`))
	}))
	defer server.Close()

	result, err := Check(context.Background(), Options{
		CurrentVersion: "v0.2.46",
		APIBaseURL:     server.URL,
		GOOS:           "windows",
		GOARCH:         "amd64",
	})
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.AssetName != "vs-ai-proxy-v0.2.47-windows-x64.exe.zip" || !result.UpdateAvailable {
		t.Fatalf("result = %#v, want retried matching Windows asset", result)
	}
	if atomic.LoadInt32(&calls) < 2 {
		t.Fatalf("calls = %d, want retry", calls)
	}
}

func TestCheckRetriesWhenChecksumAssetIsTemporarilyMissing(t *testing.T) {
	oldWait := releaseAssetWait
	releaseAssetWait = time.Millisecond
	t.Cleanup(func() { releaseAssetWait = oldWait })

	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/qwdingyu/vs-ai-proxy/releases/latest" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		call := atomic.AddInt32(&calls, 1)
		assets := `[{"name":"vs-ai-proxy-v0.2.47-windows-x64.exe.zip","browser_download_url":"https://example.invalid/windows"}]`
		if call > 1 {
			assets = `[{"name":"vs-ai-proxy-v0.2.47-windows-x64.exe.zip","browser_download_url":"https://example.invalid/windows"},{"name":"checksums.txt","browser_download_url":"https://example.invalid/checksums"}]`
		}
		_, _ = fmt.Fprintf(w, `{"tag_name":"v0.2.47","html_url":"https://example.invalid/release","assets":%s}`, assets)
	}))
	defer server.Close()

	result, err := Check(context.Background(), Options{
		CurrentVersion: "v0.2.46",
		APIBaseURL:     server.URL,
		GOOS:           "windows",
		GOARCH:         "amd64",
	})
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if result.ChecksumURL != "https://example.invalid/checksums" {
		t.Fatalf("ChecksumURL = %q, want retried checksum asset", result.ChecksumURL)
	}
	if atomic.LoadInt32(&calls) < 2 {
		t.Fatalf("calls = %d, want retry", calls)
	}
}

func TestCheckReportsExpectedAssetAndActualAssetsWhenNoMatchingReleaseAsset(t *testing.T) {
	oldWait := releaseAssetWait
	releaseAssetWait = time.Millisecond
	t.Cleanup(func() { releaseAssetWait = oldWait })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/qwdingyu/vs-ai-proxy/releases/latest" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{
			"tag_name":"v0.2.47",
			"html_url":"https://example.invalid/release",
			"assets":[
				{"name":"vs-ai-proxy-v0.2.47-linux-x64.tar.gz","browser_download_url":"https://example.invalid/linux"},
				{"name":"checksums.txt","browser_download_url":"https://example.invalid/checksums"}
			]
		}`))
	}))
	defer server.Close()

	_, err := Check(context.Background(), Options{
		CurrentVersion: "v0.2.46",
		APIBaseURL:     server.URL,
		GOOS:           "windows",
		GOARCH:         "amd64",
	})
	if err == nil {
		t.Fatalf("Check() error = nil, want missing asset diagnostic")
	}
	message := err.Error()
	for _, want := range []string{
		`期望资产前缀 "vs-ai-proxy-v0.2.47-windows-x64"`,
		"vs-ai-proxy-v0.2.47-linux-x64.tar.gz",
		"GitHub Release 资产/CDN 尚未完全可见",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("error = %q, want contains %q", message, want)
		}
	}
}

func TestCheckUsesAuthTokenWhenProvided(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer token-123" {
			t.Fatalf("Authorization = %q, want bearer token", got)
		}
		_, _ = w.Write([]byte(`{
			"tag_name":"v0.2.13",
			"html_url":"https://github.com/qwdingyu/vs-ai-proxy/releases/tag/v0.2.13",
			"assets":[{"name":"vs-ai-proxy-v0.2.13-linux-x64.tar.gz","browser_download_url":"https://example.invalid/linux"},{"name":"checksums.txt","browser_download_url":"https://example.invalid/checksums"}]
		}`))
	}))
	defer server.Close()

	_, err := Check(context.Background(), Options{
		CurrentVersion: "0.2.12",
		APIBaseURL:     server.URL,
		AuthToken:      "token-123",
		GOOS:           "linux",
		GOARCH:         "amd64",
	})
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
}

func TestCheckReportsFriendlyRateLimitWithoutToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want empty", got)
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"API rate limit exceeded"}`))
	}))
	defer server.Close()

	_, err := Check(context.Background(), Options{
		CurrentVersion: "0.2.12",
		APIBaseURL:     server.URL,
		AuthToken:      "",
		GOOS:           "linux",
		GOARCH:         "amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("Check() error = %v, want GITHUB_TOKEN hint", err)
	}
}

func TestDownloadFetchesAndExtractsTarGzAsset(t *testing.T) {
	archive := buildTarGz(t, "linux-x64/vs-ai-proxy", []byte("new-binary"))
	sha := sha256.Sum256(archive)
	shaHex := hex.EncodeToString(sha[:])

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/qwdingyu/vs-ai-proxy/releases/latest":
			_, _ = fmt.Fprintf(w, `{"tag_name":"v0.2.13","html_url":"https://example.invalid/release","assets":[{"name":"vs-ai-proxy-v0.2.13-linux-x64.tar.gz","browser_download_url":"%s/asset"},{"name":"checksums.txt","browser_download_url":"%s/checksums"}]}`, server.URL, server.URL)
		case "/asset":
			_, _ = w.Write(archive)
		case "/checksums":
			_, _ = fmt.Fprintf(w, "%s  vs-ai-proxy-v0.2.13-linux-x64.tar.gz\n", shaHex)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	targetDir := t.TempDir()
	result, err := Download(context.Background(), Options{
		CurrentVersion: "0.2.12",
		TargetDir:      targetDir,
		APIBaseURL:     server.URL,
		GOOS:           "linux",
		GOARCH:         "amd64",
	})
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if result.SHA256 != shaHex {
		t.Fatalf("SHA256 = %q, want %q", result.SHA256, shaHex)
	}
	data, err := os.ReadFile(filepath.Join(targetDir, "vs-ai-proxy"))
	if err != nil {
		t.Fatalf("ReadFile(binary) error = %v", err)
	}
	if string(data) != "new-binary" {
		t.Fatalf("binary data = %q", string(data))
	}
}

func TestDownloadRejectsReleaseWithoutChecksumAsset(t *testing.T) {
	oldWait := releaseAssetWait
	releaseAssetWait = time.Millisecond
	t.Cleanup(func() { releaseAssetWait = oldWait })

	assetRequested := false
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/qwdingyu/vs-ai-proxy/releases/latest":
			_, _ = fmt.Fprintf(
				w,
				`{"tag_name":"v0.2.13","html_url":"https://example.invalid/release","assets":[{"name":"vs-ai-proxy-v0.2.13-linux-x64.tar.gz","browser_download_url":"%s/asset"}]}`,
				server.URL,
			)
		case "/asset":
			assetRequested = true
			_, _ = w.Write([]byte("unverified-archive"))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	_, err := Download(context.Background(), Options{
		CurrentVersion: "0.2.12",
		TargetDir:      t.TempDir(),
		APIBaseURL:     server.URL,
		GOOS:           "linux",
		GOARCH:         "amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("Download() error = %v, want missing checksum error", err)
	}
	if assetRequested {
		t.Fatalf("release asset was downloaded before checksum availability was verified")
	}
}

func TestDownloadRejectsChecksumMismatch(t *testing.T) {
	archive := buildTarGz(t, "linux-x64/vs-ai-proxy", []byte("new-binary"))
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/qwdingyu/vs-ai-proxy/releases/latest":
			_, _ = fmt.Fprintf(
				w,
				`{"tag_name":"v0.2.13","html_url":"https://example.invalid/release","assets":[{"name":"vs-ai-proxy-v0.2.13-linux-x64.tar.gz","browser_download_url":"%s/asset"},{"name":"checksums.txt","browser_download_url":"%s/checksums"}]}`,
				server.URL,
				server.URL,
			)
		case "/asset":
			_, _ = w.Write(archive)
		case "/checksums":
			_, _ = fmt.Fprintf(
				w,
				"%s  vs-ai-proxy-v0.2.13-linux-x64.tar.gz\n",
				strings.Repeat("0", 64),
			)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	targetDir := t.TempDir()
	_, err := Download(context.Background(), Options{
		CurrentVersion: "0.2.12",
		TargetDir:      targetDir,
		APIBaseURL:     server.URL,
		GOOS:           "linux",
		GOARCH:         "amd64",
	})
	if err == nil || !strings.Contains(err.Error(), "checksum 不匹配") {
		t.Fatalf("Download() error = %v, want checksum mismatch", err)
	}
	if _, statErr := os.Stat(filepath.Join(targetDir, "vs-ai-proxy")); !os.IsNotExist(statErr) {
		t.Fatalf("unverified binary should not be extracted, stat error = %v", statErr)
	}
}

func TestExtractZipBinary(t *testing.T) {
	archivePath := filepath.Join(t.TempDir(), "update.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	zipWriter := zip.NewWriter(file)
	entry, err := zipWriter.Create("windows-x64/vs-ai-proxy.exe")
	if err != nil {
		t.Fatalf("zip Create() error = %v", err)
	}
	_, _ = entry.Write([]byte("windows-binary"))
	if err := zipWriter.Close(); err != nil {
		t.Fatalf("zip Close() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("file Close() error = %v", err)
	}

	binaryPath := filepath.Join(t.TempDir(), "vs-ai-proxy.exe")
	if err := extractBinary(archivePath, binaryPath, "windows"); err != nil {
		t.Fatalf("extractBinary() error = %v", err)
	}
	data, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != "windows-binary" {
		t.Fatalf("binary data = %q", string(data))
	}
}

func TestReplaceExecutableBacksUpAndInstallsStage(t *testing.T) {
	dir := t.TempDir()
	executablePath := filepath.Join(dir, "vs-ai-proxy")
	stagePath := executablePath + ".new"
	backupPath := executablePath + ".bak"
	if err := os.WriteFile(executablePath, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile(old) error = %v", err)
	}
	if err := os.WriteFile(stagePath, []byte("new"), 0o755); err != nil {
		t.Fatalf("WriteFile(new) error = %v", err)
	}

	if err := replaceExecutable(executablePath, stagePath, backupPath); err != nil {
		t.Fatalf("replaceExecutable() error = %v", err)
	}
	installed, err := os.ReadFile(executablePath)
	if err != nil {
		t.Fatalf("ReadFile(installed) error = %v", err)
	}
	backup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("ReadFile(backup) error = %v", err)
	}
	if string(installed) != "new" || string(backup) != "old" {
		t.Fatalf("installed/backup = %q/%q, want new/old", string(installed), string(backup))
	}
}

func TestWindowsSelfUpdateScriptIncludesPreflightRetryRollbackAndCleanupChecks(t *testing.T) {
	result := SelfUpdateResult{
		ExecutablePath:     `C:\apps\vs-ai-proxy.exe`,
		StagedBinaryPath:   `C:\apps\vs-ai-proxy.exe.new`,
		BackupPath:         `C:\apps\vs-ai-proxy.exe.bak-20260712000102`,
		NeedsExternalApply: true,
	}

	script := windowsSelfUpdateScript(result, []string{"--config", `C:\cfg\config.json`}, `C:\apps\vs-ai-proxy-self-update.log`, 1234, `C:\apps`)
	for _, want := range []string{
		"function Write-UpdateLog",
		"function Assert-PathExists",
		"function Move-WithRetry",
		"$label 不存在",
		"新版暂存文件为空",
		"已重试 20 次",
		"新版暂存文件仍存在",
		"rollback restored backup",
		"--config",
		`C:\cfg\config.json`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
}

func buildTarGz(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gzipWriter := gzip.NewWriter(&buf)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}); err != nil {
		t.Fatalf("WriteHeader() error = %v", err)
	}
	if _, err := tarWriter.Write(data); err != nil {
		t.Fatalf("tar Write() error = %v", err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatalf("tar Close() error = %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("gzip Close() error = %v", err)
	}
	return buf.Bytes()
}
