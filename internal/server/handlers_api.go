package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/yelinaung/trackkr/internal/db"
	"github.com/yelinaung/trackkr/internal/icon"
	"github.com/yelinaung/trackkr/internal/identity"
)

type ingestRequest struct {
	Records []ingestRecord `json:"records"`
}

type ingestRecord struct {
	RecordID  string            `json:"record_id"`
	Producer  identity.Producer `json:"producer"`
	AppName   string            `json:"app_name"`
	Title     string            `json:"title"`
	URL       string            `json:"url,omitempty"`
	StartedAt time.Time         `json:"started_at"`
	EndedAt   time.Time         `json:"ended_at"`
	DurationS int               `json:"duration_s"`
}

type ingestResponse struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
	// Clamped counts records whose interval was trimmed to the duration they
	// reported. It is surfaced rather than corrected silently: a non-zero
	// count means some client is measuring one of the two wrong.
	Clamped int `json:"clamped"`
}

// ingestDurationSlack is how far an interval may exceed the duration a record
// reports before the interval is treated as wrong. A client truncates its
// duration to whole seconds while keeping sub-second timestamps, so just under
// a second of excess is normal.
const ingestDurationSlack = 2 * time.Second

// clampSuspendedInterval trims a record whose interval outruns the time it
// says it measured, and reports whether it did.
//
// A daemon that measures elapsed time with a monotonic clock but stamps wall
// clock timestamps disagrees with itself across a suspend: the monotonic clock
// stops while the machine sleeps, so an hour of lid-closed time lands in the
// interval and not in the duration. Charting the interval turns that sleep
// into an hour of use.
//
// The duration is the trustworthy half -- it is what the client actually
// measured -- so the interval is brought back to it rather than the record
// being dropped. A client that reports no duration is left alone; there is
// nothing better to believe than its timestamps.
func clampSuspendedInterval(startedAt, endedAt time.Time, durationS int) (time.Time, bool) {
	if durationS <= 0 {
		return endedAt, false
	}
	reported := time.Duration(durationS) * time.Second
	if endedAt.Sub(startedAt) <= reported+ingestDurationSlack {
		return endedAt, false
	}
	return startedAt.Add(reported), true
}

// deviceView is the API's device representation. It deliberately omits
// APIKey: DeviceRow carries the plaintext key, so marshalling the row
// would let one compromised device key harvest every other key on the
// account. The session-authenticated /devices page shows keys on
// purpose; that is a different trust level.
type deviceView struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	DeviceType string    `json:"device_type"`
	CreatedAt  time.Time `json:"created_at"`
}

type devicesResponse struct {
	Devices []deviceView `json:"devices"`
}

// HandleListDevices lists the devices belonging to the same user as the
// authenticated device.
func HandleListDevices(queries APIQuerier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		device := DeviceFromContext(r.Context())
		if device == nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}

		devices, err := queries.ListDevicesByUser(r.Context(), device.UserID)
		if err != nil {
			http.Error(w, `{"error":"failed to list devices"}`, http.StatusInternalServerError)
			return
		}

		views := make([]deviceView, 0, len(devices))
		for _, d := range devices {
			views = append(views, deviceView{
				ID:         d.ID,
				Name:       d.Name,
				DeviceType: d.DeviceType,
				CreatedAt:  d.CreatedAt,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(devicesResponse{Devices: views})
	}
}

func HandleIngestActivity(queries APIQuerier) http.HandlerFunc {
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

		rows := make([]db.ActivityRecordRow, 0, len(req.Records))
		rejected := 0
		clamped := 0
		for i := range req.Records {
			rec := &req.Records[i]
			// The producer is trusted downstream to scope deduplication, so
			// an unknown non-empty value is a rejection rather than a
			// silent coercion. A missing one is normalized for daemons
			// that predate the field.
			producer := rec.Producer
			if producer == "" {
				producer = identity.ProducerDesktop
				if rec.URL != "" && icon.AppKey(rec.AppName) == "firefox" {
					producer = identity.ProducerFirefox
				}
			}
			if !identity.ValidProducer(producer) || !identity.Valid(rec.RecordID) {
				rejected++
				continue
			}
			if !rec.StartedAt.Before(rec.EndedAt) {
				// Permanently invalid rows are acknowledged and discarded so one
				// stale pending record cannot wedge a reporter's retry queue.
				rejected++
				continue
			}
			endedAt, trimmed := clampSuspendedInterval(rec.StartedAt, rec.EndedAt, rec.DurationS)
			if trimmed {
				clamped++
			}
			var url *string
			if rec.URL != "" {
				url = &rec.URL
			}
			rows = append(rows, db.ActivityRecordRow{
				DeviceID:  device.ID,
				RecordID:  rec.RecordID,
				Producer:  producer,
				AppName:   rec.AppName,
				Title:     rec.Title,
				URL:       url,
				StartedAt: rec.StartedAt,
				EndedAt:   endedAt,
				DurationS: rec.DurationS,
			})
		}

		accepted := 0
		if len(rows) > 0 {
			var err error
			accepted, err = queries.InsertActivityRecords(r.Context(), rows)
			if err != nil {
				http.Error(w, `{"error":"failed to insert records"}`, http.StatusInternalServerError)
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(ingestResponse{
			Accepted: accepted,
			Rejected: rejected,
			Clamped:  clamped,
		})
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
