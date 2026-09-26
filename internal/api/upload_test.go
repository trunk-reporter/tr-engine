package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// mockCallUploader implements CallUploader for testing.
type mockCallUploader struct {
	lastInstanceID string
	lastFormat     string
	lastFields     map[string]string
	lastAudioLen   int
	lastFilename   string
	result         *UploadCallResult
	err            error
}

func (m *mockCallUploader) ProcessUpload(ctx context.Context, instanceID string, format string, fields map[string]string, audioData []byte, audioFilename string) (*UploadCallResult, error) {
	m.lastInstanceID = instanceID
	m.lastFormat = format
	m.lastFields = fields
	m.lastAudioLen = len(audioData)
	m.lastFilename = audioFilename
	if m.err != nil {
		return nil, m.err
	}
	if m.result != nil {
		return m.result, nil
	}
	return &UploadCallResult{
		CallID:    123,
		SystemID:  1,
		Tgid:      9044,
		StartTime: time.Unix(1708881234, 0),
	}, nil
}

func newTestUploadHandler(mock *mockCallUploader) *UploadHandler {
	return NewUploadHandler(mock, "test-instance", zerolog.Nop())
}

func buildMultipartForm(t *testing.T, fields map[string]string, fileField string, fileData []byte, fileName string) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	for k, v := range fields {
		writer.WriteField(k, v)
	}
	if fileData != nil && fileField != "" {
		part, err := writer.CreateFormFile(fileField, fileName)
		if err != nil {
			t.Fatal(err)
		}
		part.Write(fileData)
	}
	writer.Close()
	return body, writer.FormDataContentType()
}

