package vlupdate

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Download fetches the archive for art at expectedVersion from the configured
// binary repo, verifies its SHA256 against the published checksums.txt, extracts
// the binary, and atomically installs it under destDir. Returns the binary path.
func Download(ctx context.Context, repo string, expectedVersion, destDir string, art artifact) (string, error) {
	if repo == "" {
		return "", errors.New("binary repo is empty")
	}

	platform := fmt.Sprintf("%s_%s", runtime.GOOS, runtime.GOARCH)
	ext := "tar.gz"
	if runtime.GOOS == "windows" {
		ext = "zip"
	}
	archiveName := fmt.Sprintf("%s_%s.%s", art.Name, platform, ext)

	tag := expectedVersion
	if !strings.HasPrefix(tag, "v") {
		tag = "v" + tag
	}
	base := fmt.Sprintf("%s/releases/download/%s", repo, tag)
	archiveURL := base + "/" + archiveName
	checksumURL := base + "/checksums.txt"

	expectedSum, err := fetchExpectedSum(ctx, checksumURL, archiveName)
	if err != nil {
		return "", fmt.Errorf("fetching checksum from %s: %w", checksumURL, err)
	}

	if err := os.MkdirAll(destDir, 0755); err != nil {
		return "", fmt.Errorf("creating dest dir: %w", err)
	}

	fmt.Printf("Downloading %s %s for %s...\n", art.Name, expectedVersion, platform)
	archivePath := filepath.Join(destDir, archiveName+".tmp")
	if err := downloadAndVerify(ctx, archiveURL, archivePath, expectedSum); err != nil {
		os.Remove(archivePath)
		return "", err
	}
	defer os.Remove(archivePath)

	finalPath := filepath.Join(destDir, art.binaryName)
	tmpBin := finalPath + ".tmp"
	if err := extractBinary(archivePath, tmpBin, ext, art.binaryName); err != nil {
		os.Remove(tmpBin)
		return "", fmt.Errorf("extracting archive: %w", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmpBin, 0755); err != nil {
			return "", fmt.Errorf("chmod: %w", err)
		}
	}
	if err := os.Rename(tmpBin, finalPath); err != nil {
		return "", fmt.Errorf("installing binary: %w", err)
	}
	fmt.Printf("Downloaded %s to %s\n", art.Name, finalPath)
	return finalPath, nil
}

func fetchExpectedSum(ctx context.Context, url, archiveName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("checksums.txt: HTTP %d", resp.StatusCode)
	}
	scan := bufio.NewScanner(resp.Body)
	for scan.Scan() {
		// Format: "<sha256>  <filename>"
		fields := strings.Fields(scan.Text())
		if len(fields) == 2 && fields[1] == archiveName {
			return fields[0], nil
		}
	}
	if err := scan.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no checksum entry for %s", archiveName)
}

func downloadAndVerify(ctx context.Context, url, dst, expectedSum string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("archive: HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		return fmt.Errorf("downloading: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != expectedSum {
		return fmt.Errorf("sha256 mismatch: got %s want %s", got, expectedSum)
	}
	return nil
}

// extractBinary pulls the single named binary out of the archive and writes it to dst.
func extractBinary(archivePath, dst, ext, binName string) error {
	if ext == "zip" {
		return extractFromZip(archivePath, dst, binName)
	}
	return extractFromTarGz(archivePath, dst, binName)
}

func extractFromTarGz(archivePath, dst, binName string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(hdr.Name) != binName {
			continue
		}
		return writeFromReader(dst, tr)
	}
	return fmt.Errorf("%s not found in archive", binName)
}

func extractFromZip(archivePath, dst, binName string) error {
	zr, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer zr.Close()

	for _, zf := range zr.File {
		if filepath.Base(zf.Name) != binName {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		err = writeFromReader(dst, rc)
		rc.Close()
		return err
	}
	return fmt.Errorf("%s not found in archive", binName)
}

func writeFromReader(dst string, r io.Reader) error {
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, r); err != nil {
		return err
	}
	return out.Close()
}
