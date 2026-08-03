package server

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strconv"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/rs/zerolog"
	"github.com/yelinaung/trackkr/internal/db"
	"github.com/yelinaung/trackkr/internal/favicon"
	"golang.org/x/crypto/bcrypt"
)

const (
	// minPasswordChars is the shortest password registration accepts,
	// counted in characters to match the form's minlength and the
	// message shown on rejection. Counting bytes instead would let
	// four CJK characters through as "12".
	minPasswordChars = 12
	// maxPasswordBytes is bcrypt's hard limit, which is genuinely a
	// byte count: a passphrase of accented or non-Latin characters
	// reaches it well before 72 characters.
	maxPasswordBytes = 72

	// uniqueViolation is PostgreSQL's SQLSTATE for a duplicate key.
	uniqueViolation = "23505"
	// dateLayout is the value the date input submits and the dashboard links
	// carry.
	dateLayout             = "2006-01-02"
	flashKindError         = "error"
	dashboardViewDay       = "day"
	dashboardViewWeek      = "week"
	dashboardTotalPageSize = 10
	// dashboardTotalLimit is two ten-row disclosure batches per column.
	dashboardTotalLimit = 2 * dashboardTotalPageSize
)

// isUniqueViolation reports whether err is a duplicate-key error rather
// than an operational failure.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}

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
	GetActivitySummary(ctx context.Context, userID int64, start, end time.Time, deviceID *int64) (*db.ActivitySummary, error)
	GetSiteTotals(ctx context.Context, userID int64, start, end time.Time, deviceID *int64) ([]db.SiteTotalRow, error)
	ListCategories(ctx context.Context, userID int64) ([]db.CategorySummaryRow, error)
	CreateCategory(ctx context.Context, userID int64, name, colorKey string) (*db.CategoryRow, error)
	UpdateCategory(ctx context.Context, userID, categoryID int64, name, colorKey string) (*db.CategoryRow, error)
	DeleteCategory(ctx context.Context, userID, categoryID int64) error
	ListKnownApplications(ctx context.Context, userID int64, since time.Time, limit int) ([]db.KnownApplicationRow, error)
	ListAppCategoryAssignments(ctx context.Context, userID int64, appKeys []string) (map[string]db.CategoryRow, error)
	SetAppCategory(ctx context.Context, userID int64, appKey string, categoryID *int64) error
	ListEditableActivityRecords(ctx context.Context, userID int64, filter *db.EditableActivityFilter) (*db.EditableActivityPage, error)
	SetActivityRecordCategoryOverride(ctx context.Context, userID, recordID int64, categoryID *int64) error
	DeleteActivityRecordCategoryOverride(ctx context.Context, userID, recordID int64) error
	SetActivityRecordApplicationCategory(ctx context.Context, userID, recordID int64, categoryID *int64) error
}

// webHandlers carries what every dashboard handler needs.
type webHandlers struct {
	queries        WebQuerier
	icons          appIconReader
	siteIcons      siteIconStore
	siteRefresh    siteIconRefreshQueue
	templates      *templates
	codec          *sessionCodec
	limiter        *attemptLimiter
	loc            *time.Location
	logger         *zerolog.Logger
	allowReg       bool
	trustedProxies []netip.Prefix
}

// ensureCSRF reuses the request's token or mints one. Every page that
// renders a form needs this; reading the cookie alone leaves the hidden
// field empty for a visitor who has not been to /login this session.
func (h *webHandlers) ensureCSRF(w http.ResponseWriter, r *http.Request) (string, error) {
	if c, err := r.Cookie(csrfCookieName); err == nil && c.Value != "" {
		return c.Value, nil
	}
	return h.codec.issueCSRF(w)
}

