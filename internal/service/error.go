package service

import (
	"encoding/json"
	"net/http"
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
