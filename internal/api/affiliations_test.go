package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// mockLiveData implements LiveDataSource for testing affiliations.
type mockLiveData struct {
	affiliations []UnitAffiliationData
}

func (m *mockLiveData) ActiveCalls() []ActiveCallData                   { return nil }
func (m *mockLiveData) LatestRecorders() []RecorderStateData            { return nil }
func (m *mockLiveData) TRInstanceStatus() []TRInstanceStatusData        { return nil }
func (m *mockLiveData) UnitAffiliations() []UnitAffiliationData         { return m.affiliations }
func (m *mockLiveData) Subscribe(EventFilter) (<-chan SSEEvent, func()) { return nil, func() {} }
func (m *mockLiveData) ReplaySince(string, EventFilter) []SSEEvent      { return nil }
func (m *mockLiveData) WatcherStatus() *WatcherStatusData               { return nil }
func (m *mockLiveData) TranscriptionStatus() *TranscriptionStatusData   { return nil }
func (m *mockLiveData) EnqueueTranscription(int64) bool                 { return false }
func (m *mockLiveData) TranscriptionQueueStats() *TranscriptionQueueStatsData { return nil }
func (m *mockLiveData) IngestMetrics() *IngestMetricsData                     { return nil }
func (m *mockLiveData) MaintenanceStatus() *MaintenanceStatusData             { return nil }
func (m *mockLiveData) RunMaintenance(context.Context) (*MaintenanceRunData, error) { return nil, nil }
func (m *mockLiveData) SubmitBackfill(context.Context, BackfillFiltersData) (int, int, int, error) { return 0, 0, 0, nil }
func (m *mockLiveData) BackfillStatus() *BackfillStatusData { return nil }
func (m *mockLiveData) CancelBackfill(int) bool { return false }

// affiliationsResponse matches the JSON shape returned by ListAffiliations.
type affiliationsResponse struct {
	Affiliations []UnitAffiliationData  `json:"affiliations"`
	Total        int                    `json:"total"`
	Limit        int                    `json:"limit"`
	Offset       int                    `json:"offset"`
	Summary      map[string]interface{} `json:"summary"`
}

func decodeAffiliations(t *testing.T, rec *httptest.ResponseRecorder) affiliationsResponse {
	t.Helper()
	var resp affiliationsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("JSON decode: %v", err)
	}
	return resp
}

func sampleAffiliations() []UnitAffiliationData {
	now := time.Now().UTC()
	return []UnitAffiliationData{
		{SystemID: 1, Sysid: "BEE00", UnitID: 42, Tgid: 100, Status: "affiliated", LastEventTime: now, AffiliatedSince: now.Add(-10 * time.Minute)},
		{SystemID: 1, Sysid: "BEE00", UnitID: 43, Tgid: 200, Status: "affiliated", LastEventTime: now, AffiliatedSince: now.Add(-5 * time.Minute)},
		{SystemID: 2, Sysid: "BEE01", UnitID: 50, Tgid: 100, Status: "off", LastEventTime: now.Add(-2 * time.Minute), AffiliatedSince: now.Add(-30 * time.Minute)},
		{SystemID: 2, Sysid: "BEE01", UnitID: 51, Tgid: 300, Status: "affiliated", LastEventTime: now.Add(-10 * time.Minute), AffiliatedSince: now.Add(-20 * time.Minute)},
		{SystemID: 1, Sysid: "BEE00", UnitID: 44, Tgid: 100, Status: "affiliated", LastEventTime: now, AffiliatedSince: now.Add(-1 * time.Minute)},
	}
}

