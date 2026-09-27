package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// wwwAuthenticate is sent with every 401 (§4.7).
const wwwAuthenticate = `Bearer realm="tr-engine"`

// WriteJSON writes a JSON response with the given status code. A 401 also
// gets the WWW-Authenticate challenge.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", wwwAuthenticate)
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// ErrorCode is a machine-readable error code for programmatic handling.
type ErrorCode = string

const (
	ErrBadRequest       ErrorCode = "bad_request"
	ErrUnauthorized     ErrorCode = "unauthorized"
	ErrForbidden        ErrorCode = "forbidden"
	ErrNotFound         ErrorCode = "not_found"
	ErrConflict         ErrorCode = "conflict"
	ErrRateLimited      ErrorCode = "rate_limited"
	ErrInternalError    ErrorCode = "internal_error"
	ErrServiceUnavail   ErrorCode = "service_unavailable"
	ErrInvalidBody      ErrorCode = "invalid_body"
	ErrInvalidParameter ErrorCode = "invalid_parameter"
	ErrInvalidTimeRange ErrorCode = "invalid_time_range"
	ErrQueryFailed      ErrorCode = "query_failed"
	ErrAmbiguousID      ErrorCode = "ambiguous_id"
	ErrDuplicate        ErrorCode = "duplicate"
	ErrRequestTimeout   ErrorCode = "request_timeout"

	// Auth codes (§4.7). The auth layer emits ErrForbidden only for a matched
	// route without a policy.
	ErrKeyRequired          ErrorCode = "key_required"          // 401: no credential, and anonymous access doesn't allow this
	ErrInvalidKey           ErrorCode = "invalid_key"           // 401: unknown, revoked or expired key
	ErrInvalidTicket        ErrorCode = "invalid_ticket"        // 401: bad, expired or orphaned ticket
	ErrInsufficientScope    ErrorCode = "insufficient_scope"    // 403: the credential lacks the route's scope
	ErrRestrictedCredential ErrorCode = "restricted_credential" // 403: restricted credential on a route that can't enforce it
)

// codeFromStatus returns a default error code for an HTTP status code.
func codeFromStatus(status int) ErrorCode {
	switch status {
	case http.StatusBadRequest:
		return ErrBadRequest
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusForbidden:
		return ErrForbidden
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusConflict:
		return ErrConflict
	case http.StatusTooManyRequests:
		return ErrRateLimited
	case http.StatusInternalServerError:
		return ErrInternalError
	case http.StatusServiceUnavailable:
		return ErrServiceUnavail
	default:
		return ErrInternalError
	}
}

// ErrorResponse is the standard error response body.
type ErrorResponse struct {
	Code   ErrorCode `json:"code"`
	Error  string    `json:"error"`
	Detail string    `json:"detail,omitempty"`
}

// WriteError writes a JSON error response with an auto-derived error code.
func WriteError(w http.ResponseWriter, status int, msg string) {
	WriteJSON(w, status, ErrorResponse{Code: codeFromStatus(status), Error: msg})
}

// WriteErrorDetail writes a JSON error response with detail and auto-derived code.
func WriteErrorDetail(w http.ResponseWriter, status int, msg, detail string) {
	WriteJSON(w, status, ErrorResponse{Code: codeFromStatus(status), Error: msg, Detail: detail})
}

// WriteErrorWithCode writes a JSON error response with a specific error code.
func WriteErrorWithCode(w http.ResponseWriter, status int, code ErrorCode, msg string) {
	WriteJSON(w, status, ErrorResponse{Code: code, Error: msg})
}

// WriteErrorWithCodeDetail writes a JSON error response with a specific code and detail.
func WriteErrorWithCodeDetail(w http.ResponseWriter, status int, code ErrorCode, msg, detail string) {
	WriteJSON(w, status, ErrorResponse{Code: code, Error: msg, Detail: detail})
}

// Pagination holds parsed pagination parameters.
type Pagination struct {
	Limit  int
	Offset int
}

// ParsePagination extracts limit and offset from query params with defaults.
// Returns an error if values are present but invalid.
func ParsePagination(r *http.Request) (Pagination, error) {
	p := Pagination{Limit: 50, Offset: 0}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return p, fmt.Errorf("invalid limit %q: must be an integer", v)
		}
		if n < 1 {
			return p, fmt.Errorf("invalid limit %d: must be >= 1", n)
		}
		if n > 1000 {
			n = 1000
		}
		p.Limit = n
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return p, fmt.Errorf("invalid offset %q: must be an integer", v)
		}
		if n < 0 {
			return p, fmt.Errorf("invalid offset %d: must be >= 0", n)
		}
		p.Offset = n
	}
	return p, nil
}

// SortParam holds a parsed sort parameter.
type SortParam struct {
	Field string
	Desc  bool
}

