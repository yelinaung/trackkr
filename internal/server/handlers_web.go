package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
	"golang.org/x/crypto/bcrypt"
)

// defaultDeviceType is used when the form leaves the type blank.
const defaultDeviceType = "desktop"

// bcryptDummyHash is compared against when the username is unknown, so a
// failed login costs the same time either way and does not reveal which
// accounts exist. It is the hash of a value nobody can submit.
const bcryptDummyHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

// WebQuerier is everything the dashboard reads and writes. It is kept
// apart from APIQuerier so a test fake only implements what it uses.
type WebQuerier interface {
	GetUserByID(ctx context.Context, id int64) (*db.UserRow, error)
	GetUserByUsername(ctx context.Context, username string) (*db.UserRow, error)
	CreateUser(ctx context.Context, username, passwordHash string) (*db.UserRow, error)
	ListDevicesByUser(ctx context.Context, userID int64) ([]db.DeviceRow, error)
	CreateDevice(ctx context.Context, userID int64, name, deviceType, apiKey string) (*db.DeviceRow, error)
	DeleteDevice(ctx context.Context, id, userID int64) error
	GetActivityRecords(ctx context.Context, userID int64, start, end time.Time, deviceID *int64) ([]db.ActivityRecordRow, error)
	GetAppTotals(ctx context.Context, userID int64, start, end time.Time, deviceID *int64) ([]db.AppTotalRow, error)
}

// webHandlers carries what every dashboard handler needs.
type webHandlers struct {
	queries   WebQuerier
	templates *templates
	codec     *sessionCodec
	limiter   *attemptLimiter
	loc       *time.Location
	logger    *zerolog.Logger
	allowReg  bool
}

func (h *webHandlers) base(r *http.Request, token string) *pageData {
	return &pageData{
		User:              UserFromContext(r.Context()),
		CSRFToken:         token,
		Timezone:          h.loc.String(),
		AllowRegistration: h.allowReg,
		RecordLimit:       db.ActivityRecordLimit,
	}
}

// fail logs and shows a generic error; template and database details do
// not belong in a response body.
func (h *webHandlers) fail(w http.ResponseWriter, err error, msg string) {
	h.logger.Error().Err(err).Msg(msg)
	http.Error(w, "something went wrong", http.StatusInternalServerError)
}

func (h *webHandlers) handleLoginForm() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, err := h.codec.issueCSRF(w)
		if err != nil {
			h.fail(w, err, "issuing csrf token")
			return
		}

		data := h.base(r, token)
		if err := h.templates.renderPage(w, pageLogin, data); err != nil {
			h.fail(w, err, "rendering login")
		}
	}
}

func (h *webHandlers) handleLogin() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host := clientHost(r)
		now := time.Now()

		if !h.limiter.allow(host, now) {
			http.Error(w, "too many attempts, try again later", http.StatusTooManyRequests)
			return
		}

		username := r.PostFormValue("username")
		user, err := h.queries.GetUserByUsername(r.Context(), username)
		if err != nil {
			// Spend the same time as a real comparison.
			_ = bcrypt.CompareHashAndPassword([]byte(bcryptDummyHash), []byte(r.PostFormValue("password")))
			h.limiter.record(host, now)
			h.renderLoginError(w, r)
			return
		}

		if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(r.PostFormValue("password"))); err != nil {
			h.limiter.record(host, now)
			h.renderLoginError(w, r)
			return
		}

		h.limiter.reset(host)
		// Rotate both cookies so a pre-auth CSRF value cannot be pinned
		// into the authenticated session.
		if _, err := h.codec.issueCSRF(w); err != nil {
			h.fail(w, err, "rotating csrf token")
			return
		}
		h.codec.setSession(w, user.ID)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

// renderLoginError says the same thing for an unknown user and a wrong
// password.
func (h *webHandlers) renderLoginError(w http.ResponseWriter, r *http.Request) {
	token, err := h.codec.issueCSRF(w)
	if err != nil {
		h.fail(w, err, "issuing csrf token")
		return
	}

	data := h.base(r, token)
	data.Flash = "Invalid username or password."
	data.FlashKind = "error"

	w.WriteHeader(http.StatusUnauthorized)
	if err := h.templates.renderPage(w, pageLogin, data); err != nil {
		h.fail(w, err, "rendering login")
	}
}

func (h *webHandlers) handleLogout() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h.codec.clearSession(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	}
}

func (h *webHandlers) handleRegisterForm() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, err := h.codec.issueCSRF(w)
		if err != nil {
			h.fail(w, err, "issuing csrf token")
			return
		}

		if err := h.templates.renderPage(w, pageRegister, h.base(r, token)); err != nil {
			h.fail(w, err, "rendering register")
		}
	}
}

func (h *webHandlers) handleRegister() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := r.PostFormValue("username")
		password := r.PostFormValue("password")

		if username == "" || len(password) < 12 {
			h.renderRegisterError(w, r, "Pick a username and a password of at least 12 characters.")
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			h.fail(w, err, "hashing password")
			return
		}

		user, err := h.queries.CreateUser(r.Context(), username, string(hash))
		if err != nil {
			h.renderRegisterError(w, r, "That username is taken.")
			return
		}

		if _, err := h.codec.issueCSRF(w); err != nil {
			h.fail(w, err, "rotating csrf token")
			return
		}
		h.codec.setSession(w, user.ID)
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
}

