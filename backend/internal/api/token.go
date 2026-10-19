package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/owncloud/ocis-agent/backend/internal/auth"
	"github.com/owncloud/ocis-agent/backend/internal/ocisclient"
	"github.com/owncloud/ocis-agent/backend/internal/store"
)

// The enrolment and token endpoints. The caller's raw bearer is handed to
// auth only for the actions of the user that need it (enrol, unenroll,
// revoke, revoke-all and its preview); it is never stored, logged or
// echoed. Renewal uses the agent's own app token inside auth.

// caller returns the principal and the validated bearer of the request.
func caller(r *http.Request) (Principal, ocisclient.Secret, error) {
	p, ok := principal(r.Context())
	if !ok {
		return Principal{}, "", errNoPrincipal
	}
	b, ok := bearerFrom(r.Context())
	if !ok || b == "" {
		return Principal{}, "", errNoPrincipal
	}
	return p, b, nil
}

// strictBool reads an optional JSON boolean decoded as a RawMessage: absent
// is (false, false); only the literals true and false are accepted (null, a
// string or a number is errBadJSON).
func strictBool(raw json.RawMessage) (value, present bool, err error) {
	switch string(raw) {
	case "":
		return false, false, nil
	case "true":
		return true, true, nil
	case "false":
		return false, true, nil
	}
	return false, false, errBadJSON
}

// enroll is POST /agent-api/v1/enroll with the body {} (nothing else).
// 201 for a new enrolment, 200 for a re-enrolment (the token is replaced),
// both with the status body.
func (s *Server) enroll(w http.ResponseWriter, r *http.Request) error {
	var body struct{}
	if err := decodeBody(w, r, &body); err != nil {
		return err
	}
	p, bearer, err := caller(r)
	if err != nil {
		return err
	}
	existed := false
	if err := s.d.Store.View(r.Context(), func(tx store.Tx) error {
		_, err := tx.GetUser(p.UserID)
		switch {
		case err == nil:
			existed = true
		case !errors.Is(err, store.ErrNotFound):
			return err
		}
		return nil
	}); err != nil {
		return err
	}
	id := auth.Identity{UserID: p.UserID, Username: p.Username, DisplayName: p.DisplayName, DriveID: p.DriveID}
	if err := s.d.Auth.Enroll(r.Context(), id, bearer); err != nil {
		return err
	}
	v, err := s.status(r.Context(), p)
	if err != nil {
		return err
	}
	code := http.StatusCreated
	if existed {
		code = http.StatusOK
	}
	writeJSON(w, code, v)
	return nil
}

type unenrollBody struct {
	DeleteMemory json.RawMessage `json:"delete_memory"`
}

// unenroll is POST /agent-api/v1/unenroll with {"delete_memory": bool}
// (optional, default false). 204; 404 when not enrolled.
func (s *Server) unenroll(w http.ResponseWriter, r *http.Request) error {
	var body unenrollBody
	if err := decodeBody(w, r, &body); err != nil {
		return err
	}
	del, _, err := strictBool(body.DeleteMemory)
	if err != nil {
		return err
	}
	p, bearer, err := caller(r)
	if err != nil {
		return err
	}
	if err := s.d.Auth.Unenroll(r.Context(), p.UserID, bearer, del); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// revoke is DELETE /agent-api/v1/token: delete the agent's tokens (ours
// only), pause the agent with credential_revoked.
func (s *Server) revoke(w http.ResponseWriter, r *http.Request) error {
	p, bearer, err := caller(r)
	if err != nil {
		return err
	}
	n, err := s.d.Auth.Revoke(r.Context(), p.UserID, bearer)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, revokedView{Revoked: n})
	return nil
}

// previewRevokeAll is GET /agent-api/v1/token/revoke-all/preview: all of the
// caller's app tokens (labels and dates) for the confirmation dialog.
func (s *Server) previewRevokeAll(w http.ResponseWriter, r *http.Request) error {
	_, bearer, err := caller(r)
	if err != nil {
		return err
	}
	list, err := s.d.Auth.PreviewRevokeAll(r.Context(), bearer)
	if err != nil {
		return err
	}
	v := previewView{Tokens: make([]tokenSummaryView, 0, len(list))}
	for _, t := range list {
		v.Tokens = append(v.Tokens, tokenSummaryView{
			Label: t.Label, CreatedAt: timeView(t.CreatedAt), ExpiresAt: timeView(t.ExpiresAt), Ours: t.Ours,
		})
	}
	writeJSON(w, http.StatusOK, v)
	return nil
}

type revokeAllBody struct {
	Confirm json.RawMessage `json:"confirm"`
}

// revokeAll is POST /agent-api/v1/token/revoke-all with exactly
// {"confirm": true}: the incident revoke of ALL the user's app tokens.
// Anything else is 400 and nothing is sent to oCIS.
func (s *Server) revokeAll(w http.ResponseWriter, r *http.Request) error {
	var body revokeAllBody
	if err := decodeBody(w, r, &body); err != nil {
		return err
	}
	if confirm, _, err := strictBool(body.Confirm); err != nil || !confirm {
		return errBadJSON
	}
	p, bearer, err := caller(r)
	if err != nil {
		return err
	}
	n, err := s.d.Auth.RevokeAll(r.Context(), p.UserID, bearer, true)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, revokedView{Revoked: n})
	return nil
}

// renew is POST /agent-api/v1/token/renew without a body: replace the
// agent's app token now (with the app token itself, inside auth).
// credentials_invalid (409) means the user has to enrol again.
func (s *Server) renew(w http.ResponseWriter, r *http.Request) error {
	if err := requireEmptyBody(r); err != nil {
		return err
	}
	p, ok := principal(r.Context())
	if !ok {
		return errNoPrincipal
	}
	res, err := s.d.Auth.Renew(r.Context(), p.UserID)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, renewedView{Renewed: true, OldCleaned: res.OldCleaned})
	return nil
}
