package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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

func TestDownloadOverwritesExistingArchiveAndExtractedBinary(t *testing.T) {
	archiveName := "vs-ai-proxy-v0.2.61-windows-x64.exe.zip"
	archive := buildZip(t, "windows-x64/vs-ai-proxy.exe", []byte("new-windows-binary"))
	sha := sha256.Sum256(archive)
	shaHex := hex.EncodeToString(sha[:])

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/qwdingyu/vs-ai-proxy/releases/latest":
			_, _ = fmt.Fprintf(w, `{"tag_name":"v0.2.61","html_url":"https://example.invalid/release","assets":[{"name":"%s","browser_download_url":"%s/asset"},{"name":"checksums.txt","browser_download_url":"%s/checksums"}]}`, archiveName, server.URL, server.URL)
		case "/asset":
			_, _ = w.Write(archive)
		case "/checksums":
			_, _ = fmt.Fprintf(w, "%s  %s\n", shaHex, archiveName)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	targetDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(targetDir, archiveName), []byte("old-archive"), 0o644); err != nil {
		t.Fatalf("WriteFile(old archive) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, "vs-ai-proxy.exe"), []byte("old-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile(old binary) error = %v", err)
	}

	result, err := Download(context.Background(), Options{
		CurrentVersion: "v0.2.60",
		TargetDir:      targetDir,
		APIBaseURL:     server.URL,
		GOOS:           "windows",
		GOARCH:         "amd64",
	})
	if err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	if result.SHA256 != shaHex {
		t.Fatalf("SHA256 = %q, want %q", result.SHA256, shaHex)
	}
	storedArchive, err := os.ReadFile(filepath.Join(targetDir, archiveName))
	if err != nil {
		t.Fatalf("ReadFile(archive) error = %v", err)
	}
	if !bytes.Equal(storedArchive, archive) {
		t.Fatalf("archive was not replaced with the downloaded content")
	}
	extracted, err := os.ReadFile(filepath.Join(targetDir, "vs-ai-proxy.exe"))
	if err != nil {
		t.Fatalf("ReadFile(binary) error = %v", err)
	}
	if string(extracted) != "new-windows-binary" {
		t.Fatalf("binary data = %q, want new-windows-binary", string(extracted))
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

func TestDownloadErrorPreservesManualAssetURL(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/qwdingyu/vs-ai-proxy/releases/latest":
			_, _ = fmt.Fprintf(
				w,
				`{"tag_name":"v0.2.58","html_url":"https://github.com/qwdingyu/vs-ai-proxy/releases/tag/v0.2.58","assets":[{"name":"vs-ai-proxy-v0.2.58-windows-x64.exe.zip","browser_download_url":"%s/asset"},{"name":"checksums.txt","browser_download_url":"%s/checksums"}]}`,
				server.URL,
				server.URL,
			)
		case "/asset":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("temporarily unavailable"))
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	_, err := Download(context.Background(), Options{
		CurrentVersion: "v0.2.57",
		TargetDir:      t.TempDir(),
		APIBaseURL:     server.URL,
		GOOS:           "windows",
		GOARCH:         "amd64",
	})
	var downloadErr *DownloadError
	if !errors.As(err, &downloadErr) {
		t.Fatalf("Download() error = %T %[1]v, want DownloadError", err)
	}
	if downloadErr.CheckResult.AssetURL != server.URL+"/asset" {
		t.Fatalf("AssetURL = %q, want preserved manual asset URL", downloadErr.CheckResult.AssetURL)
	}
	if downloadErr.CheckResult.ReleaseURL != "https://github.com/qwdingyu/vs-ai-proxy/releases/tag/v0.2.58" {
		t.Fatalf("ReleaseURL = %q, want preserved release URL", downloadErr.CheckResult.ReleaseURL)
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

func TestReplaceStagedUpdateFileRollsBackExistingTargetWhenInstallFails(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "vs-ai-proxy.tmp")
	targetPath := filepath.Join(dir, "vs-ai-proxy")
	backupPath := targetPath + ".bak"
	if err := os.WriteFile(tmpPath, []byte("new"), 0o755); err != nil {
		t.Fatalf("WriteFile(tmp) error = %v", err)
	}
	if err := os.WriteFile(targetPath, []byte("old"), 0o755); err != nil {
		t.Fatalf("WriteFile(target) error = %v", err)
	}
	if err := os.WriteFile(backupPath, []byte("stale-backup"), 0o755); err != nil {
		t.Fatalf("WriteFile(stale backup) error = %v", err)
	}

	oldRenameFile := renameFile
	t.Cleanup(func() { renameFile = oldRenameFile })
	renameFile = func(source, target string) error {
		if source == tmpPath && target == targetPath {
			return errors.New("forced target replacement failure")
		}
		return os.Rename(source, target)
	}

	err := replaceStagedUpdateFile(tmpPath, targetPath)
	if err == nil || !strings.Contains(err.Error(), "已回滚旧文件") {
		t.Fatalf("replaceStagedUpdateFile() error = %v, want rollback error", err)
	}
	restored, readErr := os.ReadFile(targetPath)
	if readErr != nil {
		t.Fatalf("ReadFile(target) error = %v", readErr)
	}
	if string(restored) != "old" {
		t.Fatalf("target content = %q, want old content restored", string(restored))
	}
	if _, statErr := os.Stat(backupPath); statErr != nil {
		t.Fatalf("backup stat error = %v, want stale backup preserved untouched", statErr)
	}
	staleBackup, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("ReadFile(stale backup) error = %v", err)
	}
	if string(staleBackup) != "stale-backup" {
		t.Fatalf("stale backup content = %q, want preserved", string(staleBackup))
	}
}

func TestReplaceStagedUpdateFileRejectsDirectoryTarget(t *testing.T) {
	dir := t.TempDir()
	tmpPath := filepath.Join(dir, "vs-ai-proxy.tmp")
	targetPath := filepath.Join(dir, "vs-ai-proxy")
	if err := os.WriteFile(tmpPath, []byte("new"), 0o755); err != nil {
		t.Fatalf("WriteFile(tmp) error = %v", err)
	}
	if err := os.Mkdir(targetPath, 0o755); err != nil {
		t.Fatalf("Mkdir(target) error = %v", err)
	}

	err := replaceStagedUpdateFile(tmpPath, targetPath)
	if err == nil || !strings.Contains(err.Error(), "是目录") {
		t.Fatalf("replaceStagedUpdateFile() error = %v, want directory rejection", err)
	}
}

func TestSelfUpdateInstallsOnCurrentPlatformWithoutExternalApply(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses delayed PowerShell replacement and is covered by script generation tests")
	}
	platform := platformAlias(runtime.GOOS, runtime.GOARCH)
	if platform == "" {
		t.Skipf("unsupported release asset platform for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	archiveName := fmt.Sprintf("vs-ai-proxy-v0.2.61-%s.tar.gz", platform)
	archive := buildTarGz(t, platform+"/vs-ai-proxy", []byte("new-current-platform-binary"))
	sha := sha256.Sum256(archive)
	shaHex := hex.EncodeToString(sha[:])
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/qwdingyu/vs-ai-proxy/releases/latest":
			_, _ = fmt.Fprintf(w, `{"tag_name":"v0.2.61","html_url":"https://example.invalid/release","assets":[{"name":"%s","browser_download_url":"%s/asset"},{"name":"checksums.txt","browser_download_url":"%s/checksums"}]}`, archiveName, server.URL, server.URL)
		case "/asset":
			_, _ = w.Write(archive)
		case "/checksums":
			_, _ = fmt.Fprintf(w, "%s  %s\n", shaHex, archiveName)
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	targetDir := t.TempDir()
	executablePath := filepath.Join(t.TempDir(), binaryName(runtime.GOOS))
	if err := os.WriteFile(executablePath, []byte("old-current-platform-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile(current executable) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(targetDir, binaryName(runtime.GOOS)), []byte("stale-extracted-binary"), 0o755); err != nil {
		t.Fatalf("WriteFile(stale extracted binary) error = %v", err)
	}

	result, err := SelfUpdate(context.Background(), Options{
		CurrentVersion: "v0.2.60",
		TargetDir:      targetDir,
		APIBaseURL:     server.URL,
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
		ExecutablePath: executablePath,
	})
	if err != nil {
		t.Fatalf("SelfUpdate() error = %v", err)
	}
	if !result.UpdateAvailable || result.NeedsExternalApply {
		t.Fatalf("result = %#v, want installed update without external apply", result)
	}
	installed, err := os.ReadFile(executablePath)
	if err != nil {
		t.Fatalf("ReadFile(installed executable) error = %v", err)
	}
	backup, err := os.ReadFile(result.BackupPath)
	if err != nil {
		t.Fatalf("ReadFile(backup) error = %v", err)
	}
	if string(installed) != "new-current-platform-binary" || string(backup) != "old-current-platform-binary" {
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
		"('[{0}] {1}' -f (Get-Date -Format o), $message)",
		"$label + ' 不存在",
		"新版暂存文件为空",
		"已重试 20 次",
		"新版暂存文件仍存在",
		"ERROR_RECORD",
		"ERROR_STACK",
		"ERROR_POSITION",
		"rollback restored backup",
		"Write-UpdateLog ('rollback failed: ' + $_.Exception.Message)",
		"--config",
		`C:\cfg\config.json`,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	for _, bad := range []string{
		`Write-UpdateLog "`,
		`throw "`,
		`$($_.Exception.Message)`,
		`$($_ | Out-String)`,
	} {
		if strings.Contains(script, bad) {
			t.Fatalf("script contains parser-fragile form %q:\n%s", bad, script)
		}
	}
}

func TestWriteWindowsSelfUpdateScriptFileReplacesStaleScript(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "vs-ai-proxy-self-update.ps1")
	oldScript := "Write-UpdateLog \"rollback failed: $($_.Exception.Message)\n" + strings.Repeat("old-tail", 64)
	newScript := "Write-UpdateLog ('rollback failed: ' + $_.Exception.Message)\n"
	if err := os.WriteFile(scriptPath, []byte(oldScript), 0o600); err != nil {
		t.Fatalf("write stale script: %v", err)
	}

	if err := writeWindowsSelfUpdateScriptFile(scriptPath, newScript); err != nil {
		t.Fatalf("writeWindowsSelfUpdateScriptFile() error = %v", err)
	}

	got, err := os.ReadFile(scriptPath)
	if err != nil {
		t.Fatalf("read replaced script: %v", err)
	}
	if string(got) != windowsPowerShellBOM+newScript {
		t.Fatalf("script content = %q, want exact BOM-prefixed new script %q", string(got), windowsPowerShellBOM+newScript)
	}
	if !strings.HasPrefix(string(got), windowsPowerShellBOM) {
		t.Fatalf("script should include UTF-8 BOM for Windows PowerShell 5.1 compatibility")
	}
	temps, err := filepath.Glob(filepath.Join(dir, "vs-ai-proxy-self-update.ps1.*.tmp"))
	if err != nil {
		t.Fatalf("glob temp scripts: %v", err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary scripts left behind: %v", temps)
	}
}

func TestAppendWindowsSelfUpdateLogPersistsLauncherErrors(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "vs-ai-proxy-self-update.log")

	if err := appendWindowsSelfUpdateLog(logPath, "launcher prepared script=C:\\apps\\vs-ai-proxy-self-update.ps1"); err != nil {
		t.Fatalf("appendWindowsSelfUpdateLog() error = %v", err)
	}
	if err := appendWindowsSelfUpdateLog(logPath, "launcher failed to start powershell: access denied"); err != nil {
		t.Fatalf("appendWindowsSelfUpdateLog() second error = %v", err)
	}

	content, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(log) error = %v", err)
	}
	for _, want := range []string{
		"launcher prepared script=",
		"launcher failed to start powershell: access denied",
	} {
		if !strings.Contains(string(content), want) {
			t.Fatalf("log content = %q, want %q", string(content), want)
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

func buildZip(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zipWriter := zip.NewWriter(&buf)
	header := &zip.FileHeader{Name: name, Method: zip.Deflate}
	header.SetMode(0o755)
	entry, err := zipWriter.CreateHeader(header)
	if err != nil {
		t.Fatalf("zip CreateHeader() error = %v", err)
	}
	if _, err := entry.Write(data); err != nil {
		t.Fatalf("zip Write() error = %v", err)
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatalf("zip Close() error = %v", err)
	}
	return buf.Bytes()
}
