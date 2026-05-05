package storage

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/azukaar/plurality/src/docsupport"
	"github.com/azukaar/plurality/src/utils"
)

// otherAllowedExts is the set of non-document extensions /upload accepts.
// Document formats (pdf/docx/xlsx/pptx) are sourced from docsupport.IsDocumentType.
var otherAllowedExts = map[string]bool{
	"zip": true, "tar": true, "gz": true, "7z": true, "rar": true,
	"mp3": true, "wav": true, "m4a": true, "ogg": true, "flac": true,
	"mp4": true, "mov": true, "webm": true, "mkv": true,
	"bin": true,
}

type uploadResponse struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Ext      string `json:"ext"`
	Type     string `json:"type"`
	Size     int    `json:"size"`
}

// HandleUpload accepts a multipart/form-data POST with a "file" field,
// persists the blob under the authenticated user's storage prefix, and
// returns the internal /attachments/... URL plus metadata.
func HandleUpload(w http.ResponseWriter, r *http.Request) {
	userID, ok := r.Context().Value("userID").(string)
	if !ok || userID == "" {
		http.Error(w, "User not authenticated", http.StatusUnauthorized)
		return
	}

	// Cap total request body to MaxBlobSize + slack for multipart overhead.
	r.Body = http.MaxBytesReader(w, r.Body, MaxBlobSize+1<<20)

	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "Upload too large or malformed: "+err.Error(), http.StatusRequestEntityTooLarge)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Missing 'file' form field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		utils.SendHTTPError(w, "reading upload: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if len(data) > MaxBlobSize {
		http.Error(w, fmt.Sprintf("File exceeds maximum size of %d bytes", MaxBlobSize), http.StatusRequestEntityTooLarge)
		return
	}

	filename := header.Filename
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	if ext == "" {
		ext = "bin"
	}

	if !isAllowedUploadExt(ext) {
		http.Error(w, "Unsupported file extension: "+ext, http.StatusUnsupportedMediaType)
		return
	}

	urlPath, err := SaveBlob(userID, data, ext)
	if err != nil {
		utils.SendHTTPError(w, "saving upload: "+err.Error(), http.StatusInternalServerError)
		return
	}

	partType := "file"
	if docsupport.IsDocumentType(ext) {
		partType = ext
	}

	resp := uploadResponse{
		URL:      urlPath,
		Filename: filename,
		Ext:      ext,
		Type:     partType,
		Size:     len(data),
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func isAllowedUploadExt(ext string) bool {
	if docsupport.IsDocumentType(ext) {
		return true
	}
	return otherAllowedExts[ext]
}
