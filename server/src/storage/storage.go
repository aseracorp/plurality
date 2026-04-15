package storage

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/azukaar/plurality/src/utils"
)

var storagePath string

// Init reads the USER_DATA_STORAGE env var (default "./users-data" relative
// to the binary) and creates the directory.
func Init() {
	storagePath = os.Getenv("USER_DATA_STORAGE")
	if storagePath == "" {
		exec, _ := os.Executable()
		storagePath = filepath.Join(filepath.Dir(exec), "users-data")
	}
	if err := os.MkdirAll(storagePath, 0755); err != nil {
		utils.Error("[Storage] Failed to create storage directory", err)
	}
	utils.Log("[Storage] Initialized at %s", storagePath)
}

// StoragePath returns the configured base path.
func StoragePath() string {
	return storagePath
}

// IsInternalURL returns true if the URL is an internal attachment path.
func IsInternalURL(url string) bool {
	return strings.HasPrefix(url, "/attachments/")
}

// SaveBlob writes raw bytes to {base}/{userID}/{YYYY.MM}/{uuid}.{ext}
// and returns the URL path /attachments/{userID}/{YYYY.MM}/{uuid}.{ext}.
func SaveBlob(userID string, data []byte, ext string) (string, error) {
	if err := validatePathComponent(userID); err != nil {
		return "", fmt.Errorf("invalid userID: %w", err)
	}

	month := time.Now().Format("2006.01")
	dir := filepath.Join(storagePath, userID, "attachments", month)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("creating directory: %w", err)
	}

	id := generateUUID()
	filename := id + "." + ext
	filePath := filepath.Join(dir, filename)

	if err := os.WriteFile(filePath, data, 0644); err != nil {
		return "", fmt.Errorf("writing file: %w", err)
	}

	urlPath := "/attachments/" + userID + "/" + month + "/" + filename
	utils.Debug("[Storage] Saved blob %s (%d bytes)", urlPath, len(data))
	return urlPath, nil
}

// ReadBlob reads a file from the storage path given a URL path like
// /attachments/{userID}/{month}/{filename}.
// Returns the raw bytes and detected MIME type.
func ReadBlob(urlPath string) ([]byte, string, error) {
	rel := strings.TrimPrefix(urlPath, "/attachments/")
	parts := strings.SplitN(rel, "/", 3)
	if len(parts) != 3 {
		return nil, "", fmt.Errorf("invalid attachment path: %s", urlPath)
	}
	for _, p := range parts {
		if err := validatePathComponent(p); err != nil {
			return nil, "", fmt.Errorf("invalid path component: %w", err)
		}
	}

	filePath := filepath.Join(storagePath, parts[0], "attachments", parts[1], parts[2])
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, "", fmt.Errorf("reading file: %w", err)
	}

	mimeType := mime.TypeByExtension(filepath.Ext(parts[2]))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	return data, mimeType, nil
}

// DeleteUserFiles removes {base}/{userID}/ recursively.
func DeleteUserFiles(userID string) error {
	if err := validatePathComponent(userID); err != nil {
		return err
	}
	dir := filepath.Join(storagePath, userID)
	utils.Log("[Storage] Deleting all files for user: %s", dir)
	return os.RemoveAll(dir)
}

// FileSizeFromURL returns the file size for an internal attachment URL path.
// Matches the utils.FileSizeFunc signature for use with BuildAttachmentIndex.
func FileSizeFromURL(urlPath string) int64 {
	rel := strings.TrimPrefix(urlPath, "/attachments/")
	parts := strings.SplitN(rel, "/", 3)
	if len(parts) != 3 {
		return 0
	}
	filePath := filepath.Join(storagePath, parts[0], "attachments", parts[1], parts[2])
	info, err := os.Stat(filePath)
	if err != nil {
		return 0
	}
	return info.Size()
}

func generateUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func validatePathComponent(s string) error {
	if s == "" {
		return fmt.Errorf("empty path component")
	}
	if strings.Contains(s, "..") || strings.Contains(s, "/") || strings.Contains(s, "\\") {
		return fmt.Errorf("invalid path component: %q", s)
	}
	return nil
}
