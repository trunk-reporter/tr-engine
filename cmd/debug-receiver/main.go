package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	outputDir      = "/data/tr-engine/debug-reports"
	uploadDir      = "/data/tr-engine/debug-uploads"
	listenAddr     = ":8090"
	webhookURL     = ""
	maxBody        = int64(50 << 20)      // 50 MB for JSON reports
	maxUpload      = int64(50 << 20)      // 50 MB for file uploads
)

type rateLimiter struct {
	mu    sync.Mutex
	times map[string]time.Time
}

func (rl *rateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	last, ok := rl.times[ip]
	if ok && time.Since(last) < time.Minute {
		return false
	}
	rl.times[ip] = time.Now()
	return true
}

func extractIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.Split(fwd, ",")
		return strings.TrimSpace(parts[0])
	}
	if real := r.Header.Get("X-Real-IP"); real != "" {
		return real
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func hashIP(ip string) string {
	h := sha256.Sum256([]byte(ip))
	return fmt.Sprintf("%x", h[:4])
}

func notifyDiscordUpload(ip, filename string, size int64) {
	if webhookURL == "" {
		return
	}
	sizeMB := float64(size) / (1024 * 1024)
	msg := fmt.Sprintf("<@139209424953802752> <@1481021387756797972> **File uploaded**\nFrom: `%s`\nFile: `%s`\nSize: %.2f MB", hashIP(ip), filename, sizeMB)
	payload, _ := json.Marshal(map[string]string{"content": msg})
	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("discord webhook error: %v", err)
		return
	}
	resp.Body.Close()
}

func notifyDiscord(ip, filename string, body []byte) {
	if webhookURL == "" {
		return
	}

	// Extract some summary fields from the report
	var report map[string]interface{}
	json.Unmarshal(body, &report)

	sizeKB := float64(len(body)) / 1024
	msg := fmt.Sprintf("<@139209424953802752> <@1481021387756797972> **Debug report received**\nFrom: `%s`\nFile: `%s`\nSize: %.1f KB", hashIP(ip), filename, sizeKB)

	if txCount, ok := report["transmissions"]; ok {
		if arr, ok := txCount.([]interface{}); ok {
			msg += fmt.Sprintf("\nTransmissions: %d", len(arr))
		}
	}
	if ua, ok := report["user_agent"].(string); ok {
		msg += fmt.Sprintf("\nUA: %s", ua)
	}

	payload, _ := json.Marshal(map[string]string{"content": msg})
	resp, err := http.Post(webhookURL, "application/json", bytes.NewReader(payload))
	if err != nil {
		log.Printf("discord webhook error: %v", err)
		return
	}
	resp.Body.Close()
}

func writeGzipped(path string, data []byte) error {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(data); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0644)
}