func (h *webHandlers) base(r *http.Request, token string) *pageData {
	return &pageData{
		User:              UserFromContext(r.Context()),
		CSRFToken:         token,
		Timezone:          h.loc.String(),
		AllowRegistration: h.allowReg,
		RecordLimit:       db.ActivityRecordLimit,
		SourceLimit:       db.ActivitySourceLimit,
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
		token, err := h.ensureCSRF(w, r)
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
		host := clientHost(r, h.trustedProxies)
		now := time.Now()

		// Claim the attempt before doing any work: reserving up front
		// is what stops a concurrent burst from getting more bcrypt
		// comparisons than the limit allows. A success releases it.
		if !h.limiter.reserve(host, now) {
			http.Error(w, "too many attempts, try again later", http.StatusTooManyRequests)
			return
		}

		username := r.PostFormValue("username")
		user, err := h.queries.GetUserByUsername(r.Context(), username)
		if err != nil {
			// Only a missing row is a failed login. Anything else is
			// our problem, not the visitor's: report it as a 500 and
			// hand back the attempt they reserved.
			if !errors.Is(err, pgx.ErrNoRows) {
				h.limiter.release(host)
				h.fail(w, err, "looking up user")
				return
			}
			// Spend the same time as a real comparison.
			_ = bcrypt.CompareHashAndPassword([]byte(bcryptDummyHash), []byte(r.PostFormValue("password")))
			h.renderLoginError(w, r)
			return
		}

		if err := bcrypt.CompareHashAndPassword([]byte(user.Password), []byte(r.PostFormValue("password"))); err != nil {
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
	data.FlashKind = flashKindError

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
		token, err := h.ensureCSRF(w, r)
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
		// Registration hashes with bcrypt too, so it needs the same
		// throttle as login or it becomes the cheaper CPU target.
		host := clientHost(r, h.trustedProxies)
		if !h.limiter.reserve(host, time.Now()) {
			http.Error(w, "too many attempts, try again later", http.StatusTooManyRequests)
			return
		}

		username := r.PostFormValue("username")
		password := r.PostFormValue("password")

		if username == "" || utf8.RuneCountInString(password) < minPasswordChars {
			h.renderRegisterError(w, r, fmt.Sprintf(
				"Pick a username and a password of at least %d characters.", minPasswordChars,
			))
			return
		}

		// bcrypt refuses anything over 72 bytes, which a fairly short
		// non-ASCII passphrase can reach. Say so here rather than
		// letting GenerateFromPassword turn it into a 500.
		if len(password) > maxPasswordBytes {
			h.renderRegisterError(w, r, fmt.Sprintf(
				"That password is too long: %d bytes, and the limit is %d. "+
					"Accented or non-Latin characters count as several bytes each.",
				len(password), maxPasswordBytes,
			))
			return
		}

		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			h.fail(w, err, "hashing password")
			return
		}

		user, err := h.queries.CreateUser(r.Context(), username, string(hash))
		if err != nil {
			// Only a uniqueness violation means the name is taken;
			// telling someone to pick another name during an outage
			// sends them chasing a problem that is not theirs.
			if !isUniqueViolation(err) {
				h.fail(w, err, "creating user")
				return
			}
			h.renderRegisterError(w, r, "That username is taken.")
			return
		}

		// The reservation stands. Unlike login, where proving
		// credentials clears the bucket, a successful registration is
		// exactly what an abuser repeats: every unique username
		// succeeds, so giving the attempt back would let one host mint
		// accounts without limit, each costing a bcrypt hash and a row.
		// Keeping it caps registrations at the same rate as failures.
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
	data.FlashKind = flashKindError

	w.WriteHeader(http.StatusBadRequest)
	if err := h.templates.renderPage(w, pageRegister, data); err != nil {
		h.fail(w, err, "rendering register")
	}
}

func (h *webHandlers) handleDashboard() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := h.timelineData(w, r)
		if err != nil {
			h.fail(w, err, "building dashboard")
			return
		}

		if err := h.templates.renderPage(w, pageDashboard, data); err != nil {
			h.fail(w, err, "rendering dashboard")
		}
	}
}

// handleTimeline serves the htmx partial for the view, date, and device filters.
func (h *webHandlers) handleTimeline() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := h.timelineData(w, r)
		if err != nil {
			h.fail(w, err, "building timeline")
			return
		}

		data.Partial = true
		if err := h.templates.renderPartial(w, partialTimeline, data); err != nil {
			h.fail(w, err, "rendering timeline")
		}
	}
}

// dashboardWindow is the period one request selects: the day it names, the
// day-or-week span that follows from it, and an optional single device.
type dashboardWindow struct {
	day        time.Time
	view       string
	start, end time.Time
	deviceID   *int64
}

func (h *webHandlers) parseWindow(r *http.Request) *dashboardWindow {
	day := h.parseDay(r.URL.Query().Get(dateParam))
	view := parseDashboardView(r.URL.Query().Get(viewParam))
	start, end := dayBounds(day)
	if view == dashboardViewWeek {
		start, end = weekBounds(day)
	}
	return &dashboardWindow{
		day:      day,
		view:     view,
		start:    start,
		end:      end,
		deviceID: parseDeviceID(r.URL.Query().Get("device")),
	}
}

