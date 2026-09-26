package ingest

import (
	"context"
	"errors"
	"testing"

	"github.com/snarg/tr-engine/internal/api"
)

// Format detection is tested in api/upload_test.go (detectUploadFormat).

// ── ParseRdioScannerFields ──────────────────────────────────────────────

func TestParseRdioScannerFields_Basic(t *testing.T) {
	fields := map[string]string{
		"talkgroup":   "12345",
		"frequency":   "851000000",
		"dateTime":    "1700000000",
		"systemLabel": "butco",
		"audioType":   "m4a",
	}
	meta, err := ParseRdioScannerFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Talkgroup != 12345 {
		t.Errorf("Talkgroup = %d, want 12345", meta.Talkgroup)
	}
	if meta.Freq != 851000000 {
		t.Errorf("Freq = %f, want 851000000", meta.Freq)
	}
	if meta.StartTime != 1700000000 {
		t.Errorf("StartTime = %d, want 1700000000", meta.StartTime)
	}
	if meta.ShortName != "butco" {
		t.Errorf("ShortName = %q, want %q", meta.ShortName, "butco")
	}
	if meta.AudioType != "m4a" {
		t.Errorf("AudioType = %q, want %q", meta.AudioType, "m4a")
	}
}

func TestParseRdioScannerFields_MissingTalkgroup(t *testing.T) {
	fields := map[string]string{
		"frequency":   "851000000",
		"systemLabel": "butco",
	}
	_, err := ParseRdioScannerFields(fields)
	if err == nil {
		t.Fatal("expected error for missing talkgroup, got nil")
	}
}

func TestParseRdioScannerFields_InvalidTalkgroup(t *testing.T) {
	fields := map[string]string{
		"talkgroup": "not_a_number",
	}
	_, err := ParseRdioScannerFields(fields)
	if err == nil {
		t.Fatal("expected error for invalid talkgroup, got nil")
	}
}

func TestParseRdioScannerFields_TalkgroupMetadata(t *testing.T) {
	fields := map[string]string{
		"talkgroup":      "12345",
		"talkgroupLabel": "Fire Dispatch",
		"talkgroupName":  "Butler Co Fire/EMS Dispatch",
		"talkgroupTag":   "Fire-Tac",
		"talkgroupGroup": "Fire",
	}
	meta, err := ParseRdioScannerFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.TalkgroupTag != "Fire Dispatch" {
		t.Errorf("TalkgroupTag = %q, want %q", meta.TalkgroupTag, "Fire Dispatch")
	}
	if meta.TalkgroupDesc != "Butler Co Fire/EMS Dispatch" {
		t.Errorf("TalkgroupDesc = %q, want %q", meta.TalkgroupDesc, "Butler Co Fire/EMS Dispatch")
	}
	if meta.TalkgroupGroupTag != "Fire-Tac" {
		t.Errorf("TalkgroupGroupTag = %q, want %q", meta.TalkgroupGroupTag, "Fire-Tac")
	}
	if meta.TalkgroupGroup != "Fire" {
		t.Errorf("TalkgroupGroup = %q, want %q", meta.TalkgroupGroup, "Fire")
	}
}

func TestParseRdioScannerFields_Emergency(t *testing.T) {
	fields := map[string]string{
		"talkgroup": "100",
		"emergency": "true",
		"encrypted": "1",
	}
	meta, err := ParseRdioScannerFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Emergency != 1 {
		t.Errorf("Emergency = %d, want 1", meta.Emergency)
	}
	if meta.Encrypted != 1 {
		t.Errorf("Encrypted = %d, want 1", meta.Encrypted)
	}
}

func TestParseRdioScannerFields_Sources(t *testing.T) {
	fields := map[string]string{
		"talkgroup": "100",
		"sources":   `[{"src":54321,"time":1700000000,"pos":0.0,"emergency":0,"signal_system":"","tag":"Unit 1"}]`,
	}
	meta, err := ParseRdioScannerFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(meta.SrcList) != 1 {
		t.Fatalf("SrcList length = %d, want 1", len(meta.SrcList))
	}
	if meta.SrcList[0].Src != 54321 {
		t.Errorf("SrcList[0].Src = %d, want 54321", meta.SrcList[0].Src)
	}
	if meta.SrcList[0].Tag != "Unit 1" {
		t.Errorf("SrcList[0].Tag = %q, want %q", meta.SrcList[0].Tag, "Unit 1")
	}
}

