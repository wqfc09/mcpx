package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRunDownloadsVerifiesAndInstallsRelease(t *testing.T) {
	archive := tarGzipArchive(t, "mcpx", []byte("new mcpx binary"))
	assetName := "mcpx_0.5.0_linux_amd64.tar.gz"
	checksum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(checksum[:]), assetName)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/opentokenz/mcpx/releases/latest":
			writeRelease(t, w, "v0.5.0", serverURL(r)+"/download/"+assetName, serverURL(r)+"/download/checksums.txt", assetName)
		case "/download/" + assetName:
			_, _ = w.Write(archive)
		case "/download/checksums.txt":
			_, _ = io.WriteString(w, checksums)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var verifiedVersion string
	var installed []byte
	result, err := Run(context.Background(), Options{
		CurrentVersion: "0.4.1",
		APIBaseURL:     server.URL,
		HTTPClient:     server.Client(),
		GOOS:           "linux",
		GOARCH:         "amd64",
		ExecutablePath: "/opt/mcpx",
		VerifyBinary: func(path, version string) error {
			verifiedVersion = version
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if string(content) != "new mcpx binary" {
				return fmt.Errorf("unexpected extracted content %q", content)
			}
			return nil
		},
		Install: func(currentPath, newPath string) error {
			if currentPath != "/opt/mcpx" {
				return fmt.Errorf("unexpected current path %q", currentPath)
			}
			var err error
			installed, err = os.ReadFile(newPath)
			return err
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.TargetVersion != "0.5.0" || result.AssetName != assetName || result.InstalledPath != "/opt/mcpx" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if verifiedVersion != "0.5.0" || string(installed) != "new mcpx binary" {
		t.Fatalf("verify/install did not receive selected release: version=%q installed=%q", verifiedVersion, installed)
	}
}

func TestRunCheckOnlyDoesNotDownloadAssets(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/opentokenz/mcpx/releases/latest":
			writeRelease(t, w, "v0.5.0", serverURL(r)+"/asset", serverURL(r)+"/checksums", "mcpx_0.5.0_darwin_arm64.tar.gz")
		default:
			downloads.Add(1)
			_, _ = io.WriteString(w, "unexpected")
		}
	}))
	defer server.Close()

	result, err := Run(context.Background(), Options{
		CurrentVersion: "0.4.1",
		CheckOnly:      true,
		APIBaseURL:     server.URL,
		HTTPClient:     server.Client(),
		GOOS:           "darwin",
		GOARCH:         "arm64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.UpToDate || !result.CheckedOnly || result.TargetVersion != "0.5.0" {
		t.Fatalf("unexpected check result: %+v", result)
	}
	if downloads.Load() != 0 {
		t.Fatalf("check-only downloaded %d asset(s)", downloads.Load())
	}
}

func TestRunReportsUpToDateWithoutDownloading(t *testing.T) {
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/opentokenz/mcpx/releases/latest" {
			writeRelease(t, w, "v0.5.0", serverURL(r)+"/asset", serverURL(r)+"/checksums", "mcpx_0.5.0_linux_amd64.tar.gz")
			return
		}
		downloads.Add(1)
		http.NotFound(w, r)
	}))
	defer server.Close()

	result, err := Run(context.Background(), Options{
		CurrentVersion: "v0.5.0",
		APIBaseURL:     server.URL,
		HTTPClient:     server.Client(),
		GOOS:           "linux",
		GOARCH:         "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.UpToDate || result.TargetVersion != "0.5.0" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if downloads.Load() != 0 {
		t.Fatalf("up-to-date check downloaded %d asset(s)", downloads.Load())
	}
}

