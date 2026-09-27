package api

import (
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/snarg/tr-engine/internal/audio"
	"github.com/snarg/tr-engine/internal/database"
	"github.com/snarg/tr-engine/internal/storage"
)

type CallsHandler struct {
	db         *database.DB
	audioDir   string
	trAudioDir string
	store      storage.AudioStore
	live       LiveDataSource
}

func NewCallsHandler(db *database.DB, audioDir, trAudioDir string, store storage.AudioStore, live LiveDataSource) *CallsHandler {
	return &CallsHandler{db: db, audioDir: audioDir, trAudioDir: trAudioDir, store: store, live: live}
}

// enrichAudioURLs sets audio_url on calls that have a call_filename but no
// audio_file_path, when TR_AUDIO_DIR mode is active.
func (h *CallsHandler) enrichAudioURLs(calls []database.CallAPI) {
	if h.trAudioDir == "" {
		return
	}
	for i := range calls {
		if calls[i].AudioURL == nil && calls[i].CallFilename != "" {
			url := fmt.Sprintf("/api/v1/calls/%d/audio", calls[i].CallID)
			calls[i].AudioURL = &url
		}
	}
}

var callSortFields = map[string]string{
	"start_time": "c.start_time",
	"stop_time":  "c.stop_time",
	"duration":   "c.duration",
	"tgid":       "c.tgid",
	"freq":       "c.freq",
}

// ListCalls returns calls with comprehensive filters.
func (h *CallsHandler) ListCalls(w http.ResponseWriter, r *http.Request) {
	p, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}
	sort := ParseSort(r, "-start_time", callSortFields)

	filter := database.CallFilter{
		Limit:  p.Limit,
		Offset: p.Offset,
		Sort:   sort.SQLOrderBy(callSortFields),
	}

	filter.Sysids = QueryStringListAliased(r, "sysid", "sysids")
	filter.SystemIDs = QueryIntListAliased(r, "system_id", "systems")
	filter.SiteIDs = QueryIntListAliased(r, "site_id", "sites")
	filter.Tgids = QueryIntListAliased(r, "tgid", "tgids")
	filter.UnitIDs = QueryIntListAliased(r, "unit_id", "units", "unit_ids")
	if v, ok := QueryBool(r, "emergency"); ok {
		filter.Emergency = &v
	}
	if v, ok := QueryBool(r, "encrypted"); ok {
		filter.Encrypted = &v
	}
	if v, ok := QueryBool(r, "deduplicate"); ok {
		filter.Deduplicate = v
	}
	if t, ok := QueryTime(r, "start_time"); ok {
		filter.StartTime = &t
	}
	if t, ok := QueryTime(r, "end_time"); ok {
		filter.EndTime = &t
	}
	if msg := ValidateTimeRange(filter.StartTime, filter.EndTime); msg != "" {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidTimeRange, msg)
		return
	}

	calls, total, err := h.db.ListCalls(r.Context(), PrincipalFrom(r), filter)
	if err != nil {
		WriteError(w, http.StatusInternalServerError, "failed to list calls")
		return
	}
	h.enrichAudioURLs(calls)
	WriteJSON(w, http.StatusOK, map[string]any{
		"calls":  calls,
		"total":  total,
		"limit":  p.Limit,
		"offset": p.Offset,
	})
}

// ListActiveCalls returns currently active calls from the in-memory MQTT
// tracker. The caller's restriction is applied to every call, whether or not
// the request has filters of its own.
func (h *CallsHandler) ListActiveCalls(w http.ResponseWriter, r *http.Request) {
	if h.live == nil {
		WriteJSON(w, http.StatusOK, map[string]any{
			"calls": []any{},
			"total": 0,
		})
		return
	}

	// Apply the restriction and the filters
	p := PrincipalFrom(r)
	sysid, hasSysid := QueryString(r, "sysid")
	tgid, hasTgid := QueryInt(r, "tgid")
	emergency, hasEmergency := QueryBool(r, "emergency")
	encrypted, hasEncrypted := QueryBool(r, "encrypted")

	all := h.live.ActiveCalls()
	calls := make([]ActiveCallData, 0, len(all))
	for _, c := range all {
		if !p.AllowsTG(c.SystemID, c.Tgid) {
			continue
		}
		if hasSysid && c.Sysid != sysid {
			continue
		}
		if hasTgid && c.Tgid != tgid {
			continue
		}
		if hasEmergency && c.Emergency != emergency {
			continue
		}
		if hasEncrypted && c.Encrypted != encrypted {
			continue
		}
		calls = append(calls, c)
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"calls": calls,
		"total": len(calls),
	})
}

// GetCall returns a single call by ID. A call outside the caller's
// restriction is 404, like a missing one.
func (h *CallsHandler) GetCall(w http.ResponseWriter, r *http.Request) {
	id, err := PathInt64(r, "id")
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid call ID")
		return
	}
	if !requireCallAccess(w, r, h.db, id, "call not found") {
		return
	}

	call, err := h.db.GetCallByID(r.Context(), PrincipalFrom(r), id)
	if err != nil {
		writeLookupError(w, err, "call not found", "failed to get call")
		return
	}
	if h.trAudioDir != "" && call.AudioURL == nil && call.CallFilename != "" {
		url := fmt.Sprintf("/api/v1/calls/%d/audio", call.CallID)
		call.AudioURL = &url
	}
	WriteJSON(w, http.StatusOK, call)
}

