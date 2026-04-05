package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
)

type ingestRequest struct {
	Records []ingestRecord `json:"records"`
}

type ingestRecord struct {
	AppName   string    `json:"app_name"`
	Title     string    `json:"title"`
	URL       string    `json:"url,omitempty"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	DurationS int       `json:"duration_s"`
}

type ingestResponse struct {
	Accepted int `json:"accepted"`
}

func HandleIngestActivity(queries Querier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		device := DeviceFromContext(r.Context())
		if device == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		var req ingestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
			return
		}

		if len(req.Records) == 0 {
			http.Error(w, `{"error":"no records provided"}`, http.StatusBadRequest)
			return
		}

		rows := make([]db.ActivityRecordRow, len(req.Records))
		for i, rec := range req.Records {
			var url *string
			if rec.URL != "" {
				url = &rec.URL
			}
			rows[i] = db.ActivityRecordRow{
				DeviceID:  device.ID,
				AppName:   rec.AppName,
				Title:     rec.Title,
				URL:       url,
				StartedAt: rec.StartedAt,
				EndedAt:   rec.EndedAt,
				DurationS: rec.DurationS,
			}
		}

		accepted, err := queries.InsertActivityRecords(r.Context(), rows)
		if err != nil {
			http.Error(w, `{"error":"failed to insert records"}`, http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ingestResponse{Accepted: accepted})
	}
}

func HandleHeartbeat() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		device := DeviceFromContext(r.Context())
		if device == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}
}
