package transcribe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IMBEClient calls an OpenAI-compatible /v1/audio/transcriptions endpoint
// with IMBE .dvcf files instead of standard audio. Implements the Provider interface.
type IMBEClient struct {
	url     string
	model   string
	timeout time.Duration
	client  *http.Client
}

// imbeResponse is the parsed response from the IMBE ASR API.
type imbeResponse struct {
	Text     string  `json:"text"`
	Duration float64 `json:"duration"`
}

// NewIMBEClient creates a new IMBE ASR HTTP client.
func NewIMBEClient(url, model string, timeout time.Duration) *IMBEClient {
	return &IMBEClient{
		url:     url,
		model:   model,
		timeout: timeout,
		client:  &http.Client{Timeout: timeout},
	}
}

// Name returns the provider name.
func (c *IMBEClient) Name() string { return "imbe" }

// Model returns the configured model identifier.
func (c *IMBEClient) Model() string { return c.model }

// Transcribe derives the .dvcf file path from the audio path, sends it to the
// IMBE ASR endpoint, and returns the transcription result.
func (c *IMBEClient) Transcribe(ctx context.Context, audioPath string, opts TranscribeOpts) (*Response, error) {
	// Derive .dvcf path: replace the audio file extension with .dvcf
	ext := filepath.Ext(audioPath)
	dvcfPath := strings.TrimSuffix(audioPath, ext) + ".dvcf"

	f, err := os.Open(dvcfPath)
	if err != nil {
		return nil, fmt.Errorf("open dvcf file: %w (expected at %s)", err, dvcfPath)
	}
	defer f.Close()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	// Tap file field
	part, err := w.CreateFormFile("file", filepath.Base(dvcfPath))
	if err != nil {
		return nil, fmt.Errorf("create form file: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return nil, fmt.Errorf("copy dvcf data: %w", err)
	}

	// Model
	if c.model != "" {
		w.WriteField("model", c.model)
	}

	w.Close()

	endpoint := strings.TrimRight(c.url, "/") + "/v1/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("imbe request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("imbe API error (status %d): %s", resp.StatusCode, string(body))
	}

	var result imbeResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &Response{
		Text:     result.Text,
		Duration: result.Duration,
	}, nil
}
