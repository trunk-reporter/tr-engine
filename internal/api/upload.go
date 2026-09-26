package api

import (
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/rs/zerolog"
)

// UploadHandler handles HTTP call uploads compatible with rdio-scanner and OpenMHz.
type UploadHandler struct {
	uploader   CallUploader
	instanceID string
	log        zerolog.Logger
}

// NewUploadHandler creates a new upload handler.
func NewUploadHandler(uploader CallUploader, instanceID string, log zerolog.Logger) *UploadHandler {
	return &UploadHandler{
		uploader:   uploader,
		instanceID: instanceID,
		log:        log.With().Str("handler", "upload").Logger(),
	}
}


// Upload handles POST /api/v1/call-upload.
// Accepts multipart form uploads in rdio-scanner or OpenMHz format.
// Auto-detects the format from form field names.
func (h *UploadHandler) Upload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "invalid multipart form: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()

	// Collect all field names for format detection
	var fieldNames []string
	for k := range r.MultipartForm.Value {
		fieldNames = append(fieldNames, k)
	}
	for k := range r.MultipartForm.File {
		fieldNames = append(fieldNames, k)
	}

	format := detectUploadFormat(fieldNames)
	if format == "" {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrBadRequest, "unrecognized upload format: expected rdio-scanner or OpenMHz fields")
		return
	}

	// Extract flat form values. The upload plugins' API key fields were
	// consumed by the upload middleware and never reach the pipeline.
	fields := make(map[string]string, len(r.MultipartForm.Value))
	for k, v := range r.MultipartForm.Value {
		if len(v) > 0 && k != "key" && k != "api_key" {
			fields[k] = v[0]
		}
	}

	// Read audio file
	var audioData []byte
	var audioFilename string
	audioFieldName := "audio" // rdio-scanner
	if format == "openmhz" {
		audioFieldName = "call"
	}

	if file, header, err := r.FormFile(audioFieldName); err == nil {
		defer file.Close()
		data, readErr := io.ReadAll(file)
		if readErr != nil {
			WriteError(w, http.StatusBadRequest, "failed to read audio file")
			return
		}
		audioData = data
		audioFilename = header.Filename

		// Infer audio type from filename if not set in form fields
		if fields["audioType"] == "" && fields["audio_type"] == "" {
			ext := strings.TrimPrefix(filepath.Ext(audioFilename), ".")
			if ext != "" {
				fields["audioType"] = ext
			}
		}
	}

	// Process the upload through the pipeline
	result, err := h.uploader.ProcessUpload(r.Context(), h.instanceID, format, fields, audioData, audioFilename)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate call") {
			WriteErrorWithCode(w, http.StatusConflict, ErrDuplicate, err.Error())
			return
		}
		h.log.Error().Err(err).Str("format", format).Msg("upload processing failed")
		WriteError(w, http.StatusInternalServerError, err.Error())
		return
	}

	WriteJSON(w, http.StatusCreated, result)
}

// detectUploadFormat inspects form field names to determine the upload format.
// Returns "rdio-scanner", "openmhz", or "" if unknown.
func detectUploadFormat(fieldNames []string) string {
	set := make(map[string]bool, len(fieldNames))
	for _, name := range fieldNames {
		set[name] = true
	}

	// rdio-scanner indicators: "audio" file field, "audioName", "systemLabel"
	if set["audio"] || set["audioName"] || set["systemLabel"] {
		return "rdio-scanner"
	}

	// OpenMHz indicators: "call" file field, "talkgroup_num", "start_time"
	if set["call"] || set["talkgroup_num"] {
		return "openmhz"
	}

	return ""
}
