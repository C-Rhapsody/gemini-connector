package main

import (
	"encoding/json"
	"net/http"
	"time"
)

func (s *OpenAICompatibleServer) handleModels(w http.ResponseWriter, r *http.Request, reqID string, startTime time.Time, clientClass string) {
	models, err := s.catalog.GetModels(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "api_error", "Catalog unavailable", "catalog_unavailable", nil, true)
		s.logOutcome(reqID, "GET", "/v1/models", http.StatusServiceUnavailable, "catalog_unavailable", startTime, false, nil, 0, clientClass)
		return
	}

	resp := ModelListResponse{
		Object: "list",
		Data:   models,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
	s.logOutcome(reqID, "GET", "/v1/models", http.StatusOK, "", startTime, false, nil, 0, clientClass)
}
