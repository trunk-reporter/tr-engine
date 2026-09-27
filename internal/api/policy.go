package api

import "github.com/snarg/tr-engine/internal/auth"

// Mode says what a route does with a restricted principal (§6.1).
type Mode int

const (
	// Deny: restricted principals get 403 restricted_credential. It is the
	// zero value, so a policy that doesn't say otherwise fails closed.
	Deny Mode = iota
	// Enforced: the handler applies the principal's restrictions to
	// everything it returns.
	Enforced
)

func (m Mode) String() string {
	if m == Enforced {
		return "enforced"
	}
	return "deny"
}

// RoutePolicy is who may call one route (§6.1).
type RoutePolicy struct {
	Scope       auth.Scope // "" = public; otherwise listen, edit, admin or upload
	Restricted  Mode       // Deny or Enforced
	Ticket      bool       // ?ticket= is honoured (GET/HEAD only)
	KeyRequired bool       // anonymous principals never pass, whatever the anonymous policy says
	FormKey     bool       // upload: the key may come from multipart fields; checked by the upload middleware
}

// apiPrefix is where the REST API lives.
const apiPrefix = "/api/v1"

var (
	publicRoute = RoutePolicy{}
	listenRoute = RoutePolicy{Scope: auth.ScopeListen} // Restricted: Deny
	editRoute   = RoutePolicy{Scope: auth.ScopeEdit}
	adminRoute  = RoutePolicy{Scope: auth.ScopeAdmin}
	// ticketRoute is an enforced listen route that also honours ?ticket=.
	ticketRoute = RoutePolicy{Scope: auth.ScopeListen, Restricted: Enforced, Ticket: true}
	// enforcedRoute is a listen route whose handler applies restrictions.
	enforcedRoute = RoutePolicy{Scope: auth.ScopeListen, Restricted: Enforced}
)