func (w *dashboardWindow) label() string {
	if w.view == dashboardViewWeek {
		return weekLabel(w.start, w.end)
	}
	return w.start.Format("Monday, 2 January 2006")
}

// apply writes the filter state every page with the period controls needs.
func (h *webHandlers) applyWindow(data *pageData, win *dashboardWindow) {
	if win.deviceID != nil {
		data.SelectedDevice = *win.deviceID
	}
	data.Date = win.day.Format(dateLayout)
	data.Today = time.Now().In(h.loc).Format(dateLayout)
	data.DateLabel = win.label()
	data.View = win.view
}

// timelineData gathers the selected day or week for the signed-in user.
func (h *webHandlers) timelineData(w http.ResponseWriter, r *http.Request) (*pageData, error) {
	user := UserFromContext(r.Context())
	if user == nil {
		return nil, errors.New("timeline requested without a session")
	}

	token, err := h.ensureCSRF(w, r)
	if err != nil {
		return nil, err
	}
	data := h.base(r, token)
	win := h.parseWindow(r)
	h.applyWindow(data, win)

	devices, err := h.queries.ListDevicesByUser(r.Context(), user.ID)
	if err != nil {
		return nil, err
	}

	activity, err := h.queries.GetActivitySummary(r.Context(), user.ID, win.start, win.end, win.deviceID)
	if err != nil {
		return nil, err
	}
	records := activity.Records
	totals := activity.Totals
	displayTotals := totals[:min(len(totals), dashboardTotalLimit)]

	appNames := make([]string, 0, len(displayTotals))
	for _, total := range displayTotals {
		appNames = append(appNames, total.AppName)
	}
	iconsByKey := h.appIcons(r.Context(), user.ID, appNames)

	sites, err := h.queries.GetSiteTotals(r.Context(), user.ID, win.start, win.end, win.deviceID)
	if err != nil {
		return nil, err
	}
	displaySites := sites[:min(len(sites), dashboardTotalLimit)]

	siteViews := make([]TotalView, 0, len(displaySites))
	for _, site := range displaySites {
		view := h.siteTotalView(user.ID, site)
		view.DetailURL = detailURL(detailKindSite, site.Site, win)
		siteViews = append(siteViews, view)
	}

	for _, total := range totals {
		data.TotalSeconds += total.Seconds
	}
	views := make([]TotalView, 0, len(displayTotals))
	for _, t := range displayTotals {
		view := appTotalView(t, iconsByKey)
		view.DetailURL = detailURL(detailKindApp, t.AppName, win)
		views = append(views, view)
	}

	data.Devices = devices
	data.Totals = views
	data.Sites = siteViews
	data.CategoryTotals = activity.CategoryTotals
	if win.view == dashboardViewWeek {
		data.Chart = layoutWeek(records, devices, win.start, win.end)
	} else {
		data.Chart = layout(records, devices, win.day)
	}
	data.Truncated = activity.TimelineTruncated
	data.SourceTruncated = activity.SourceTruncated

	return data, nil
}

