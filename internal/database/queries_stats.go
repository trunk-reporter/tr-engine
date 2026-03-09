package database

import (
	"context"
	"fmt"
	"time"
)

// StatsResponse contains overall system statistics.
type StatsResponse struct {
	Systems            int              `json:"systems"`
	Talkgroups         int              `json:"talkgroups"`
	Units              int              `json:"units"`
	TotalCalls         int              `json:"total_calls"`
	Calls30d           int              `json:"calls_30d"`
	Calls24h           int              `json:"calls_24h"`
	Calls1h            int              `json:"calls_1h"`
	TotalDurationHours float64          `json:"total_duration_hours"`
	SystemActivity     []SystemActivity `json:"system_activity"`
}

// SystemActivity contains per-system activity breakdown.
type SystemActivity struct {
	SystemID         int    `json:"system_id"`
	SystemName       string `json:"system_name"`
	Sysid            string `json:"sysid"`
	Calls1h          int    `json:"calls_1h"`
	Calls24h         int    `json:"calls_24h"`
	ActiveTalkgroups int    `json:"active_talkgroups"`
	ActiveUnits      int    `json:"active_units"`
}

// GetStats returns overall system statistics.
func (db *DB) GetStats(ctx context.Context) (*StatsResponse, error) {
	stats, err := db.Q.GetOverallStats(ctx)
	if err != nil {
		return nil, err
	}

	s := &StatsResponse{
		Systems:            stats.Systems,
		Talkgroups:         stats.Talkgroups,
		Units:              stats.Units,
		TotalCalls:         stats.TotalCalls,
		Calls30d:           stats.Calls30d,
		Calls24h:           stats.Calls24h,
		Calls1h:            stats.Calls1h,
		TotalDurationHours: stats.TotalDurationHours,
	}

	activities, err := db.Q.GetSystemActivity(ctx)
	if err != nil {
		return nil, err
	}

	s.SystemActivity = make([]SystemActivity, len(activities))
	for i, a := range activities {
		s.SystemActivity[i] = SystemActivity{
			SystemID:         a.SystemID,
			SystemName:       a.SystemName,
			Sysid:            a.Sysid,
			Calls1h:          a.Calls1h,
			Calls24h:         a.Calls24h,
			ActiveTalkgroups: a.ActiveTalkgroups,
			ActiveUnits:      a.ActiveUnits,
		}
	}

	return s, nil
}

// TalkgroupActivityFilter specifies filters for the talkgroup activity summary.
type TalkgroupActivityFilter struct {
	SystemIDs []int
	SiteIDs   []int
	Tgids     []int
	After     *time.Time
	Before    *time.Time
	Limit     int
	Offset    int
	SortField string  // "calls", "duration", "tgid"
	CallState *string // filter by call_state (default: "COMPLETED")
}

// TalkgroupActivity represents call counts grouped by talkgroup.
type TalkgroupActivity struct {
	SystemID       int       `json:"system_id"`
	SystemName     string    `json:"system_name"`
	Tgid           int       `json:"tgid"`
	TgAlphaTag     string    `json:"tg_alpha_tag,omitempty"`
	TgDescription  string    `json:"tg_description,omitempty"`
	TgTag          string    `json:"tg_tag,omitempty"`
	TgGroup        string    `json:"tg_group,omitempty"`
	CallCount      int       `json:"call_count"`
	TotalDuration  float64   `json:"total_duration"`
	EmergencyCount int       `json:"emergency_count"`
	FirstCall      time.Time `json:"first_call"`
	LastCall       time.Time `json:"last_call"`
}