// routePolicies maps every route, as "METHOD /full/pattern" exactly as
// chi.Mux.Find returns it, to its policy (§6.3). A matched route that is not
// in this table is refused with 403 (Match); TestRoutePolicyTable checks the
// table against the router in both directions.
//
// HEAD requests are served by the GET handler (middleware.GetHead) and use the
// GET entry.
//
// A listen route is Enforced only when its handler applies the principal's
// restrictions (§6.3, §7); otherwise it is Deny, and restricted principals
// get 403 restricted_credential.
var routePolicies = map[string]RoutePolicy{
	// Public: no credential needed; the anonymous policy doesn't matter.
	"GET /api/v1/health":       publicRoute,
	"GET /api/v1/whoami":       publicRoute,
	"GET /api/v1/openapi.yaml": publicRoute,
	"GET /api/v1/pages":        publicRoute,
	"GET /favicon.ico":         publicRoute,
	"GET /*":                   publicRoute,

	// listen, Restricted: Enforced (§6.3). The REST handlers pass the
	// principal to the query functions, which apply its restrictions (§7.2),
	// and single resources outside them are 404 (restriction_db_test.go
	// covers each). POST /tickets signs only the requested narrowing; the
	// key's current restriction is applied each time the ticket is verified.
	// The SSE stream and the live-audio WebSocket keep the principal on
	// their subscriber, check every event (SSEEventAllowed) and frame
	// (audio.PrincipalAllows) against it, and re-check it while open (§7.3
	// to §7.5; stream_db_test.go).
	"GET /api/v1/systems":                   enforcedRoute,
	"GET /api/v1/systems/{id}":              enforcedRoute,
	"GET /api/v1/sites/{id}":                enforcedRoute,
	"GET /api/v1/talkgroups":                enforcedRoute,
	"GET /api/v1/talkgroups/{id}":           enforcedRoute,
	"GET /api/v1/talkgroups/{id}/calls":     enforcedRoute,
	"GET /api/v1/talkgroup-directory":       enforcedRoute,
	"GET /api/v1/calls":                     enforcedRoute,
	"GET /api/v1/calls/active":              enforcedRoute,
	"GET /api/v1/calls/{id}":                enforcedRoute,
	"GET /api/v1/calls/{id}/audio":          {Scope: auth.ScopeListen, Restricted: Enforced, Ticket: true},
	"GET /api/v1/calls/{id}/frequencies":    enforcedRoute,
	"GET /api/v1/calls/{id}/transmissions":  enforcedRoute,
	"GET /api/v1/calls/{id}/transcription":  enforcedRoute,
	"GET /api/v1/calls/{id}/transcriptions": enforcedRoute,
	"GET /api/v1/call-groups":               enforcedRoute,
	"GET /api/v1/call-groups/{id}":          enforcedRoute,
	"GET /api/v1/transcriptions/search":     enforcedRoute,
	"GET /api/v1/transcriptions/batch":      enforcedRoute,
	"GET /api/v1/events/stream":             ticketRoute,
	"GET /api/v1/audio/live":                ticketRoute,
	"POST /api/v1/tickets":                  {Scope: auth.ScopeListen, Restricted: Enforced, KeyRequired: true},

	// listen, Restricted: Deny (§6.3): no talkgroup dimension, or aggregates
	// that leak other talkgroups' activity.
	"GET /api/v1/p25-systems":                    listenRoute,
	"GET /api/v1/talkgroups/encryption-stats":    listenRoute,
	"GET /api/v1/talkgroups/{id}/units":          listenRoute,
	"GET /api/v1/units":                          listenRoute,
	"GET /api/v1/units/{id}":                     listenRoute,
	"GET /api/v1/units/{id}/calls":               listenRoute,
	"GET /api/v1/units/{id}/events":              listenRoute,
	"GET /api/v1/unit-events":                    listenRoute,
	"GET /api/v1/unit-affiliations":              listenRoute,
	"GET /api/v1/unit-tag-suggestions":           listenRoute,
	"GET /api/v1/unit-tag-suggestions/{id}":      listenRoute,
	"GET /api/v1/stats":                          listenRoute,
	"GET /api/v1/stats/rates":                    listenRoute,
	"GET /api/v1/stats/talkgroup-activity":       listenRoute,
	"GET /api/v1/stats/call-volume":              listenRoute,
	"GET /api/v1/stats/daily-overview":           listenRoute,
	"GET /api/v1/stats/category-breakdown":       listenRoute,
	"GET /api/v1/stats/call-heatmap":             listenRoute,
	"GET /api/v1/analytics/recorder-utilization": listenRoute,
	"GET /api/v1/analytics/decode-rates":         listenRoute,
	"GET /api/v1/trunking-messages":              listenRoute,
	"GET /api/v1/recorders":                      listenRoute,
	"GET /api/v1/transcriptions/queue":           listenRoute,
	"GET /api/v1/audio/jitter":                   listenRoute,
	"GET /metrics":                               {Scope: auth.ScopeListen, KeyRequired: true},

	// edit: radio metadata.
	"PATCH /api/v1/talkgroups/{id}":                  editRoute,
	"PATCH /api/v1/units/{id}":                       editRoute,
	"PUT /api/v1/calls/{id}/transcription":           editRoute,
	"POST /api/v1/calls/{id}/transcribe":             editRoute,
	"POST /api/v1/calls/{id}/transcription/verify":   editRoute,
	"POST /api/v1/calls/{id}/transcription/reject":   editRoute,
	"POST /api/v1/calls/{id}/transcription/exclude":  editRoute,
	"POST /api/v1/unit-tag-suggestions/{id}/approve": editRoute,
	"POST /api/v1/unit-tag-suggestions/{id}/dismiss": editRoute,

	// admin: system and data management.
	"GET /api/v1/console-messages":            adminRoute,
	"PATCH /api/v1/systems/{id}":              adminRoute,
	"PATCH /api/v1/sites/{id}":                adminRoute,
	"POST /api/v1/talkgroup-directory/import": adminRoute,
	"POST /api/v1/unit-tags/import":           adminRoute,
	"POST /api/v1/admin/systems/merge":        adminRoute,
	// admin: maintenance.
	"GET /api/v1/admin/maintenance":                 adminRoute,
	"POST /api/v1/admin/maintenance":                adminRoute,
	"PUT /api/v1/admin/maintenance/config":          adminRoute,
	"DELETE /api/v1/admin/maintenance/config/{key}": adminRoute,
	// admin: transcription backfill.
	"POST /api/v1/admin/transcribe-backfill":        adminRoute,
	"GET /api/v1/admin/transcribe-backfill":         adminRoute,
	"DELETE /api/v1/admin/transcribe-backfill":      adminRoute,
	"DELETE /api/v1/admin/transcribe-backfill/{id}": adminRoute,
	// admin: storage.
	"GET /api/v1/admin/storage/stats":          adminRoute,
	"POST /api/v1/admin/storage/purge/{table}": adminRoute,
	// admin: other.
	"POST /api/v1/query":        adminRoute,
	"POST /api/v1/pages":        adminRoute,
	"POST /api/v1/debug-report": adminRoute,
	// admin: keys, anonymous access, audit log.
	"GET /api/v1/keys":             adminRoute,
	"POST /api/v1/keys":            adminRoute,
	"GET /api/v1/keys/{id}":        adminRoute,
	"PATCH /api/v1/keys/{id}":      adminRoute,
	"DELETE /api/v1/keys/{id}":     adminRoute,
	"GET /api/v1/anonymous-access": adminRoute,
	"PUT /api/v1/anonymous-access": adminRoute,
	"GET /api/v1/admin/audit-log":  adminRoute,

	// upload: the key may also come from the multipart fields key/api_key.
	"POST /api/v1/call-upload": {Scope: auth.ScopeUpload, FormKey: true},
}