func TestUpload_RdioScanner_Success(t *testing.T) {
	mock := &mockCallUploader{}
	handler := newTestUploadHandler(mock)

	body, ct := buildMultipartForm(t, map[string]string{
		"talkgroup":   "9044",
		"dateTime":    "1708881234",
		"systemLabel": "butco",
		"frequency":   "859262500",
	}, "audio", []byte("fake-audio-data"), "test.m4a")

	req := httptest.NewRequest("POST", "/api/v1/call-upload", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	handler.Upload(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}

	// Verify mock was called with correct format
	if mock.lastFormat != "rdio-scanner" {
		t.Errorf("format = %q, want %q", mock.lastFormat, "rdio-scanner")
	}
	if mock.lastInstanceID != "test-instance" {
		t.Errorf("instanceID = %q, want %q", mock.lastInstanceID, "test-instance")
	}
	if mock.lastFields["talkgroup"] != "9044" {
		t.Errorf("fields[talkgroup] = %q, want %q", mock.lastFields["talkgroup"], "9044")
	}
	if mock.lastAudioLen != len("fake-audio-data") {
		t.Errorf("audioLen = %d, want %d", mock.lastAudioLen, len("fake-audio-data"))
	}
	if mock.lastFilename != "test.m4a" {
		t.Errorf("filename = %q, want %q", mock.lastFilename, "test.m4a")
	}

	// Verify response body
	var result UploadCallResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result.CallID != 123 {
		t.Errorf("result.CallID = %d, want 123", result.CallID)
	}
}

func TestUpload_OpenMHz_Success(t *testing.T) {
	mock := &mockCallUploader{}
	handler := newTestUploadHandler(mock)

	body, ct := buildMultipartForm(t, map[string]string{
		"talkgroup_num": "9044",
		"start_time":    "1708881234",
		"stop_time":     "1708881276",
		"freq":          "859262500",
	}, "call", []byte("fake-audio-data"), "test.m4a")

	req := httptest.NewRequest("POST", "/api/v1/call-upload", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	handler.Upload(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if mock.lastFormat != "openmhz" {
		t.Errorf("format = %q, want %q", mock.lastFormat, "openmhz")
	}
}

func TestUpload_UnknownFormat(t *testing.T) {
	mock := &mockCallUploader{}
	handler := newTestUploadHandler(mock)

	body, ct := buildMultipartForm(t, map[string]string{
		"foo": "bar",
		"baz": "qux",
	}, "", nil, "")

	req := httptest.NewRequest("POST", "/api/v1/call-upload", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	handler.Upload(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}

	var resp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["error"] == "" {
		t.Error("expected error message in response")
	}
}

func TestUpload_DuplicateCall(t *testing.T) {
	mock := &mockCallUploader{
		err: fmt.Errorf("duplicate call: existing call_id=456"),
	}
	handler := newTestUploadHandler(mock)

	body, ct := buildMultipartForm(t, map[string]string{
		"talkgroup":   "9044",
		"dateTime":    "1708881234",
		"systemLabel": "butco",
	}, "audio", []byte("fake-audio"), "test.m4a")

	req := httptest.NewRequest("POST", "/api/v1/call-upload", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	handler.Upload(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusConflict)
	}
}

func TestUpload_InternalError(t *testing.T) {
	mock := &mockCallUploader{
		err: fmt.Errorf("database connection failed"),
	}
	handler := newTestUploadHandler(mock)

	body, ct := buildMultipartForm(t, map[string]string{
		"talkgroup":   "9044",
		"dateTime":    "1708881234",
		"systemLabel": "butco",
	}, "audio", []byte("fake-audio"), "test.m4a")

	req := httptest.NewRequest("POST", "/api/v1/call-upload", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	handler.Upload(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestUpload_NoAudioFile(t *testing.T) {
	mock := &mockCallUploader{}
	handler := newTestUploadHandler(mock)

	// rdio-scanner format with no audio file attachment
	body, ct := buildMultipartForm(t, map[string]string{
		"talkgroup":   "9044",
		"dateTime":    "1708881234",
		"systemLabel": "butco",
	}, "", nil, "")

	req := httptest.NewRequest("POST", "/api/v1/call-upload", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	handler.Upload(rec, req)

	// Should still succeed — audio is optional (metadata-only upload)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if mock.lastAudioLen != 0 {
		t.Errorf("audioLen = %d, want 0", mock.lastAudioLen)
	}
}

func TestUpload_AudioTypeInference(t *testing.T) {
	mock := &mockCallUploader{}
	handler := newTestUploadHandler(mock)

	body, ct := buildMultipartForm(t, map[string]string{
		"talkgroup":   "9044",
		"dateTime":    "1708881234",
		"systemLabel": "butco",
		// No audioType field — should be inferred from filename
	}, "audio", []byte("fake"), "recording.wav")

	req := httptest.NewRequest("POST", "/api/v1/call-upload", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	handler.Upload(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusCreated)
	}
	if mock.lastFields["audioType"] != "wav" {
		t.Errorf("audioType = %q, want %q", mock.lastFields["audioType"], "wav")
	}
}

func TestUpload_NotMultipart(t *testing.T) {
	mock := &mockCallUploader{}
	handler := newTestUploadHandler(mock)

	req := httptest.NewRequest("POST", "/api/v1/call-upload", bytes.NewBufferString("not multipart"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.Upload(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestDetectUploadFormat(t *testing.T) {
	tests := []struct {
		name   string
		fields []string
		want   string
	}{
		{"rdio-scanner by audio field", []string{"audio", "talkgroup", "dateTime"}, "rdio-scanner"},
		{"rdio-scanner by audioName", []string{"audioName", "talkgroup"}, "rdio-scanner"},
		{"rdio-scanner by systemLabel", []string{"systemLabel", "talkgroup"}, "rdio-scanner"},
		{"openmhz by call field", []string{"call", "talkgroup_num", "freq"}, "openmhz"},
		{"openmhz by talkgroup_num", []string{"talkgroup_num", "start_time"}, "openmhz"},
		{"unknown format", []string{"foo", "bar"}, ""},
		{"empty fields", []string{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectUploadFormat(tt.fields)
			if got != tt.want {
				t.Errorf("detectUploadFormat = %q, want %q", got, tt.want)
			}
		})
	}
}

// An error caused by the upload's own fields is the client's fault: 400, not
// 500.
func TestUpload_InvalidFieldsIsBadRequest(t *testing.T) {
	mock := &mockCallUploader{
		err: fmt.Errorf("%w: parse rdio-scanner fields: missing required field: talkgroup", ErrInvalidUpload),
	}
	handler := newTestUploadHandler(mock)

	body, ct := buildMultipartForm(t, map[string]string{
		"systemLabel": "butco",
	}, "audio", []byte("fake-audio"), "test.m4a")

	req := httptest.NewRequest("POST", "/api/v1/call-upload", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	handler.Upload(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Code != ErrInvalidBody {
		t.Errorf("code = %q, want %q", resp.Code, ErrInvalidBody)
	}
}

// A body over the route's limit is 413, whether the key came from the header
// (the handler parses the form) or from the form (the upload middleware
// does).
func TestUpload_BodyTooLargeIs413(t *testing.T) {
	mock := &mockCallUploader{}
	handler := MaxBodySize(1024)(http.HandlerFunc(newTestUploadHandler(mock).Upload))

	body, ct := buildMultipartForm(t, map[string]string{
		"talkgroup":   "9044",
		"dateTime":    "1708881234",
		"systemLabel": "butco",
	}, "audio", bytes.Repeat([]byte("x"), 4096), "test.m4a")

	req := httptest.NewRequest("POST", "/api/v1/call-upload", body)
	req.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
	if mock.lastFormat != "" {
		t.Error("the uploader was called for a rejected body")
	}
}