func TestRunSpecificVersionUsesTagEndpoint(t *testing.T) {
	var requestedPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestedPath = r.URL.Path
		writeRelease(t, w, "v0.4.9", serverURL(r)+"/asset", serverURL(r)+"/checksums", "mcpx_0.4.9_linux_amd64.tar.gz")
	}))
	defer server.Close()

	result, err := Run(context.Background(), Options{
		CurrentVersion: "0.4.1",
		TargetVersion:  "v0.4.9",
		CheckOnly:      true,
		APIBaseURL:     server.URL,
		HTTPClient:     server.Client(),
		GOOS:           "linux",
		GOARCH:         "amd64",
	})
	if err != nil {
		t.Fatal(err)
	}
	if requestedPath != "/repos/opentokenz/mcpx/releases/tags/v0.4.9" || result.TargetVersion != "0.4.9" {
		t.Fatalf("specific version path=%q result=%+v", requestedPath, result)
	}
}

func TestRunRejectsChecksumMismatch(t *testing.T) {
	archive := tarGzipArchive(t, "mcpx", []byte("binary"))
	assetName := "mcpx_0.5.0_linux_amd64.tar.gz"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/opentokenz/mcpx/releases/latest":
			writeRelease(t, w, "v0.5.0", serverURL(r)+"/asset", serverURL(r)+"/checksums", assetName)
		case "/asset":
			_, _ = w.Write(archive)
		case "/checksums":
			_, _ = fmt.Fprintf(w, "%064x  %s\n", 1, assetName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	_, err := Run(context.Background(), Options{
		CurrentVersion: "0.4.1",
		APIBaseURL:     server.URL,
		HTTPClient:     server.Client(),
		GOOS:           "linux",
		GOARCH:         "amd64",
		ExecutablePath: "/unused",
		VerifyBinary:   func(string, string) error { return nil },
		Install:        func(string, string) error { return nil },
	})
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") {
		t.Fatalf("checksum mismatch error=%v", err)
	}
}

func TestExtractZipBinary(t *testing.T) {
	archive := zipArchive(t, "mcpx.exe", []byte("windows binary"))
	directory := t.TempDir()
	path, err := extractBinary("mcpx_0.5.0_windows_amd64.zip", archive, directory, "windows")
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "windows binary" {
		t.Fatalf("zip content=%q", content)
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		left  string
		right string
		want  int
	}{
		{"0.4.1", "0.5.0", -1},
		{"v0.5.0", "0.5.0", 0},
		{"1.0.0", "0.9.9", 1},
		{"0.5.0-rc.1", "0.5.0", -1},
		{"0.5.0-rc.2", "0.5.0-rc.10", -1},
		{"0.5.0-beta", "0.5.0-rc", -1},
	}
	for _, test := range tests {
		t.Run(test.left+"_"+test.right, func(t *testing.T) {
			got, err := compareVersions(test.left, test.right)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("compareVersions(%q,%q)=%d, want %d", test.left, test.right, got, test.want)
			}
		})
	}
}

func TestReplaceExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("replacing a running executable has Windows-specific locking semantics")
	}
	directory := t.TempDir()
	current := filepath.Join(directory, "mcpx")
	candidate := filepath.Join(directory, "candidate")
	if err := os.WriteFile(current, []byte("old"), 0o751); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(current)
	if err != nil {
		t.Fatal(err)
	}
	expectedMode := before.Mode().Perm()
	if err := os.WriteFile(candidate, []byte("new"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := replaceExecutable(current, candidate); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(current)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(current)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "new" || info.Mode().Perm() != expectedMode {
		t.Fatalf("replacement content=%q mode=%o, want mode=%o", content, info.Mode().Perm(), expectedMode)
	}
}

func writeRelease(t *testing.T, w http.ResponseWriter, tag, assetURL, checksumURL, assetName string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(githubRelease{
		TagName: tag,
		Assets: []githubAsset{
			{Name: assetName, BrowserDownloadURL: assetURL},
			{Name: "checksums.txt", BrowserDownloadURL: checksumURL},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func serverURL(r *http.Request) string {
	return "http://" + r.Host
}

func tarGzipArchive(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	gzipWriter := gzip.NewWriter(&buffer)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func zipArchive(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
