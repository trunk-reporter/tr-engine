package main

import (
	"bytes"
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
	outputDir  = "/data/tr-engine/debug-reports"
	listenAddr = ":8090"
	webhookURL = ""
	maxBody    = int64(1 << 20) // 1 MB
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

func notifyDiscord(ip, filename string, body []byte) {
	if webhookURL == "" {
		return
	}

	// Extract some summary fields from the report
	var report map[string]interface{}
	json.Unmarshal(body, &report)

	sizeKB := float64(len(body)) / 1024
	msg := fmt.Sprintf("<@139209424953802752> **Debug report received**\nFrom: `%s`\nFile: `%s`\nSize: %.1f KB", ip, filename, sizeKB)

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

	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("failed to create output dir %s: %v", outputDir, err)
	}

	rl := &rateLimiter{times: make(map[string]time.Time)}

	mux := http.NewServeMux()
	mux.HandleFunc("/report", func(w http.ResponseWriter, r *http.Request) {
		// CORS
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		ip := extractIP(r)
		if !rl.Allow(ip) {
			http.Error(w, "rate limited — try again in 1 minute", http.StatusTooManyRequests)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, "read error", http.StatusBadRequest)
			return
		}

		if !json.Valid(body) {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}

		ts := time.Now().UTC().Format("2006-01-02T15-04-05")
		filename := fmt.Sprintf("%s_%s.json", ts, strings.ReplaceAll(ip, ":", "-"))
		path := filepath.Join(outputDir, filename)

		if err := os.WriteFile(path, body, 0644); err != nil {
			log.Printf("failed to write report: %v", err)
			http.Error(w, "write error", http.StatusInternalServerError)
			return
		}

		log.Printf("saved report from %s → %s (%d bytes)", ip, filename, len(body))
		go notifyDiscord(ip, filename, body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	log.Printf("debug-receiver listening on %s, writing to %s", listenAddr, outputDir)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		log.Fatal(err)
	}
}
