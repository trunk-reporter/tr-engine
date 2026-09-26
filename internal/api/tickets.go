package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/hlog"
	"github.com/snarg/tr-engine/internal/auth"
)

// TicketsHandler mints stream tickets (§3.5).
type TicketsHandler struct {
	authn *authenticator
}

func NewTicketsHandler(authn *authenticator) *TicketsHandler {
	return &TicketsHandler{authn: authn}
}

func (h *TicketsHandler) Routes(r chi.Router) {
	r.Post("/tickets", h.Mint)
}

type ticketResponse struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Mint issues a short-lived, listen-only ticket for the calling key, for
// ?ticket= on the stream and audio routes. The body is optional:
// {"ttl_seconds": 600, "restriction": {...}}. The ticket carries only the
// requested narrowing; the key's own restriction is applied each time the
// ticket is verified, so key changes reach outstanding tickets.
func (h *TicketsHandler) Mint(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r)
	// The route is KeyRequired with scope listen; this only guards against
	// the handler being reached some other way.
	if p == nil || p.Kind != auth.KindKey || !p.Has(auth.ScopeListen) {
		WriteErrorWithCode(w, http.StatusForbidden, ErrInsufficientScope, "this operation needs the listen scope")
		return
	}

	var req struct {
		TTLSeconds  *int64            `json:"ttl_seconds"`
		Restriction *auth.Restriction `json:"restriction"`
	}
	if r.Body != nil {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "invalid request body: "+err.Error())
			return
		}
	}
	ttl := auth.TicketDefaultTTL
	if req.TTLSeconds != nil {
		// Clamp the number before converting, so a huge value can't overflow.
		secs := min(max(*req.TTLSeconds, int64(auth.TicketMinTTL/time.Second)), int64(auth.TicketMaxTTL/time.Second))
		ttl = auth.ClampTicketTTL(time.Duration(secs) * time.Second)
	}

	// Validate the narrowing before anything else looks at it: it is
	// bounded (MaxTicketEntries) only from here on.
	if req.Restriction != nil {
		if err := req.Restriction.Validate(auth.KindTicket); err != nil {
			writeTicketNarrowingError(w, err)
			return
		}
	}

	secret, err := h.authn.ticketSecret(r.Context())
	if err != nil {
		hlog.FromRequest(r).Error().Err(err).Msg("tickets: reading the ticket secret failed")
		WriteErrorWithCode(w, http.StatusServiceUnavailable, ErrServiceUnavail, "ticket signing is unavailable; try again")
		return
	}
	exp := time.Unix(h.authn.now().Add(ttl).Unix(), 0).UTC()
	ticket, err := auth.SignTicket(secret, auth.TicketPayload{KeyID: p.KeyID, ExpiresAt: exp, Narrowing: req.Restriction})
	if err != nil {
		writeTicketNarrowingError(w, err)
		return
	}

	if req.Restriction != nil {
		// A narrowing naming a merged-away system would be refused on every
		// use (resolveTicket); say so now, while the client can fix it.
		merged, err := h.authn.mergedAway(r.Context(), req.Restriction.ReferencedSystems())
		if err != nil {
			hlog.FromRequest(r).Error().Err(err).Msg("tickets: checking the narrowing for merged systems failed")
			WriteErrorWithCode(w, http.StatusServiceUnavailable, ErrServiceUnavail, "ticket signing is unavailable; try again")
			return
		}
		if merged {
			WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody,
				"restriction: names a system that was merged into another; use the system it was merged into")
			return
		}
	}
	WriteJSON(w, http.StatusOK, ticketResponse{Ticket: ticket, ExpiresAt: exp})
}

// writeTicketNarrowingError answers 400 for a narrowing that Validate or
// SignTicket refused.
func writeTicketNarrowingError(w http.ResponseWriter, err error) {
	if errors.Is(err, auth.ErrTicketTooLarge) {
		WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, auth.ErrTicketTooLarge.Error())
		return
	}
	WriteErrorWithCode(w, http.StatusBadRequest, ErrInvalidBody, "restriction: "+err.Error())
}