func main() {
	if dir := os.Getenv("DEBUG_REPORT_DIR"); dir != "" {
		outputDir = dir
	}
	if addr := os.Getenv("LISTEN_ADDR"); addr != "" {
		listenAddr = addr
	}
	if wh := os.Getenv("DISCORD_WEBHOOK_URL"); wh != "" {
		webhookURL = wh
	}

	if dir := os.Getenv("UPLOAD_DIR"); dir != "" {
		uploadDir = dir
	}

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("failed to create output dir %s: %v", outputDir, err)
	}
	if err := os.MkdirAll(uploadDir, 0755); err != nil {
		log.Fatalf("failed to create upload dir %s: %v", uploadDir, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		// CORS
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Encoding")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ip := extractIP(r)
		ts := time.Now().UTC().Format("2006-01-02T15-04-05")
		baseName := fmt.Sprintf("%s_%s", ts, hashIP(ip))

		ct := r.Header.Get("Content-Type")
		isMultipart := strings.HasPrefix(ct, "multipart/")

		if isMultipart {
			// Multipart: report.json + audio_N.pcm files
			r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
			if err := r.ParseMultipartForm(maxUpload); err != nil {
				http.Error(w, "multipart parse error", http.StatusBadRequest)
				return
			}

			var body []byte
			var audioFiles []string

			for fieldName, headers := range r.MultipartForm.File {
				for _, fh := range headers {
					src, err := fh.Open()
					if err != nil {
						continue
					}
					data, err := io.ReadAll(src)
					src.Close()
					if err != nil {
						continue
					}

					if fieldName == "report" {
						body = data
					} else {
						// Audio file — save alongside report
						audioName := fmt.Sprintf("%s_%s", baseName, filepath.Base(fh.Filename))
						audioPath := filepath.Join(outputDir, audioName)
						if err := os.WriteFile(audioPath, data, 0644); err != nil {
							log.Printf("failed to write audio %s: %v", audioName, err)
							continue
						}
						audioFiles = append(audioFiles, audioName)
						log.Printf("saved audio from %s → %s (%d bytes)", ip, audioName, len(data))
					}
				}
			}

			if body == nil {
				http.Error(w, "missing report field", http.StatusBadRequest)
				return
			}
			if !json.Valid(body) {
				http.Error(w, "invalid JSON in report", http.StatusBadRequest)
				return
			}

			filename := baseName + ".json.gz"
			path := filepath.Join(outputDir, filename)
			if err := writeGzipped(path, body); err != nil {
				log.Printf("failed to write report: %v", err)
				http.Error(w, "write error", http.StatusInternalServerError)
				return
			}

			log.Printf("saved report from %s → %s (%d bytes raw, %d audio files)", ip, filename, len(body), len(audioFiles))
			go notifyDiscord(ip, filename, body)

		} else {
			// Legacy JSON body (with optional gzip)
			var reader io.Reader = io.LimitReader(r.Body, maxBody)
			if r.Header.Get("Content-Encoding") == "gzip" {
				gz, err := gzip.NewReader(reader)
				if err != nil {
					http.Error(w, "invalid gzip", http.StatusBadRequest)
					return
				}
				defer gz.Close()
				reader = gz
			}

			body, err := io.ReadAll(reader)
			if err != nil {
				http.Error(w, "read error", http.StatusBadRequest)
				return
			}
			if !json.Valid(body) {
				http.Error(w, "invalid JSON", http.StatusBadRequest)
				return
			}

			filename := baseName + ".json.gz"
			path := filepath.Join(outputDir, filename)
			if err := writeGzipped(path, body); err != nil {
				log.Printf("failed to write report: %v", err)
				http.Error(w, "write error", http.StatusInternalServerError)
				return
			}

			log.Printf("saved report from %s → %s (%d bytes raw)", ip, filename, len(body))
			go notifyDiscord(ip, filename, body)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	mux.HandleFunc("/upload", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Encoding")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
		if err := r.ParseMultipartForm(maxUpload); err != nil {
			http.Error(w, "file too large (50 MB max)", http.StatusRequestEntityTooLarge)
			return
		}

		ip := extractIP(r)
		ts := time.Now().UTC().Format("2006-01-02T15-04-05")

		var saved []string
		for _, headers := range r.MultipartForm.File {
			for _, fh := range headers {
				src, err := fh.Open()
				if err != nil {
					continue
				}

				// Sanitize filename: keep only the base name
				origName := filepath.Base(fh.Filename)
				filename := fmt.Sprintf("%s_%s_%s", ts, hashIP(ip), origName)
				path := filepath.Join(uploadDir, filename)

				dst, err := os.Create(path)
				if err != nil {
					src.Close()
					log.Printf("failed to create file: %v", err)
					continue
				}

				n, err := io.Copy(dst, src)
				src.Close()
				dst.Close()
				if err != nil {
					log.Printf("failed to write file: %v", err)
					continue
				}

				log.Printf("saved upload from %s → %s (%d bytes)", ip, filename, n)
				go notifyDiscordUpload(ip, filename, n)
				saved = append(saved, filename)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"ok":    len(saved) > 0,
			"files": saved,
		})
	})

	// Simple upload page
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html>
<html><head><title>Debug Upload</title>
<style>
body { font-family: system-ui; max-width: 600px; margin: 60px auto; padding: 0 20px; background: #1a1a2e; color: #e0e0e0; }
h1 { color: #fff; }
form { margin: 20px 0; padding: 20px; border: 1px dashed #444; border-radius: 8px; }
input[type=file] { margin: 10px 0; }
button { background: #0f3460; color: #fff; border: none; padding: 10px 24px; border-radius: 4px; cursor: pointer; font-size: 16px; }
button:hover { background: #16498a; }
#status { margin-top: 15px; }
.ok { color: #4ecca3; } .err { color: #e74c3c; }
</style></head><body>
<h1>tr-engine Debug Upload</h1>
<p>Upload PCM captures, logs, or other debug files (50 MB max per file).</p>
<form id="f" enctype="multipart/form-data">
  <input type="file" name="files" multiple><br>
  <button type="submit">Upload</button>
  <div id="status"></div>
</form>
<script>
document.getElementById('f').onsubmit = function(e) {
  e.preventDefault();
  var fd = new FormData(this);
  var st = document.getElementById('status');
  st.textContent = 'Uploading...';
  st.className = '';
  fetch('upload', { method: 'POST', body: fd })
    .then(function(r) { return r.json(); })
    .then(function(d) {
      if (d.ok) { st.textContent = 'Uploaded: ' + d.files.join(', '); st.className = 'ok'; }
      else { st.textContent = 'No files uploaded'; st.className = 'err'; }
    })
    .catch(function(e) { st.textContent = 'Error: ' + e.message; st.className = 'err'; });
};
</script>
</body></html>`))
	})

	log.Printf("debug-receiver listening on %s, writing to %s", listenAddr, outputDir)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}
