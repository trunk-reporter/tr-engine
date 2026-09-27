package database

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/snarg/tr-engine/internal/auth"
	"github.com/snarg/tr-engine/internal/database/sqlcdb"
)

// TranscriptionRow is the input for inserting a transcription.
type TranscriptionRow struct {
	CallID        int64
	CallStartTime time.Time
	Text          string
	Source        string // "auto", "human", "llm"
	IsPrimary     bool
	Confidence    *float32
	Language      string
	Model         string
	Provider      string
	WordCount     int
	DurationMs    int
	ProviderMs    *int
	Words         json.RawMessage // word-level timestamps with unit attribution
}

// ErrInvalidTranscriptionSource is returned by InsertTranscription for a
// source that the transcriptions.source CHECK constraint doesn't accept.
var ErrInvalidTranscriptionSource = errors.New("invalid transcription source")

// transcriptionStatusForSource maps every source the transcriptions.source
// CHECK accepts (auto, human, llm) to the transcription_status that a primary
// transcription from it gives its call and call group, whose CHECK has no
// source values: a human transcription is verified; an automatic or
// LLM-generated one is machine output that nobody has reviewed.
var transcriptionStatusForSource = map[string]string{
	"auto":  "auto",
	"human": "verified",
	"llm":   "auto",
}

// ValidTranscriptionSource reports whether InsertTranscription accepts
// source.
func ValidTranscriptionSource(source string) bool {
	_, ok := transcriptionStatusForSource[source]
	return ok
}