// appIcons resolves stored icons for a set of application names.
//
// One application can be stored under several keys: the macOS detector
// reports "Google Chrome" and the Linux X11 detector "google-chrome", so a
// user with both devices has two icon rows for one browser. Ask for every
// candidate and choose by the ordered alias list in appTotalView, never by
// whichever row the database returned first.
//
// A failure here loses the icons, not the page: monograms stand in.
func (h *webHandlers) appIcons(
	ctx context.Context,
	userID int64,
	appNames []string,
) map[string]db.AppIconRow {
	keys := make([]string, 0, len(appNames)*2)
	seen := make(map[string]struct{}, len(appNames)*2)
	for _, name := range appNames {
		for _, key := range db.AppIconKeys(name) {
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}

	rows, err := h.icons.AppIconMetadata(ctx, userID, keys)
	if err != nil {
		h.logger.Warn().Err(err).Msg("application icon metadata unavailable")
		return nil
	}
	byKey := make(map[string]db.AppIconRow, len(rows))
	for i := range rows {
		byKey[rows[i].AppKey] = rows[i]
	}
	return byKey
}

// appTotalView builds one application row from its total and the icons
// already resolved for the page.
func appTotalView(total db.AppTotalRow, iconsByKey map[string]db.AppIconRow) TotalView {
	view := newTotalView(total.AppName, total.Seconds)
	if row, ok := firstPresentIcon(iconsByKey, db.AppIconKeys(total.AppName)); ok {
		view.IconURL = fmt.Sprintf(
			"/app-icons/%d/%s.png",
			row.ID,
			hex.EncodeToString(row.SHA256),
		)
	}
	return view
}

// siteTotalView builds one website row. Only a site key the fetcher would
// accept gets a signed icon URL; anything else keeps its monogram.
func (h *webHandlers) siteTotalView(userID int64, site db.SiteTotalRow) TotalView {
	view := newTotalView(site.Site, site.Seconds)
	canonical, err := favicon.CanonicalSite(site.Site)
	if err == nil && canonical == site.Site {
		view.IconURL = fmt.Sprintf(
			"/site-icons/%s?sig=%s",
			site.Site,
			h.codec.siteIconSignature(userID, site.Site),
		)
	}
	return view
}

func newTotalView(name string, seconds int64) TotalView {
	fill, chip, monogramFill := appPalette(name)
	return TotalView{
		AppName:      name,
		Seconds:      seconds,
		Fill:         fill,
		Monogram:     appMonogram(name),
		MonogramFill: monogramFill,
		MonogramBG:   chip,
	}
}

func appMonogram(name string) string {
	runes := make([]rune, 0, 2)
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			continue
		}
		runes = append(runes, unicode.ToUpper(r))
		if len(runes) == 2 {
			break
		}
	}
	if len(runes) == 0 {
		return "?"
	}
	return string(runes)
}

// parseDay falls back to today in the server's timezone.
func (h *webHandlers) parseDay(raw string) time.Time {
	if raw != "" {
		if day, err := time.ParseInLocation(dateLayout, raw, h.loc); err == nil {
			return day
		}
	}
	return time.Now().In(h.loc)
}

func parseDashboardView(raw string) string {
	if raw == dashboardViewWeek {
		return dashboardViewWeek
	}
	return dashboardViewDay
}

// weekBounds returns the Monday-based calendar week containing day.
func weekBounds(day time.Time) (start, end time.Time) {
	start, _ = dayBounds(day)
	daysSinceMonday := (int(start.Weekday()) + 6) % 7
	start = start.AddDate(0, 0, -daysSinceMonday)
	return start, start.AddDate(0, 0, 7)
}

func weekLabel(start, end time.Time) string {
	last := end.AddDate(0, 0, -1)
	if start.Year() != last.Year() {
		return fmt.Sprintf("%s - %s", start.Format("2 January 2006"), last.Format("2 January 2006"))
	}
	if start.Month() != last.Month() {
		return fmt.Sprintf("%s - %s", start.Format("2 January"), last.Format("2 January 2006"))
	}
	return fmt.Sprintf("%d-%s", start.Day(), last.Format("2 January 2006"))
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
		data, err := h.devicesData(w, r)
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

		data, err := h.devicesData(w, r)
		if err != nil {
			h.fail(w, err, "listing devices")
			return
		}

		if err := h.templates.renderPartial(w, partialDevices, data); err != nil {
			h.fail(w, err, "rendering device rows")
		}
	}
}

func (h *webHandlers) devicesData(w http.ResponseWriter, r *http.Request) (*pageData, error) {
	user := UserFromContext(r.Context())
	if user == nil {
		return nil, errors.New("devices requested without a session")
	}

	token, err := h.ensureCSRF(w, r)
	if err != nil {
		return nil, err
	}

	devices, err := h.queries.ListDevicesByUser(r.Context(), user.ID)
	if err != nil {
		return nil, err
	}

	data := h.base(r, token)
	data.Devices = devices
	return data, nil
}

// firstPresentIcon picks the earliest candidate key that has a stored icon.
//
// The order is the caller's preference, not the database's: selecting by result
// order would give a user with a Mac and a Linux box whichever row happened to
// sort first, and the answer could change between renders.
func firstPresentIcon(rows map[string]db.AppIconRow, keys []string) (db.AppIconRow, bool) {
	for _, key := range keys {
		if row, ok := rows[key]; ok {
			return row, true
		}
	}
	return db.AppIconRow{}, false
}