func TestListAffiliations(t *testing.T) {
	t.Run("nil_live_returns_empty", func(t *testing.T) {
		h := NewAffiliationsHandler(nil)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/unit-affiliations", nil)
		h.ListAffiliations(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		resp := decodeAffiliations(t, rec)
		if len(resp.Affiliations) != 0 {
			t.Errorf("affiliations len = %d, want 0", len(resp.Affiliations))
		}
		if resp.Total != 0 {
			t.Errorf("total = %d, want 0", resp.Total)
		}
	})

	t.Run("returns_all_affiliations", func(t *testing.T) {
		h := NewAffiliationsHandler(&mockLiveData{affiliations: sampleAffiliations()})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/unit-affiliations", nil)
		h.ListAffiliations(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
		}
		resp := decodeAffiliations(t, rec)
		if resp.Total != 5 {
			t.Errorf("total = %d, want 5", resp.Total)
		}
	})

	t.Run("filter_by_system_id", func(t *testing.T) {
		h := NewAffiliationsHandler(&mockLiveData{affiliations: sampleAffiliations()})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/unit-affiliations?system_id=1", nil)
		h.ListAffiliations(rec, req)

		resp := decodeAffiliations(t, rec)
		if resp.Total != 3 {
			t.Errorf("total = %d, want 3", resp.Total)
		}
		for _, a := range resp.Affiliations {
			if a.SystemID != 1 {
				t.Errorf("got SystemID=%d, want 1", a.SystemID)
			}
		}
	})

	t.Run("filter_by_sysid", func(t *testing.T) {
		h := NewAffiliationsHandler(&mockLiveData{affiliations: sampleAffiliations()})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/unit-affiliations?sysid=BEE01", nil)
		h.ListAffiliations(rec, req)

		resp := decodeAffiliations(t, rec)
		if resp.Total != 2 {
			t.Errorf("total = %d, want 2", resp.Total)
		}
		for _, a := range resp.Affiliations {
			if a.Sysid != "BEE01" {
				t.Errorf("got Sysid=%q, want BEE01", a.Sysid)
			}
		}
	})

	t.Run("filter_by_tgid", func(t *testing.T) {
		h := NewAffiliationsHandler(&mockLiveData{affiliations: sampleAffiliations()})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/unit-affiliations?tgid=100", nil)
		h.ListAffiliations(rec, req)

		resp := decodeAffiliations(t, rec)
		if resp.Total != 3 {
			t.Errorf("total = %d, want 3 (tgid=100 appears in sys1 and sys2)", resp.Total)
		}
	})

	t.Run("filter_by_unit_id", func(t *testing.T) {
		h := NewAffiliationsHandler(&mockLiveData{affiliations: sampleAffiliations()})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/unit-affiliations?unit_id=42", nil)
		h.ListAffiliations(rec, req)

		resp := decodeAffiliations(t, rec)
		if resp.Total != 1 {
			t.Errorf("total = %d, want 1", resp.Total)
		}
		if len(resp.Affiliations) > 0 && resp.Affiliations[0].UnitID != 42 {
			t.Errorf("UnitID = %d, want 42", resp.Affiliations[0].UnitID)
		}
	})

	t.Run("filter_by_status", func(t *testing.T) {
		h := NewAffiliationsHandler(&mockLiveData{affiliations: sampleAffiliations()})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/unit-affiliations?status=affiliated", nil)
		h.ListAffiliations(rec, req)

		resp := decodeAffiliations(t, rec)
		// 4 affiliated out of 5 total
		if resp.Total != 4 {
			t.Errorf("total = %d, want 4", resp.Total)
		}
		for _, a := range resp.Affiliations {
			if a.Status != "affiliated" {
				t.Errorf("got status=%q, want affiliated", a.Status)
			}
		}
	})

	t.Run("filter_stale_threshold", func(t *testing.T) {
		h := NewAffiliationsHandler(&mockLiveData{affiliations: sampleAffiliations()})
		rec := httptest.NewRecorder()
		// stale_threshold=60 means exclude events older than 60s
		req := httptest.NewRequest("GET", "/unit-affiliations?stale_threshold=60", nil)
		h.ListAffiliations(rec, req)

		resp := decodeAffiliations(t, rec)
		// Only items with LastEventTime within 60s of now should remain
		// Sample data: items 0,1,4 have LastEventTime=now (within 60s), item 2 is 2min ago, item 3 is 10min ago
		if resp.Total != 3 {
			t.Errorf("total = %d, want 3", resp.Total)
		}
	})

	t.Run("filter_active_within", func(t *testing.T) {
		h := NewAffiliationsHandler(&mockLiveData{affiliations: sampleAffiliations()})
		rec := httptest.NewRecorder()
		// active_within=300 (5min) excludes item 3 (10min ago)
		req := httptest.NewRequest("GET", "/unit-affiliations?active_within=300", nil)
		h.ListAffiliations(rec, req)

		resp := decodeAffiliations(t, rec)
		if resp.Total != 4 {
			t.Errorf("total = %d, want 4", resp.Total)
		}
	})

	t.Run("combined_filters", func(t *testing.T) {
		h := NewAffiliationsHandler(&mockLiveData{affiliations: sampleAffiliations()})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/unit-affiliations?system_id=1&tgid=100&status=affiliated", nil)
		h.ListAffiliations(rec, req)

		resp := decodeAffiliations(t, rec)
		// System 1 + tgid 100 + affiliated: items 0 and 4
		if resp.Total != 2 {
			t.Errorf("total = %d, want 2", resp.Total)
		}
	})

	t.Run("sorted_by_tgid_then_unit", func(t *testing.T) {
		h := NewAffiliationsHandler(&mockLiveData{affiliations: sampleAffiliations()})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/unit-affiliations", nil)
		h.ListAffiliations(rec, req)

		resp := decodeAffiliations(t, rec)
		for i := 1; i < len(resp.Affiliations); i++ {
			prev := resp.Affiliations[i-1]
			curr := resp.Affiliations[i]
			if prev.Tgid > curr.Tgid {
				t.Errorf("not sorted by tgid: [%d].Tgid=%d > [%d].Tgid=%d", i-1, prev.Tgid, i, curr.Tgid)
			}
			if prev.Tgid == curr.Tgid && prev.UnitID > curr.UnitID {
				t.Errorf("not sorted by unit_id within tgid=%d: [%d].UnitID=%d > [%d].UnitID=%d", prev.Tgid, i-1, prev.UnitID, i, curr.UnitID)
			}
		}
	})

	t.Run("pagination", func(t *testing.T) {
		h := NewAffiliationsHandler(&mockLiveData{affiliations: sampleAffiliations()})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/unit-affiliations?limit=2&offset=1", nil)
		h.ListAffiliations(rec, req)

		resp := decodeAffiliations(t, rec)
		if resp.Total != 5 {
			t.Errorf("total = %d, want 5 (total reflects full set)", resp.Total)
		}
		if len(resp.Affiliations) != 2 {
			t.Errorf("page len = %d, want 2", len(resp.Affiliations))
		}
		if resp.Limit != 2 {
			t.Errorf("limit = %d, want 2", resp.Limit)
		}
		if resp.Offset != 1 {
			t.Errorf("offset = %d, want 1", resp.Offset)
		}
	})

	t.Run("pagination_offset_beyond_total", func(t *testing.T) {
		h := NewAffiliationsHandler(&mockLiveData{affiliations: sampleAffiliations()})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/unit-affiliations?limit=10&offset=100", nil)
		h.ListAffiliations(rec, req)

		resp := decodeAffiliations(t, rec)
		if resp.Total != 5 {
			t.Errorf("total = %d, want 5", resp.Total)
		}
		if len(resp.Affiliations) != 0 {
			t.Errorf("page len = %d, want 0", len(resp.Affiliations))
		}
	})

	t.Run("summary_counts_only_affiliated", func(t *testing.T) {
		h := NewAffiliationsHandler(&mockLiveData{affiliations: sampleAffiliations()})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", "/unit-affiliations", nil)
		h.ListAffiliations(rec, req)

		resp := decodeAffiliations(t, rec)
		rawCounts, ok := resp.Summary["talkgroup_counts"]
		if !ok {
			t.Fatal("missing talkgroup_counts in summary")
		}
		counts, ok := rawCounts.(map[string]interface{})
		if !ok {
			t.Fatal("talkgroup_counts is not a map")
		}

		// tgid 100: 2 affiliated (items 0,4; item 2 is "off"), tgid 200: 1, tgid 300: 1
		if v, ok := counts["100"]; !ok || int(v.(float64)) != 2 {
			t.Errorf("talkgroup_counts[100] = %v, want 2", v)
		}
		if v, ok := counts["200"]; !ok || int(v.(float64)) != 1 {
			t.Errorf("talkgroup_counts[200] = %v, want 1", v)
		}
		if v, ok := counts["300"]; !ok || int(v.(float64)) != 1 {
			t.Errorf("talkgroup_counts[300] = %v, want 1", v)
		}
	})
}