func TestParseRdioScannerFields_SourcesTagOTA(t *testing.T) {
	fields := map[string]string{
		"talkgroup": "100",
		"sources":   `[{"src":338,"time":1700000000,"pos":0.0,"tag":"FRNSW - P 338 - Jindabyne","tag_ota":"P338 FF1"},{"src":339,"time":1700000003,"pos":3.0,"tag":"Unit 2"}]`,
	}
	meta, err := ParseRdioScannerFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(meta.SrcList) != 2 {
		t.Fatalf("SrcList length = %d, want 2", len(meta.SrcList))
	}
	if meta.SrcList[0].TagOTA != "P338 FF1" {
		t.Errorf("SrcList[0].TagOTA = %q, want %q", meta.SrcList[0].TagOTA, "P338 FF1")
	}
	if meta.SrcList[1].TagOTA != "" {
		t.Errorf("SrcList[1].TagOTA = %q, want empty", meta.SrcList[1].TagOTA)
	}
}

func TestParseRdioScannerFields_Frequencies(t *testing.T) {
	fields := map[string]string{
		"talkgroup":   "100",
		"frequencies": `[{"freq":851000000,"time":1700000000,"pos":0.0,"len":1.5,"error_count":2,"spike_count":1}]`,
	}
	meta, err := ParseRdioScannerFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(meta.FreqList) != 1 {
		t.Fatalf("FreqList length = %d, want 1", len(meta.FreqList))
	}
	if meta.FreqList[0].Freq != 851000000 {
		t.Errorf("FreqList[0].Freq = %f, want 851000000", meta.FreqList[0].Freq)
	}
	if meta.FreqList[0].ErrorCount != 2 {
		t.Errorf("FreqList[0].ErrorCount = %d, want 2", meta.FreqList[0].ErrorCount)
	}
}

func TestParseRdioScannerFields_CallLengthFromStopTime(t *testing.T) {
	fields := map[string]string{
		"talkgroup": "100",
		"dateTime":  "1700000000",
		"stopTime":  "1700000010",
	}
	meta, err := ParseRdioScannerFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.CallLength != 10 {
		t.Errorf("CallLength = %d, want 10 (derived from stop-start)", meta.CallLength)
	}
}

func TestParseRdioScannerFields_ExplicitCallLength(t *testing.T) {
	fields := map[string]string{
		"talkgroup":  "100",
		"dateTime":   "1700000000",
		"stopTime":   "1700000010",
		"callLength": "15",
	}
	meta, err := ParseRdioScannerFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Explicit callLength should be used, not derived
	if meta.CallLength != 15 {
		t.Errorf("CallLength = %d, want 15 (explicit value)", meta.CallLength)
	}
}

func TestParseRdioScannerFields_AlternateFieldNames(t *testing.T) {
	// Test the fallback field names
	fields := map[string]string{
		"talkgroup":  "100",
		"freq":       "852000000",
		"system":     "warco",
		"audio_type": "wav",
	}
	meta, err := ParseRdioScannerFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Freq != 852000000 {
		t.Errorf("Freq = %f, want 852000000 (from 'freq' fallback)", meta.Freq)
	}
	if meta.ShortName != "warco" {
		t.Errorf("ShortName = %q, want %q (from 'system' fallback)", meta.ShortName, "warco")
	}
	if meta.AudioType != "wav" {
		t.Errorf("AudioType = %q, want %q (from 'audio_type' fallback)", meta.AudioType, "wav")
	}
}

// ── ParseOpenMHzFields ──────────────────────────────────────────────────

func TestParseOpenMHzFields_Basic(t *testing.T) {
	fields := map[string]string{
		"talkgroup_num": "54321",
		"freq":          "851500000",
		"start_time":    "1700000000",
		"stop_time":     "1700000008",
	}
	meta, err := ParseOpenMHzFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Talkgroup != 54321 {
		t.Errorf("Talkgroup = %d, want 54321", meta.Talkgroup)
	}
	if meta.Freq != 851500000 {
		t.Errorf("Freq = %f, want 851500000", meta.Freq)
	}
	if meta.StartTime != 1700000000 {
		t.Errorf("StartTime = %d, want 1700000000", meta.StartTime)
	}
	if meta.StopTime != 1700000008 {
		t.Errorf("StopTime = %d, want 1700000008", meta.StopTime)
	}
	// CallLength should be derived from stop-start
	if meta.CallLength != 8 {
		t.Errorf("CallLength = %d, want 8 (derived from stop-start)", meta.CallLength)
	}
}

func TestParseOpenMHzFields_MissingTalkgroupNum(t *testing.T) {
	fields := map[string]string{
		"freq":       "851000000",
		"start_time": "1700000000",
	}
	_, err := ParseOpenMHzFields(fields)
	if err == nil {
		t.Fatal("expected error for missing talkgroup_num, got nil")
	}
}

func TestParseOpenMHzFields_InvalidTalkgroupNum(t *testing.T) {
	fields := map[string]string{
		"talkgroup_num": "invalid",
	}
	_, err := ParseOpenMHzFields(fields)
	if err == nil {
		t.Fatal("expected error for invalid talkgroup_num, got nil")
	}
}