// Routes the audit log never records although they change nothing worth
// auditing and are high-volume (§9).
var auditSkipRoutes = map[string]bool{
	"POST /api/v1/call-upload": true,
	"POST /api/v1/tickets":     true,
}

// privateCacheRoutes answer with Cache-Control: private instead of no-store
// (§4): call audio may be cached by the browser that fetched it.
var privateCacheRoutes = map[string]bool{
	"GET /api/v1/calls/{id}/audio": true,
}

// EventRestriction says what an SSE event type does for a restricted
// principal (§7.3).
type EventRestriction int

const (
	// EventDropRestricted: restricted principals never get the event. It is
	// the zero value, so unclassified types fail closed.
	EventDropRestricted EventRestriction = iota
	// EventByTalkgroup: restricted principals get the event only when it
	// carries a non-zero system and talkgroup that every restriction allows.
	EventByTalkgroup
)

// EventPolicy is the minimum scope for one SSE event type and what it does
// for restricted principals.
type EventPolicy struct {
	Scope      auth.Scope
	Restricted EventRestriction
}

// sseEventPolicies classifies every SSE event type (§7.3). It applies to
// every principal, before the client's own filters. A type that is not listed
// needs admin and is dropped for restricted principals until it is classified
// here and in the SSEEventType description in openapi.yaml.
var sseEventPolicies = map[string]EventPolicy{
	"console":          {Scope: auth.ScopeAdmin, Restricted: EventDropRestricted},
	"call_start":       {Scope: auth.ScopeListen, Restricted: EventByTalkgroup},
	"call_update":      {Scope: auth.ScopeListen, Restricted: EventByTalkgroup},
	"call_end":         {Scope: auth.ScopeListen, Restricted: EventByTalkgroup},
	"transcription":    {Scope: auth.ScopeListen, Restricted: EventByTalkgroup},
	"unit_event":       {Scope: auth.ScopeListen, Restricted: EventByTalkgroup},
	"recorder_update":  {Scope: auth.ScopeListen, Restricted: EventDropRestricted},
	"rate_update":      {Scope: auth.ScopeListen, Restricted: EventDropRestricted},
	"trunking_message": {Scope: auth.ScopeListen, Restricted: EventDropRestricted},
}

// unclassifiedEventPolicy applies to event types missing from sseEventPolicies.
var unclassifiedEventPolicy = EventPolicy{Scope: auth.ScopeAdmin, Restricted: EventDropRestricted}

// SSEEventPolicy returns the policy for an SSE event type.
func SSEEventPolicy(eventType string) EventPolicy {
	if p, ok := sseEventPolicies[eventType]; ok {
		return p
	}
	return unclassifiedEventPolicy
}

// SSEEventAllowed reports whether principal p may receive an event of the
// given type about (systemID, tgid), before any client filter: p must hold the
// type's scope, and a restricted p gets only EventByTalkgroup types whose
// non-zero system and talkgroup every restriction allows. A nil p gets
// nothing.
func SSEEventAllowed(p *auth.Principal, eventType string, systemID, tgid int) bool {
	pol := SSEEventPolicy(eventType)
	if !p.Has(pol.Scope) {
		return false
	}
	if !p.Restricted() {
		return true
	}
	return pol.Restricted == EventByTalkgroup && systemID != 0 && tgid != 0 && p.AllowsTG(systemID, tgid)
}
