package service

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/CtrlCarlitos/agent-council/internal/storage"
)

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	OpID    string `json:"op_id,omitempty"`
}

type ErrorEnvelope struct {
	Error ErrorDetail `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message, opID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(ErrorEnvelope{
		Error: ErrorDetail{
			Code:    code,
			Message: message,
			OpID:    opID,
		},
	})
}

// writeControllerAuthError maps storage credential-classification errors to
// their HTTP envelopes (AC-004). It reports whether the error was an
// authority classification (already written) or should be handled by the
// caller's generic branch.
func writeControllerAuthError(w http.ResponseWriter, err error, opID string) bool {
	switch {
	case errors.Is(err, storage.ErrLeaseSuperseded):
		writeError(w, http.StatusForbidden, "lease_superseded", err.Error(), opID)
	case errors.Is(err, storage.ErrAdoptionRequired):
		writeError(w, http.StatusConflict, "adoption_required", err.Error(), opID)
	case errors.Is(err, storage.ErrUnauthorizedOperation):
		writeError(w, http.StatusForbidden, "unauthorized", err.Error(), opID)
	default:
		return false
	}
	return true
}

func decodeStrictJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("unexpected extra data after JSON payload")
	}
	return nil
}