func (h *webHandlers) renderRegisterError(w http.ResponseWriter, r *http.Request, msg string) {
	token, err := h.codec.issueCSRF(w)
	if err != nil {
		h.fail(w, err, "issuing csrf token")
		return
	}

	data := h.base(r, token)
	data.Flash = msg
	data.FlashKind = "error"

	w.WriteHeader(http.StatusBadRequest)
	if err := h.templates.renderPage(w, pageRegister, data); err != nil {
		h.fail(w, err, "rendering register")
	}
}

func (h *webHandlers) handleDashboard() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := h.timelineData(r)
		if err != nil {
			h.fail(w, err, "building dashboard")
			return
		}

		if err := h.templates.renderPage(w, pageDashboard, data); err != nil {
			h.fail(w, err, "rendering dashboard")
		}
	}
}

// handleTimeline serves the htmx partial for the date and device filter.
func (h *webHandlers) handleTimeline() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := h.timelineData(r)
		if err != nil {
			h.fail(w, err, "building timeline")
			return
		}

		if err := h.templates.renderPartial(w, "timeline", data); err != nil {
			h.fail(w, err, "rendering timeline")
		}
	}
}

// timelineData gathers one day of activity for the signed-in user.
func (h *webHandlers) timelineData(r *http.Request) (*pageData, error) {
	user := UserFromContext(r.Context())
	if user == nil {
		return nil, errors.New("timeline requested without a session")
	}

	token := ""
	if c, err := r.Cookie(csrfCookieName); err == nil {
		token = c.Value
	}
	data := h.base(r, token)

	day := h.parseDay(r.URL.Query().Get("date"))
	start, end := dayBounds(day)

	deviceID := parseDeviceID(r.URL.Query().Get("device"))
	if deviceID != nil {
		data.SelectedDevice = *deviceID
	}

	devices, err := h.queries.ListDevicesByUser(r.Context(), user.ID)
	if err != nil {
		return nil, err
	}

	records, err := h.queries.GetActivityRecords(r.Context(), user.ID, start, end, deviceID)
	if err != nil {
		return nil, err
	}

	totals, err := h.queries.GetAppTotals(r.Context(), user.ID, start, end, deviceID)
	if err != nil {
		return nil, err
	}

	for _, t := range totals {
		data.TotalSeconds += t.Seconds
	}

	data.Devices = devices
	data.Totals = totals
	data.Chart = layout(records, devices, day)
	data.Date = start.Format("2006-01-02")
	data.Today = time.Now().In(h.loc).Format("2006-01-02")
	data.DateLabel = start.Format("Monday, 2 January 2006")
	// The cap truncates the end of the day; totals still cover all of it.
	data.Truncated = len(records) >= db.ActivityRecordLimit
	data.Chart.Truncated = data.Truncated

	return data, nil
}

// parseDay falls back to today in the server's timezone.
func (h *webHandlers) parseDay(raw string) time.Time {
	if raw != "" {
		if day, err := time.ParseInLocation("2006-01-02", raw, h.loc); err == nil {
			return day
		}
	}
	return time.Now().In(h.loc)
}

func parseDeviceID(raw string) *int64 {
	if raw == "" {
		return nil
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil
	}
	return &id
}

func (h *webHandlers) handleDevices() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := h.devicesData(r)
		if err != nil {
			h.fail(w, err, "listing devices")
			return
		}

		if err := h.templates.renderPage(w, pageDevices, data); err != nil {
			h.fail(w, err, "rendering devices")
		}
	}
}

func (h *webHandlers) handleCreateDevice() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())

		name := r.PostFormValue("name")
		if name == "" {
			http.Error(w, "device name is required", http.StatusBadRequest)
			return
		}

		deviceType := r.PostFormValue("device_type")
		if deviceType == "" {
			deviceType = defaultDeviceType
		}

		apiKey, err := GenerateAPIKey()
		if err != nil {
			h.fail(w, err, "generating api key")
			return
		}

		if _, err := h.queries.CreateDevice(r.Context(), user.ID, name, deviceType, apiKey); err != nil {
			h.fail(w, err, "creating device")
			return
		}

		http.Redirect(w, r, "/devices", http.StatusSeeOther)
	}
}

func (h *webHandlers) handleDeleteDevice() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())

		id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
		if err != nil {
			http.Error(w, "invalid device id", http.StatusBadRequest)
			return
		}

		// Scoped by user, so one account cannot delete another's device.
		if err := h.queries.DeleteDevice(r.Context(), id, user.ID); err != nil {
			h.fail(w, err, "deleting device")
			return
		}

		data, err := h.devicesData(r)
		if err != nil {
			h.fail(w, err, "listing devices")
			return
		}

		if err := h.templates.renderPartial(w, "device_rows", data); err != nil {
			h.fail(w, err, "rendering device rows")
		}
	}
}

func (h *webHandlers) devicesData(r *http.Request) (*pageData, error) {
	user := UserFromContext(r.Context())
	if user == nil {
		return nil, errors.New("devices requested without a session")
	}

	token := ""
	if c, err := r.Cookie(csrfCookieName); err == nil {
		token = c.Value
	}

	devices, err := h.queries.ListDevicesByUser(r.Context(), user.ID)
	if err != nil {
		return nil, err
	}

	data := h.base(r, token)
	data.Devices = devices
	return data, nil
}