// TranscriptionAPI is the transcription representation for API responses.
type TranscriptionAPI struct {
	ID         int             `json:"id"`
	CallID     int64           `json:"call_id"`
	Text       string          `json:"text"`
	Source     string          `json:"source"`
	IsPrimary  bool            `json:"is_primary"`
	Confidence *float32        `json:"confidence,omitempty"`
	Language   string          `json:"language,omitempty"`
	Model      string          `json:"model,omitempty"`
	Provider   string          `json:"provider,omitempty"`
	WordCount  int             `json:"word_count"`
	DurationMs int             `json:"duration_ms"`
	ProviderMs *int            `json:"provider_ms,omitempty"`
	Words      json.RawMessage `json:"words,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

// CallTranscriptionInfo is a lightweight view of a call for the transcription worker.
type CallTranscriptionInfo struct {
	CallID           int64
	StartTime        time.Time
	SystemID         int
	Tgid             int
	Duration         *float32
	AudioFilePath    string
	CallFilename     string
	SrcList          json.RawMessage
	Encrypted        bool
	HasTranscription bool
	TgAlphaTag       string
	TgDescription    string
	TgTag            string
	TgGroup          string
}

// TranscriptionSearchFilter specifies filters for full-text search.
type TranscriptionSearchFilter struct {
	SystemIDs   []int
	SiteIDs     []int
	Tgids       []int
	StartTime   *time.Time
	EndTime     *time.Time
	PrimaryOnly *bool // default true; set to false to include all variants
	Limit     int
	Offset    int
}

// TranscriptionSearchHit is a search result with relevance score and call context.
type TranscriptionSearchHit struct {
	TranscriptionAPI
	Rank           float32   `json:"rank"`
	CallSystemID   int       `json:"system_id"`
	CallSystemName string    `json:"system_name,omitempty"`
	CallTgid       int       `json:"tgid"`
	CallTgAlphaTag string    `json:"tg_alpha_tag,omitempty"`
	CallStartTime  time.Time `json:"call_start_time"`
	CallDuration   *float32  `json:"call_duration,omitempty"`
}

func primaryTranscriptionToAPI(r sqlcdb.GetPrimaryTranscriptionRow) TranscriptionAPI {
	t := TranscriptionAPI{
		ID:         r.ID,
		CallID:     r.CallID,
		Source:     r.Source,
		IsPrimary:  r.IsPrimary,
		Confidence: r.Confidence,
		Words:      r.Words,
	}
	if r.Text != nil {
		t.Text = *r.Text
	}
	if r.Language != nil {
		t.Language = *r.Language
	}
	if r.Model != nil {
		t.Model = *r.Model
	}
	if r.Provider != nil {
		t.Provider = *r.Provider
	}
	if r.WordCount != nil {
		t.WordCount = int(*r.WordCount)
	}
	if r.DurationMs != nil {
		t.DurationMs = int(*r.DurationMs)
	}
	if r.ProviderMs != nil {
		pm := int(*r.ProviderMs)
		t.ProviderMs = &pm
	}
	if r.CreatedAt.Valid {
		t.CreatedAt = r.CreatedAt.Time
	}
	return t
}

func listTranscriptionToAPI(r sqlcdb.ListTranscriptionsByCallRow) TranscriptionAPI {
	t := TranscriptionAPI{
		ID:         r.ID,
		CallID:     r.CallID,
		Source:     r.Source,
		IsPrimary:  r.IsPrimary,
		Confidence: r.Confidence,
		Words:      r.Words,
	}
	if r.Text != nil {
		t.Text = *r.Text
	}
	if r.Language != nil {
		t.Language = *r.Language
	}
	if r.Model != nil {
		t.Model = *r.Model
	}
	if r.Provider != nil {
		t.Provider = *r.Provider
	}
	if r.WordCount != nil {
		t.WordCount = int(*r.WordCount)
	}
	if r.DurationMs != nil {
		t.DurationMs = int(*r.DurationMs)
	}
	if r.ProviderMs != nil {
		pm := int(*r.ProviderMs)
		t.ProviderMs = &pm
	}
	if r.CreatedAt.Valid {
		t.CreatedAt = r.CreatedAt.Time
	}
	return t
}

// InsertTranscription inserts a new transcription in a transaction:
// 1) Clears is_primary on existing transcriptions for this call
// 2) Inserts the new transcription
// 3) Updates the calls table denormalized fields
// 4) Updates the call_groups table transcription fields
func (db *DB) InsertTranscription(ctx context.Context, row *TranscriptionRow) (int, error) {
	status, ok := transcriptionStatusForSource[row.Source]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrInvalidTranscriptionSource, row.Source)
	}

	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	qtx := db.Q.WithTx(tx)

	if row.IsPrimary {
		if err := qtx.ClearPrimaryTranscription(ctx, sqlcdb.ClearPrimaryTranscriptionParams{
			CallID:        row.CallID,
			CallStartTime: pgtype.Timestamptz{Time: row.CallStartTime, Valid: true},
		}); err != nil {
			return 0, fmt.Errorf("clear is_primary: %w", err)
		}
	}

	wc := int32(row.WordCount)
	dm := int32(row.DurationMs)
	var pm *int32
	if row.ProviderMs != nil {
		v := int32(*row.ProviderMs)
		pm = &v
	}
	id, err := qtx.InsertTranscriptionRow(ctx, sqlcdb.InsertTranscriptionRowParams{
		CallID:        row.CallID,
		CallStartTime: pgtype.Timestamptz{Time: row.CallStartTime, Valid: true},
		Text:          &row.Text,
		Source:        row.Source,
		IsPrimary:     row.IsPrimary,
		Confidence:    row.Confidence,
		Language:      &row.Language,
		Model:         &row.Model,
		Provider:      &row.Provider,
		WordCount:     &wc,
		DurationMs:    &dm,
		ProviderMs:    pm,
		Words:         row.Words,
	})
	if err != nil {
		return 0, fmt.Errorf("insert transcription: %w", err)
	}

	if row.IsPrimary {
		if err := qtx.UpdateCallTranscriptionDenorm(ctx, sqlcdb.UpdateCallTranscriptionDenormParams{
			CallID:                 row.CallID,
			StartTime:              pgtype.Timestamptz{Time: row.CallStartTime, Valid: true},
			TranscriptionStatus:    status,
			TranscriptionText:      &row.Text,
			TranscriptionWordCount: &wc,
		}); err != nil {
			return 0, fmt.Errorf("update calls denorm: %w", err)
		}

		if err := qtx.UpdateCallGroupTranscriptionDenorm(ctx, sqlcdb.UpdateCallGroupTranscriptionDenormParams{
			CallID:              row.CallID,
			StartTime:           pgtype.Timestamptz{Time: row.CallStartTime, Valid: true},
			TranscriptionText:   &row.Text,
			TranscriptionStatus: &status,
		}); err != nil {
			return 0, fmt.Errorf("update call_groups denorm: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit tx: %w", err)
	}
	return id, nil
}

// callTranscriptionColumns are the transcription columns of the per-call
// reads, in the order the sqlc row types scan them.
const callTranscriptionColumns = `t.id, t.call_id, t.text, t.source, t.is_primary,
	t.confidence, t.language, t.model, t.provider,
	t.word_count, t.duration_ms, t.provider_ms, t.words, t.created_at`

// callTranscriptionsFrom joins each transcription to its call, whose system
// and talkgroup the restriction clause checks.
const callTranscriptionsFrom = ` FROM transcriptions t
	JOIN calls c ON c.call_id = t.call_id AND c.start_time = t.call_start_time
	WHERE t.call_id = $1`

// GetPrimaryTranscription returns the primary transcription for a call p may
// see. A call outside p's restriction has none (pgx.ErrNoRows). A nil p is
// ErrNoPrincipal.
func (db *DB) GetPrimaryTranscription(ctx context.Context, p *auth.Principal, callID int64) (*TranscriptionAPI, error) {
	restrict, restrictArgs, err := restrictSQL(p, "c.system_id", "c.tgid", 2)
	if err != nil {
		return nil, err
	}
	var row sqlcdb.GetPrimaryTranscriptionRow
	err = db.Pool.QueryRow(ctx,
		`SELECT `+callTranscriptionColumns+callTranscriptionsFrom+` AND t.is_primary = true`+restrict+`
		ORDER BY t.created_at DESC
		LIMIT 1`,
		append([]any{callID}, restrictArgs...)...,
	).Scan(
		&row.ID, &row.CallID, &row.Text, &row.Source, &row.IsPrimary,
		&row.Confidence, &row.Language, &row.Model, &row.Provider,
		&row.WordCount, &row.DurationMs, &row.ProviderMs, &row.Words, &row.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	t := primaryTranscriptionToAPI(row)
	return &t, nil
}

// ListTranscriptionsByCall returns all transcription variants for a call p
// may see; none for a call outside p's restriction. A nil p is
// ErrNoPrincipal.
func (db *DB) ListTranscriptionsByCall(ctx context.Context, p *auth.Principal, callID int64) ([]TranscriptionAPI, error) {
	restrict, restrictArgs, err := restrictSQL(p, "c.system_id", "c.tgid", 2)
	if err != nil {
		return nil, err
	}
	rows, err := db.Pool.Query(ctx,
		`SELECT `+callTranscriptionColumns+callTranscriptionsFrom+restrict+`
		ORDER BY t.created_at DESC`,
		append([]any{callID}, restrictArgs...)...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := []TranscriptionAPI{}
	for rows.Next() {
		var r sqlcdb.ListTranscriptionsByCallRow
		if err := rows.Scan(
			&r.ID, &r.CallID, &r.Text, &r.Source, &r.IsPrimary,
			&r.Confidence, &r.Language, &r.Model, &r.Provider,
			&r.WordCount, &r.DurationMs, &r.ProviderMs, &r.Words, &r.CreatedAt,
		); err != nil {
			return nil, err
		}
		result = append(result, listTranscriptionToAPI(r))
	}
	return result, rows.Err()
}

// SearchTranscriptions performs full-text search across transcriptions with call context.
// Defaults to primary transcriptions only; pass primary_only=false to include all variants.
// Results are limited to calls on talkgroups p may see: the restriction is
// part of the WHERE shared by the count and the rows (§7.2). A nil p is
// ErrNoPrincipal.
func (db *DB) SearchTranscriptions(ctx context.Context, p *auth.Principal, query string, filter TranscriptionSearchFilter) ([]TranscriptionSearchHit, int, error) {
	primaryOnly := filter.PrimaryOnly == nil || *filter.PrimaryOnly

	const fromClause = `FROM transcriptions t JOIN calls c ON c.call_id = t.call_id AND c.start_time = t.call_start_time`
	const filterClause = `
		WHERE t.search_vector @@ plainto_tsquery('english', $1)
		  AND ($2::boolean IS NOT TRUE OR t.is_primary = true)
		  AND ($3::timestamptz IS NULL OR t.call_start_time >= $3)
		  AND ($4::timestamptz IS NULL OR t.call_start_time < $4)
		  AND ($5::int[] IS NULL OR c.system_id = ANY($5))
		  AND ($6::int[] IS NULL OR c.site_id = ANY($6))
		  AND ($7::int[] IS NULL OR c.tgid = ANY($7))`
	args := []any{query, primaryOnly, filter.StartTime, filter.EndTime,
		pqIntArray(filter.SystemIDs), pqIntArray(filter.SiteIDs), pqIntArray(filter.Tgids)}
	restrict, restrictArgs, err := restrictSQL(p, "c.system_id", "c.tgid", len(args)+1)
	if err != nil {
		return nil, 0, err
	}
	whereClause := filterClause + restrict
	args = append(args, restrictArgs...)

	// Count
	var total int
	if err := db.Pool.QueryRow(ctx, "SELECT count(*) "+fromClause+whereClause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Results with rank — reuse $1 for the rank expression
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}

	// text, language, model, provider, word_count and duration_ms are
	// nullable; the per-call reads map NULL to the zero value and so does
	// this one.
	dataQuery := `
		SELECT t.id, t.call_id, COALESCE(t.text, ''), t.source, t.is_primary,
			t.confidence, COALESCE(t.language, ''), COALESCE(t.model, ''), COALESCE(t.provider, ''),
			COALESCE(t.word_count, 0), COALESCE(t.duration_ms, 0), t.provider_ms, t.words, t.created_at,
			ts_rank(t.search_vector, plainto_tsquery('english', $1)) AS rank,
			c.system_id, COALESCE(c.system_name, ''), c.tgid,
			COALESCE(c.tg_alpha_tag, ''), c.start_time, c.duration
		` + fromClause + whereClause + `
		ORDER BY rank DESC
		LIMIT ` + placeholder(len(args)+1) + ` OFFSET ` + placeholder(len(args)+2)

	rows, err := db.Pool.Query(ctx, dataQuery, append(args, limit, filter.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var hits []TranscriptionSearchHit
	for rows.Next() {
		var h TranscriptionSearchHit
		if err := rows.Scan(
			&h.ID, &h.CallID, &h.Text, &h.Source, &h.IsPrimary,
			&h.Confidence, &h.Language, &h.Model, &h.Provider,
			&h.WordCount, &h.DurationMs, &h.ProviderMs, &h.Words, &h.CreatedAt,
			&h.Rank,
			&h.CallSystemID, &h.CallSystemName, &h.CallTgid,
			&h.CallTgAlphaTag, &h.CallStartTime, &h.CallDuration,
		); err != nil {
			return nil, 0, err
		}
		hits = append(hits, h)
	}
	if hits == nil {
		hits = []TranscriptionSearchHit{}
	}
	return hits, total, rows.Err()
}

// BatchTranscriptionRow is a lightweight transcription for batch fetches.
type BatchTranscriptionRow struct {
	CallID   int64           `json:"call_id"`
	Text     string          `json:"text"`
	Segments json.RawMessage `json:"segments"`
}

// GetBatchTranscriptions returns primary transcriptions for multiple call IDs.
// Only returns call_id, text, and words->'segments' — the minimal shape needed by frontends.
// Calls outside p's restriction are silently left out (§6.3). A nil p is
// ErrNoPrincipal.
func (db *DB) GetBatchTranscriptions(ctx context.Context, p *auth.Principal, callIDs []int64) ([]BatchTranscriptionRow, error) {
	restrict, restrictArgs, err := restrictSQL(p, "c.system_id", "c.tgid", 2)
	if err != nil {
		return nil, err
	}
	if len(callIDs) == 0 {
		return []BatchTranscriptionRow{}, nil
	}

	query := `
		SELECT t.call_id, COALESCE(t.text, '') AS text, t.words->'segments' AS segments
		FROM transcriptions t
		JOIN calls c ON c.call_id = t.call_id AND c.start_time = t.call_start_time
		WHERE t.call_id = ANY($1) AND t.is_primary = true` + restrict

	rows, err := db.Pool.Query(ctx, query, append([]any{callIDs}, restrictArgs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []BatchTranscriptionRow
	for rows.Next() {
		var r BatchTranscriptionRow
		if err := rows.Scan(&r.CallID, &r.Text, &r.Segments); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	if result == nil {
		result = []BatchTranscriptionRow{}
	}
	return result, rows.Err()
}

// GetCallForTranscription returns a lightweight call view for the transcription worker.
func (db *DB) GetCallForTranscription(ctx context.Context, callID int64) (*CallTranscriptionInfo, error) {
	row, err := db.Q.GetCallForTranscription(ctx, callID)
	if err != nil {
		return nil, err
	}
	encrypted := false
	if row.Encrypted != nil {
		encrypted = *row.Encrypted
	}
	return &CallTranscriptionInfo{
		CallID:           row.CallID,
		StartTime:        row.StartTime.Time,
		SystemID:         row.SystemID,
		Tgid:             row.Tgid,
		Duration:         row.Duration,
		AudioFilePath:    row.AudioFilePath,
		CallFilename:     row.CallFilename,
		SrcList:          row.SrcList,
		Encrypted:        encrypted,
		HasTranscription: row.HasTranscription,
		TgAlphaTag:       row.TgAlphaTag,
		TgDescription:    row.TgDescription,
		TgTag:            row.TgTag,
		TgGroup:          row.TgGroup,
	}, nil
}

// TranscriptionExport contains fields needed for export.
type TranscriptionExport struct {
	SystemID      int
	Tgid          int
	CallStartTime time.Time
	Text          string
	Source        string
	IsPrimary     bool
	Confidence    *float32
	Language      string
	Model         string
	Provider      string
	WordCount     int
	DurationMs    int
	ProviderMs    *int
	Words         json.RawMessage
}

// ExportTranscriptions returns all transcriptions for the given systems and optional time range.
func (db *DB) ExportTranscriptions(ctx context.Context, systemIDs []int, start, end *time.Time) ([]TranscriptionExport, error) {
	// Check if provider_ms column exists (migration may not have been applied)
	providerMsCol := "t.provider_ms"
	if !db.columnExists(ctx, "transcriptions", "provider_ms") {
		providerMsCol = "NULL::int"
	}
	query := `
		SELECT c.system_id, c.tgid, t.call_start_time,
			COALESCE(t.text, ''), t.source, t.is_primary, t.confidence,
			COALESCE(t.language, ''), COALESCE(t.model, ''),
			COALESCE(t.provider, ''), COALESCE(t.word_count, 0),
			COALESCE(t.duration_ms, 0), ` + providerMsCol + `, t.words
		FROM transcriptions t
		JOIN calls c ON c.call_id = t.call_id AND c.start_time = t.call_start_time
		WHERE ($1::int[] IS NULL OR c.system_id = ANY($1))
		  AND ($2::timestamptz IS NULL OR t.call_start_time >= $2)
		  AND ($3::timestamptz IS NULL OR t.call_start_time < $3)
		ORDER BY t.call_start_time ASC, t.id ASC
	`
	var startArg, endArg any
	if start != nil {
		startArg = *start
	}
	if end != nil {
		endArg = *end
	}

	rows, err := db.Pool.Query(ctx, query, pqIntArray(systemIDs), startArg, endArg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []TranscriptionExport
	for rows.Next() {
		var t TranscriptionExport
		if err := rows.Scan(
			&t.SystemID, &t.Tgid, &t.CallStartTime,
			&t.Text, &t.Source, &t.IsPrimary, &t.Confidence,
			&t.Language, &t.Model, &t.Provider, &t.WordCount,
			&t.DurationMs, &t.ProviderMs, &t.Words,
		); err != nil {
			return nil, err
		}
		result = append(result, t)
	}
	return result, rows.Err()
}

// UpdateCallTranscriptionStatus updates the transcription_status on a call and its group.
func (db *DB) UpdateCallTranscriptionStatus(ctx context.Context, callID int64, startTime time.Time, status string) error {
	valid := map[string]bool{"none": true, "auto": true, "reviewed": true, "verified": true, "excluded": true, "empty": true}
	if !valid[status] {
		return fmt.Errorf("invalid transcription status: %s", status)
	}

	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	qtx := db.Q.WithTx(tx)

	if err := qtx.UpdateCallTranscriptionStatus(ctx, sqlcdb.UpdateCallTranscriptionStatusParams{
		CallID:              callID,
		StartTime:           pgtype.Timestamptz{Time: startTime, Valid: true},
		TranscriptionStatus: status,
	}); err != nil {
		return err
	}

	if err := qtx.UpdateCallGroupTranscriptionStatus(ctx, sqlcdb.UpdateCallGroupTranscriptionStatusParams{
		CallID:              callID,
		StartTime:           pgtype.Timestamptz{Time: startTime, Valid: true},
		TranscriptionStatus: &status,
	}); err != nil {
		return err
	}

	return tx.Commit(ctx)
}