// ParseSort extracts sort field and direction from query params.
// Returns the default if none specified. Validates against the allowlist.
func ParseSort(r *http.Request, defaultField string, allowed map[string]string) SortParam {
	s := SortParam{Field: defaultField, Desc: false}
	// Normalize: strip direction prefix from default so the fallback is always
	// a clean field name (prevents "-start_time" leaking into SQLColumn lookup).
	if strings.HasPrefix(s.Field, "-") {
		s.Field = s.Field[1:]
		s.Desc = true
	}

	sort := r.URL.Query().Get("sort")
	if sort == "" {
		return s
	}

	if strings.HasPrefix(sort, "-") {
		s.Desc = true
		sort = sort[1:]
	} else if dir := r.URL.Query().Get("sort_dir"); dir == "desc" {
		s.Desc = true
	}

	// Validate against allowlist
	if _, ok := allowed[sort]; ok {
		s.Field = sort
	}

	return s
}

// SQLColumn returns the SQL column for the sort field, using the allowlist mapping.
// If the field is not in the allowlist, falls back to any valid column rather than
// returning potentially unsafe user input.
func (s SortParam) SQLColumn(allowed map[string]string) string {
	if col, ok := allowed[s.Field]; ok {
		return col
	}
	for _, col := range allowed {
		return col
	}
	return "1"
}

// SQLDirection returns "ASC" or "DESC".
func (s SortParam) SQLDirection() string {
	if s.Desc {
		return "DESC"
	}
	return "ASC"
}

// SQLOrderBy returns a full ORDER BY clause like "column DESC".
func (s SortParam) SQLOrderBy(allowed map[string]string) string {
	return s.SQLColumn(allowed) + " " + s.SQLDirection()
}

// QueryInt extracts an integer query parameter. Returns 0, false if missing or invalid.
func QueryInt(r *http.Request, name string) (int, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}


// QueryBool extracts a boolean query parameter.
func QueryBool(r *http.Request, name string) (bool, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return false, false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, false
	}
	return b, true
}

// QueryString extracts a non-empty string query parameter.
func QueryString(r *http.Request, name string) (string, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return "", false
	}
	return v, true
}

// QueryTime extracts a time query parameter (RFC 3339).
func QueryTime(r *http.Request, name string) (time.Time, bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// ValidateTimeRange checks that start_time is not after end_time.
// Returns an error message if invalid, empty string if OK.
// Callers should check after parsing both QueryTime values.
func ValidateTimeRange(startTime, endTime *time.Time) string {
	if startTime != nil && endTime != nil && startTime.After(*endTime) {
		return "start_time must be before end_time"
	}
	return ""
}

// QueryIntList extracts a comma-separated list of ints from a query param.
func QueryIntList(r *http.Request, name string) []int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	var result []int
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if n, err := strconv.Atoi(p); err == nil {
			result = append(result, n)
		}
	}
	return result
}

// QueryInt64List extracts a comma-separated list of int64s from a query param.
func QueryInt64List(r *http.Request, name string) []int64 {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	var result []int64
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if n, err := strconv.ParseInt(p, 10, 64); err == nil {
			result = append(result, n)
		}
	}
	return result
}

// QueryIntListAliased tries each name in order, returning the first non-empty result.
// This lets endpoints accept both singular and plural param names (e.g. tgid and tgids).
func QueryIntListAliased(r *http.Request, names ...string) []int {
	for _, name := range names {
		if result := QueryIntList(r, name); len(result) > 0 {
			return result
		}
	}
	return nil
}

// QueryStringListAliased tries each name in order, returning the first non-empty result.
func QueryStringListAliased(r *http.Request, names ...string) []string {
	for _, name := range names {
		if result := QueryStringList(r, name); len(result) > 0 {
			return result
		}
	}
	return nil
}

// QueryStringList extracts a comma-separated list of strings from a query param.
func QueryStringList(r *http.Request, name string) []string {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// intSliceContains returns true if the slice contains the value.
func intSliceContains(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// stringSliceContains returns true if the slice contains the value.
func stringSliceContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// PathInt extracts an integer from a chi URL parameter.
func PathInt(r *http.Request, name string) (int, error) {
	v := chi.URLParam(r, name)
	if v == "" {
		return 0, fmt.Errorf("missing path parameter: %s", name)
	}
	return strconv.Atoi(v)
}

// PathInt64 extracts an int64 from a chi URL parameter.
func PathInt64(r *http.Request, name string) (int64, error) {
	v := chi.URLParam(r, name)
	if v == "" {
		return 0, fmt.Errorf("missing path parameter: %s", name)
	}
	return strconv.ParseInt(v, 10, 64)
}

// DecodeJSON reads and decodes a JSON request body into v.
func DecodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return fmt.Errorf("missing request body")
	}
	return json.NewDecoder(r.Body).Decode(v)
}