// GetTalkgroupActivity returns call counts grouped by talkgroup for a time range.
// Defaults to COMPLETED calls only; pass call_state to override (empty string = all states).
func (db *DB) GetTalkgroupActivity(ctx context.Context, filter TalkgroupActivityFilter) ([]TalkgroupActivity, int, error) {
	// Default to COMPLETED if not specified
	callState := "COMPLETED"
	if filter.CallState != nil {
		callState = *filter.CallState
	}
	var callStateArg any
	if callState != "" {
		callStateArg = callState
	}

	const whereClause = `
		WHERE ($1::text IS NULL OR c.call_state_type = $1)
		  AND ($2::int[] IS NULL OR c.system_id = ANY($2))
		  AND ($3::int[] IS NULL OR c.site_id = ANY($3))
		  AND ($4::int[] IS NULL OR c.tgid = ANY($4))
		  AND ($5::timestamptz IS NULL OR c.start_time >= $5)
		  AND ($6::timestamptz IS NULL OR c.start_time < $6)`
	args := []any{
		callStateArg,
		pqIntArray(filter.SystemIDs), pqIntArray(filter.SiteIDs), pqIntArray(filter.Tgids),
		filter.After, filter.Before,
	}

	// Count distinct talkgroups
	var total int
	if err := db.Pool.QueryRow(ctx, "SELECT count(DISTINCT (c.system_id, c.tgid)) FROM calls c"+whereClause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	// Sort
	orderBy := "count(*) DESC"
	switch filter.SortField {
	case "duration":
		orderBy = "COALESCE(sum(c.duration), 0) DESC"
	case "tgid":
		orderBy = "c.tgid ASC"
	}

	limit := filter.Limit

	dataQuery := fmt.Sprintf(`
		SELECT c.system_id, COALESCE(c.system_name, ''),
			c.tgid, COALESCE(c.tg_alpha_tag, ''), COALESCE(c.tg_description, ''),
			COALESCE(c.tg_tag, ''), COALESCE(c.tg_group, ''),
			count(*), COALESCE(sum(c.duration), 0),
			count(*) FILTER (WHERE c.emergency),
			min(c.start_time), max(c.start_time)
		FROM calls c
		%s
		GROUP BY c.system_id, COALESCE(c.system_name, ''),
			c.tgid, COALESCE(c.tg_alpha_tag, ''), COALESCE(c.tg_description, ''),
			COALESCE(c.tg_tag, ''), COALESCE(c.tg_group, '')
		ORDER BY %s
		LIMIT $7 OFFSET $8
	`, whereClause, orderBy)

	rows, err := db.Pool.Query(ctx, dataQuery, append(args, limit, filter.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var results []TalkgroupActivity
	for rows.Next() {
		var a TalkgroupActivity
		if err := rows.Scan(
			&a.SystemID, &a.SystemName,
			&a.Tgid, &a.TgAlphaTag, &a.TgDescription,
			&a.TgTag, &a.TgGroup,
			&a.CallCount, &a.TotalDuration,
			&a.EmergencyCount,
			&a.FirstCall, &a.LastCall,
		); err != nil {
			return nil, 0, err
		}
		results = append(results, a)
	}
	if results == nil {
		results = []TalkgroupActivity{}
	}
	return results, total, rows.Err()
}

// CallVolumeFilter specifies filters for the call volume summary.
type CallVolumeFilter struct {
	Interval  string // "hour" or "day"
	Days      int    // lookback days (1–90)
	SystemIDs []int
}

// CallVolumeBucket represents one time bucket of call volume.
type CallVolumeBucket struct {
	Time        time.Time `json:"time"`
	Calls       int       `json:"calls"`
	AvgDuration float64   `json:"avg_duration"`
}

// GetCallVolume returns call counts bucketed by hour or day.
func (db *DB) GetCallVolume(ctx context.Context, f CallVolumeFilter) ([]CallVolumeBucket, error) {
	interval := "hour"
	if f.Interval == "day" {
		interval = "day"
	}
	days := f.Days
	if days < 1 {
		days = 7
	}

	query := fmt.Sprintf(`
		SELECT date_trunc('%s', start_time) AS bucket,
			count(*) AS calls,
			COALESCE(round(avg(duration)::numeric, 1), 0) AS avg_dur
		FROM calls
		WHERE start_time > now() - make_interval(days => $1)
		  AND ($2::int[] IS NULL OR system_id = ANY($2))
		GROUP BY 1 ORDER BY 1
	`, interval)

	rows, err := db.Pool.Query(ctx, query, days, pqIntArray(f.SystemIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []CallVolumeBucket
	for rows.Next() {
		var b CallVolumeBucket
		if err := rows.Scan(&b.Time, &b.Calls, &b.AvgDuration); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	if buckets == nil {
		buckets = []CallVolumeBucket{}
	}
	return buckets, rows.Err()
}

// DailyOverviewFilter specifies filters for the daily overview.
type DailyOverviewFilter struct {
	Days      int // lookback days (1–90)
	SystemIDs []int
}

// DailyOverviewRow represents one day of the daily overview.
type DailyOverviewRow struct {
	Date             string  `json:"date"`
	Calls            int     `json:"calls"`
	TotalHours       float64 `json:"total_hours"`
	ActiveTalkgroups int     `json:"active_talkgroups"`
}

// GetDailyOverview returns daily call aggregates.
func (db *DB) GetDailyOverview(ctx context.Context, f DailyOverviewFilter) ([]DailyOverviewRow, error) {
	days := f.Days
	if days < 1 {
		days = 14
	}

	query := `
		SELECT date_trunc('day', start_time)::date AS day,
			count(*) AS calls,
			COALESCE(round(sum(duration)::numeric / 3600, 1), 0) AS total_hours,
			count(DISTINCT tgid) AS active_tgs
		FROM calls
		WHERE start_time > now() - make_interval(days => $1)
		  AND ($2::int[] IS NULL OR system_id = ANY($2))
		GROUP BY 1 ORDER BY 1`

	rows, err := db.Pool.Query(ctx, query, days, pqIntArray(f.SystemIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []DailyOverviewRow
	for rows.Next() {
		var r DailyOverviewRow
		var d time.Time
		if err := rows.Scan(&d, &r.Calls, &r.TotalHours, &r.ActiveTalkgroups); err != nil {
			return nil, err
		}
		r.Date = d.Format("2006-01-02")
		result = append(result, r)
	}
	if result == nil {
		result = []DailyOverviewRow{}
	}
	return result, rows.Err()
}

// CategoryBreakdownFilter specifies filters for the category breakdown.
type CategoryBreakdownFilter struct {
	Hours     int // lookback hours (1–720)
	Limit     int // max categories (1–100)
	SystemIDs []int
}

// CategoryBreakdownRow represents one tag category.
type CategoryBreakdownRow struct {
	Tag   string  `json:"tag"`
	Calls int     `json:"calls"`
	Hours float64 `json:"hours"`
}

// GetCategoryBreakdown returns calls grouped by tg_tag.
func (db *DB) GetCategoryBreakdown(ctx context.Context, f CategoryBreakdownFilter) ([]CategoryBreakdownRow, error) {
	hours := f.Hours
	if hours < 1 {
		hours = 24
	}
	limit := f.Limit
	if limit < 1 {
		limit = 10
	}

	query := `
		SELECT COALESCE(NULLIF(tg_tag, ''), 'Unknown') AS tag,
			count(*) AS calls,
			COALESCE(round(sum(duration)::numeric / 3600, 1), 0) AS hours
		FROM calls
		WHERE start_time > now() - make_interval(hours => $1)
		  AND ($2::int[] IS NULL OR system_id = ANY($2))
		GROUP BY 1 ORDER BY 2 DESC
		LIMIT $3`

	rows, err := db.Pool.Query(ctx, query, hours, pqIntArray(f.SystemIDs), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []CategoryBreakdownRow
	for rows.Next() {
		var r CategoryBreakdownRow
		if err := rows.Scan(&r.Tag, &r.Calls, &r.Hours); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	if result == nil {
		result = []CategoryBreakdownRow{}
	}
	return result, rows.Err()
}

// CallHeatmapFilter specifies filters for the call heatmap.
type CallHeatmapFilter struct {
	Days      int    // lookback days (1–90)
	Timezone  string // IANA timezone name
	SystemIDs []int
}

// CallHeatmapCell represents one dow×hour cell.
type CallHeatmapCell struct {
	DOW  int `json:"dow"`
	Hour int `json:"hour"`
	Call int `json:"calls"`
}

// GetCallHeatmap returns day-of-week × hour-of-day call counts.
func (db *DB) GetCallHeatmap(ctx context.Context, f CallHeatmapFilter) ([]CallHeatmapCell, error) {
	days := f.Days
	if days < 1 {
		days = 7
	}
	tz := f.Timezone
	if tz == "" {
		tz = "UTC"
	}

	query := `
		SELECT extract(dow FROM start_time AT TIME ZONE $1)::int AS dow,
			extract(hour FROM start_time AT TIME ZONE $1)::int AS hour,
			count(*) AS calls
		FROM calls
		WHERE start_time > now() - make_interval(days => $2)
		  AND ($3::int[] IS NULL OR system_id = ANY($3))
		GROUP BY 1, 2 ORDER BY 1, 2`

	rows, err := db.Pool.Query(ctx, query, tz, days, pqIntArray(f.SystemIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cells []CallHeatmapCell
	for rows.Next() {
		var c CallHeatmapCell
		if err := rows.Scan(&c.DOW, &c.Hour, &c.Call); err != nil {
			return nil, err
		}
		cells = append(cells, c)
	}
	if cells == nil {
		cells = []CallHeatmapCell{}
	}
	return cells, rows.Err()
}

// DecodeRateFilter specifies time range for decode rate queries.
type DecodeRateFilter struct {
	SystemIDs []int
	StartTime *time.Time
	EndTime   *time.Time
	Limit     int
}

// DecodeRateAPI represents a decode rate for API responses.
type DecodeRateAPI struct {
	Time              time.Time `json:"time"`
	SystemID          *int      `json:"system_id,omitempty"`
	SystemName        string    `json:"system_name,omitempty"`
	Sysid             string    `json:"sysid,omitempty"`
	DecodeRate        float32   `json:"decode_rate"`
	DecodeRateInterval float32  `json:"decode_rate_interval"`
	ControlChannel    int64     `json:"control_channel"`
}

// GetDecodeRates returns decode rate measurements.
func (db *DB) GetDecodeRates(ctx context.Context, filter DecodeRateFilter) ([]DecodeRateAPI, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 5000
	}

	query := `
		SELECT d.time, d.system_id, COALESCE(s.name, ''), COALESCE(s.sysid, ''),
			d.decode_rate, d.decode_rate_interval, d.control_channel
		FROM decode_rates d
		LEFT JOIN systems s ON s.system_id = d.system_id
		WHERE ($1::timestamptz IS NULL OR d.time >= $1)
		  AND ($2::timestamptz IS NULL OR d.time <= $2)
		  AND ($3::int[] IS NULL OR d.system_id = ANY($3))
		ORDER BY d.time DESC LIMIT $4`

	var systemIDs []int
	if len(filter.SystemIDs) > 0 {
		systemIDs = filter.SystemIDs
	}

	rows, err := db.Pool.Query(ctx, query, filter.StartTime, filter.EndTime, systemIDs, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rates []DecodeRateAPI
	for rows.Next() {
		var r DecodeRateAPI
		if err := rows.Scan(&r.Time, &r.SystemID, &r.SystemName, &r.Sysid,
			&r.DecodeRate, &r.DecodeRateInterval, &r.ControlChannel); err != nil {
			return nil, err
		}
		rates = append(rates, r)
	}
	if rates == nil {
		rates = []DecodeRateAPI{}
	}
	return rates, rows.Err()
}

// RecorderUtilizationFilter specifies filters for recorder utilization bucketing.
type RecorderUtilizationFilter struct {
	Bucket     string // "hour" or "day"
	Days       int    // lookback days (1–90)
	SrcNum     *int
	InstanceID *string
}

// RecorderUtilizationBucket represents one time bucket of recorder utilization.
type RecorderUtilizationBucket struct {
	Time         time.Time `json:"time"`
	SrcNum       int       `json:"src_num"`
	PctRecording float64   `json:"pct_recording"`
	PctIdle      float64   `json:"pct_idle"`
	PctAvailable float64   `json:"pct_available"`
	PctIgnore    float64   `json:"pct_ignore"`
	SampleCount  int       `json:"sample_count"`
}

// GetRecorderUtilization returns recorder utilization bucketed by hour or day.
func (db *DB) GetRecorderUtilization(ctx context.Context, f RecorderUtilizationFilter) ([]RecorderUtilizationBucket, error) {
	bucket := "hour"
	if f.Bucket == "day" {
		bucket = "day"
	}
	days := f.Days
	if days < 1 {
		days = 7
	}

	query := fmt.Sprintf(`
		SELECT date_trunc('%s', "time") AS bucket,
			src_num,
			ROUND(AVG(CASE WHEN rec_state_type = 'RECORDING' THEN 1.0 ELSE 0.0 END) * 100, 1) AS pct_recording,
			ROUND(AVG(CASE WHEN rec_state_type = 'IDLE'      THEN 1.0 ELSE 0.0 END) * 100, 1) AS pct_idle,
			ROUND(AVG(CASE WHEN rec_state_type = 'AVAILABLE' THEN 1.0 ELSE 0.0 END) * 100, 1) AS pct_available,
			ROUND(AVG(CASE WHEN rec_state_type = 'IGNORE'    THEN 1.0 ELSE 0.0 END) * 100, 1) AS pct_ignore,
			COUNT(*) AS sample_count
		FROM recorder_snapshots
		WHERE "time" > now() - make_interval(days => $1)
		  AND ($2::smallint IS NULL OR src_num = $2)
		  AND ($3::text IS NULL OR instance_id = $3)
		GROUP BY 1, src_num
		ORDER BY 1, src_num
	`, bucket)

	rows, err := db.Pool.Query(ctx, query, days, f.SrcNum, f.InstanceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []RecorderUtilizationBucket
	for rows.Next() {
		var b RecorderUtilizationBucket
		if err := rows.Scan(&b.Time, &b.SrcNum, &b.PctRecording, &b.PctIdle,
			&b.PctAvailable, &b.PctIgnore, &b.SampleCount); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	if buckets == nil {
		buckets = []RecorderUtilizationBucket{}
	}
	return buckets, rows.Err()
}

// DecodeRateBucketFilter specifies filters for bucketed decode rate aggregation.
type DecodeRateBucketFilter struct {
	Bucket    string // "hour" or "day"
	Days      int    // lookback days (1–90)
	SystemIDs []int
}

// DecodeRateBucket represents one time bucket of decode rate aggregation.
type DecodeRateBucket struct {
	Time       time.Time `json:"time"`
	SystemID   int       `json:"system_id"`
	SystemName string    `json:"system_name"`
	AvgRate    float64   `json:"avg_rate"`
	MinRate    float64   `json:"min_rate"`
	MaxRate    float64   `json:"max_rate"`
	Samples    int       `json:"sample_count"`
}

// GetDecodeRateBuckets returns decode rates bucketed by hour or day.
func (db *DB) GetDecodeRateBuckets(ctx context.Context, f DecodeRateBucketFilter) ([]DecodeRateBucket, error) {
	bucket := "hour"
	if f.Bucket == "day" {
		bucket = "day"
	}
	days := f.Days
	if days < 1 {
		days = 7
	}

	query := fmt.Sprintf(`
		SELECT date_trunc('%s', d."time") AS bucket,
			d.system_id, COALESCE(s.name, d.sys_name, ''),
			ROUND(AVG(d.decode_rate)::numeric, 1),
			ROUND(MIN(d.decode_rate)::numeric, 1),
			ROUND(MAX(d.decode_rate)::numeric, 1),
			COUNT(*)
		FROM decode_rates d
		LEFT JOIN systems s ON s.system_id = d.system_id
		WHERE d."time" > now() - make_interval(days => $1)
		  AND ($2::int[] IS NULL OR d.system_id = ANY($2))
		GROUP BY 1, d.system_id, COALESCE(s.name, d.sys_name, '')
		ORDER BY 1, d.system_id
	`, bucket)

	rows, err := db.Pool.Query(ctx, query, days, pqIntArray(f.SystemIDs))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []DecodeRateBucket
	for rows.Next() {
		var b DecodeRateBucket
		if err := rows.Scan(&b.Time, &b.SystemID, &b.SystemName,
			&b.AvgRate, &b.MinRate, &b.MaxRate, &b.Samples); err != nil {
			return nil, err
		}
		buckets = append(buckets, b)
	}
	if buckets == nil {
		buckets = []DecodeRateBucket{}
	}
	return buckets, rows.Err()
}
