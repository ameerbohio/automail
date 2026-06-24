package handlers

import (
	"encoding/json"
	"net/http"
)

// errorBody matches plans/09-api-contracts.md's error format:
// { "error": "human-readable message", "code": "MACHINE_READABLE_CODE" }
type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// WriteError and WriteJSON are exported so package main (middleware.go)
// can share this one implementation instead of keeping its own copy.
func WriteError(w http.ResponseWriter, status int, message, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errorBody{Error: message, Code: code})
}

func WriteJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