// GetCallAudio streams the audio file for a call. A call outside the
// caller's restriction is 404, like a missing one.
func (h *CallsHandler) GetCallAudio(w http.ResponseWriter, r *http.Request) {
	id, err := PathInt64(r, "id")
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid call ID")
		return
	}
	if !requireCallAccess(w, r, h.db, id, "audio not found") {
		return
	}

	audioPath, callFilename, err := h.db.GetCallAudioPath(r.Context(), PrincipalFrom(r), id)
	if err != nil {
		writeLookupError(w, err, "audio not found", "failed to look up audio")
		return
	}

	// 1. Try storage layer (local cache for tiered, local disk for local-only)
	if audioPath != "" && h.store != nil {
		if localFile := h.store.LocalPath(audioPath); localFile != "" {
			h.serveLocalFile(w, r, localFile, id)
			return
		}
	}

	// 2. Try storage Open (downloads from S3 and caches locally on tiered stores)
	if audioPath != "" && h.store != nil {
		if rc, openErr := h.store.Open(r.Context(), audioPath); openErr == nil {
			defer rc.Close()
			ext := strings.ToLower(filepath.Ext(audioPath))
			contentTypes := map[string]string{
				".m4a": "audio/mp4",
				".mp3": "audio/mpeg",
				".wav": "audio/wav",
				".ogg": "audio/ogg",
			}
			if ct, ok := contentTypes[ext]; ok {
				w.Header().Set("Content-Type", ct)
			} else {
				w.Header().Set("Content-Type", "application/octet-stream")
			}
			w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%d%s"`, id, ext))
			io.Copy(w, rc)
			return
		}
	}

	// 3. Fall back to TR_AUDIO_DIR resolution (file watch mode)
	fullPath := h.resolveAudioFile(audioPath, callFilename)
	if fullPath != "" {
		h.serveLocalFile(w, r, fullPath, id)
		return
	}

	WriteError(w, http.StatusNotFound, "audio file not found on disk")
}

func (h *CallsHandler) serveLocalFile(w http.ResponseWriter, r *http.Request, path string, callID int64) {
	ext := strings.ToLower(filepath.Ext(path))
	contentTypes := map[string]string{
		".m4a": "audio/mp4",
		".mp3": "audio/mpeg",
		".wav": "audio/wav",
		".ogg": "audio/ogg",
	}
	if ct, ok := contentTypes[ext]; ok {
		w.Header().Set("Content-Type", ct)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%d%s"`, callID, ext))
	http.ServeFile(w, r, path)
}

// resolveAudioFile finds the audio file on disk.
func (h *CallsHandler) resolveAudioFile(audioPath, callFilename string) string {
	return audio.ResolveFile(h.audioDir, h.trAudioDir, audioPath, callFilename)
}

// GetCallFrequencies returns frequency entries for a call. A call outside the
// caller's restriction is 404, like a missing one.
func (h *CallsHandler) GetCallFrequencies(w http.ResponseWriter, r *http.Request) {
	id, err := PathInt64(r, "id")
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid call ID")
		return
	}
	p, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}
	if !requireCallAccess(w, r, h.db, id, "call not found") {
		return
	}

	freqs, err := h.db.GetCallFrequencies(r.Context(), PrincipalFrom(r), id)
	if err != nil {
		writeLookupError(w, err, "call not found", "failed to get call frequencies")
		return
	}

	total := len(freqs)
	if p.Offset > 0 || p.Limit < total {
		if p.Offset >= total {
			freqs = []database.CallFrequencyAPI{}
		} else {
			end := p.Offset + p.Limit
			if end > total {
				end = total
			}
			freqs = freqs[p.Offset:end]
		}
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"frequencies": freqs,
		"total":       total,
		"limit":       p.Limit,
		"offset":      p.Offset,
	})
}

// GetCallTransmissions returns transmission entries for a call. A call
// outside the caller's restriction is 404, like a missing one.
func (h *CallsHandler) GetCallTransmissions(w http.ResponseWriter, r *http.Request) {
	id, err := PathInt64(r, "id")
	if err != nil {
		WriteError(w, http.StatusBadRequest, "invalid call ID")
		return
	}
	p, err := ParsePagination(r)
	if err != nil {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidParameter, err.Error())
		return
	}
	if !requireCallAccess(w, r, h.db, id, "call not found") {
		return
	}

	txs, err := h.db.GetCallTransmissions(r.Context(), PrincipalFrom(r), id)
	if err != nil {
		writeLookupError(w, err, "call not found", "failed to get call transmissions")
		return
	}

	total := len(txs)
	if p.Offset > 0 || p.Limit < total {
		if p.Offset >= total {
			txs = []database.CallTransmissionAPI{}
		} else {
			end := p.Offset + p.Limit
			if end > total {
				end = total
			}
			txs = txs[p.Offset:end]
		}
	}

	WriteJSON(w, http.StatusOK, map[string]any{
		"transmissions": txs,
		"total":         total,
		"limit":         p.Limit,
		"offset":        p.Offset,
	})
}

// Routes registers call routes on the given router.
func (h *CallsHandler) Routes(r chi.Router) {
	r.Get("/calls", h.ListCalls)
	r.Get("/calls/active", h.ListActiveCalls)
	r.Get("/calls/{id}", h.GetCall)
	r.Get("/calls/{id}/audio", h.GetCallAudio)
	r.Get("/calls/{id}/frequencies", h.GetCallFrequencies)
	r.Get("/calls/{id}/transmissions", h.GetCallTransmissions)
}