func TestParseOpenMHzFields_Emergency(t *testing.T) {
	fields := map[string]string{
		"talkgroup_num": "100",
		"emergency":     "1",
	}
	meta, err := ParseOpenMHzFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.Emergency != 1 {
		t.Errorf("Emergency = %d, want 1", meta.Emergency)
	}
}

func TestParseOpenMHzFields_ErrorCount(t *testing.T) {
	fields := map[string]string{
		"talkgroup_num": "100",
		"error_count":   "5",
	}
	meta, err := ParseOpenMHzFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.FreqError != 5 {
		t.Errorf("FreqError = %d, want 5", meta.FreqError)
	}
}

func TestParseOpenMHzFields_SourceList(t *testing.T) {
	fields := map[string]string{
		"talkgroup_num": "100",
		"source_list":   `[{"src":99999,"time":1700000000,"pos":0.0,"emergency":0,"signal_system":"","tag":""}]`,
	}
	meta, err := ParseOpenMHzFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(meta.SrcList) != 1 {
		t.Fatalf("SrcList length = %d, want 1", len(meta.SrcList))
	}
	if meta.SrcList[0].Src != 99999 {
		t.Errorf("SrcList[0].Src = %d, want 99999", meta.SrcList[0].Src)
	}
}

func TestParseOpenMHzFields_FreqList(t *testing.T) {
	fields := map[string]string{
		"talkgroup_num": "100",
		"freq_list":     `[{"freq":851000000,"time":1700000000,"pos":0.0,"len":2.0,"error_count":0,"spike_count":0}]`,
	}
	meta, err := ParseOpenMHzFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(meta.FreqList) != 1 {
		t.Fatalf("FreqList length = %d, want 1", len(meta.FreqList))
	}
	if meta.FreqList[0].Len != 2.0 {
		t.Errorf("FreqList[0].Len = %f, want 2.0", meta.FreqList[0].Len)
	}
}

func TestParseOpenMHzFields_ShortNameEmpty(t *testing.T) {
	// OpenMHz typically doesn't send short_name
	fields := map[string]string{
		"talkgroup_num": "100",
	}
	meta, err := ParseOpenMHzFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if meta.ShortName != "" {
		t.Errorf("ShortName = %q, want empty string", meta.ShortName)
	}
}

func TestParseOpenMHzFields_ExplicitCallLength(t *testing.T) {
	fields := map[string]string{
		"talkgroup_num": "100",
		"start_time":    "1700000000",
		"stop_time":     "1700000010",
		"call_length":   "12",
	}
	meta, err := ParseOpenMHzFields(fields)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Explicit call_length should be used
	if meta.CallLength != 12 {
		t.Errorf("CallLength = %d, want 12 (explicit value)", meta.CallLength)
	}
}

// ── parseBoolInt helper ─────────────────────────────────────────────────

func TestParseBoolInt(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"true", 1},
		{"True", 1},
		{"TRUE", 1},
		{"1", 1},
		{"false", 0},
		{"0", 0},
		{"", 0},
		{"no", 0},
		{"yes", 0}, // only "true" and "1" are truthy
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseBoolInt(tt.input)
			if got != tt.want {
				t.Errorf("parseBoolInt(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// ── firstNonEmpty helper ────────────────────────────────────────────────

func TestFirstNonEmpty(t *testing.T) {
	fields := map[string]string{
		"a": "",
		"b": "hello",
		"c": "world",
	}

	// First key is empty, should fall through to "b"
	got := firstNonEmpty(fields, "a", "b", "c")
	if got != "hello" {
		t.Errorf("firstNonEmpty = %q, want %q", got, "hello")
	}

	// No matching key
	got = firstNonEmpty(fields, "x", "y")
	if got != "" {
		t.Errorf("firstNonEmpty = %q, want empty string", got)
	}

	// First key matches
	got = firstNonEmpty(fields, "c")
	if got != "world" {
		t.Errorf("firstNonEmpty = %q, want %q", got, "world")
	}
}

// Field errors are the uploader's fault: they wrap api.ErrInvalidUpload so
// the handler answers 400 rather than 500. They are found before the
// pipeline touches anything.
func TestProcessUpload_InvalidFieldsWrapErrInvalidUpload(t *testing.T) {
	p := &Pipeline{}
	for _, c := range []struct {
		format string
		fields map[string]string
	}{
		{"rdio-scanner", map[string]string{"systemLabel": "butco"}},
		{"rdio-scanner", map[string]string{"talkgroup": "abc"}},
		{"openmhz", map[string]string{"start_time": "1708881234"}},
		{"bogus", map[string]string{"talkgroup": "1"}},
	} {
		_, err := p.ProcessUpload(context.Background(), "http-upload", c.format, c.fields, nil, "")
		if !errors.Is(err, api.ErrInvalidUpload) {
			t.Errorf("%s %v: err = %v, want one wrapping api.ErrInvalidUpload", c.format, c.fields, err)
		}
	}
}
