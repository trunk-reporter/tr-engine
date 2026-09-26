package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// Helpers for the handlers of Restricted = Enforced routes (§6.3, §7.2). The
// database functions behind those routes take the request's principal and
// apply its restrictions themselves; these helpers answer single resources
// outside the restriction exactly like missing ones, so a restricted caller
// can't learn that they exist.

// writeLookupError answers a failed single-resource lookup: 404 with
// notFound when the resource doesn't exist or is outside the caller's
// restriction (the two look the same, §4.7), otherwise 500 with failed.
func writeLookupError(w http.ResponseWriter, err error, notFound, failed string) {
	if errors.Is(err, pgx.ErrNoRows) {
		WriteError(w, http.StatusNotFound, notFound)
		return
	}
	WriteError(w, http.StatusInternalServerError, failed)
}

// callAccessChecker is the part of *database.DB that requireCallAccess needs.
type callAccessChecker interface {
	GetCallAccess(ctx context.Context, callID int64) (systemID, tgid int, err error)
}

// requireCallAccess is the first step of every route that serves one call's
// data (§7.2): it looks up the call's system and talkgroup and answers 404
// with notFound when the call doesn't exist or the caller's restriction
// doesn't allow its talkgroup. It reports whether the handler may go on.
func requireCallAccess(w http.ResponseWriter, r *http.Request, db callAccessChecker, callID int64, notFound string) bool {
	systemID, tgid, err := db.GetCallAccess(r.Context(), callID)
	if err == nil && !PrincipalFrom(r).AllowsTG(systemID, tgid) {
		err = pgx.ErrNoRows
	}
	if err != nil {
		writeLookupError(w, err, notFound, "failed to look up call")
		return false
	}
	return true
}
