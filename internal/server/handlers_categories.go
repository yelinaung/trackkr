package server

import (
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/yelinaung/trackkr/internal/db"
	"github.com/yelinaung/trackkr/internal/icon"
)

const (
	categoryNameMaxChars     = 64
	categoryRecentDays       = 180
	categoryColorSky         = "sky"
	categoryActionSet        = "set"
	categoryActionNone       = "uncategorized"
	categoryActionInherit    = "inherit"
	categoryScopeRecord      = "record"
	categoryScopeApplication = "application"
	categoryScopeField       = "scope"
	categoryActionField      = "action"
)

var categoryColors = map[string]struct{}{
	"coral": {}, "amber": {}, "leaf": {}, "teal": {},
	categoryColorSky: {}, "indigo": {}, "rose": {}, "slate": {},
}

func categoryColorOptions() []string {
	return []string{"coral", "amber", "leaf", "teal", categoryColorSky, "indigo", "rose", "slate"}
}

func normalizeCategoryName(name string) string {
	return strings.Join(strings.Fields(name), " ")
}

func validateCategory(name, colorKey string) (string, error) {
	name = normalizeCategoryName(name)
	if name == "" || utf8.RuneCountInString(name) > categoryNameMaxChars {
		return "", errors.New("category names must be 1 to 64 characters")
	}
	if _, ok := categoryColors[colorKey]; !ok {
		return "", errors.New("choose a category color")
	}
	return name, nil
}

func parsePositiveID(raw string) (*int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return nil, errors.New("id must be a positive integer")
	}
	return &id, nil
}

func (h *webHandlers) handleCategories() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		data, err := h.categoriesData(w, r)
		if err != nil {
			h.fail(w, err, "listing categories")
			return
		}
		if err := h.templates.renderPage(w, pageCategories, data); err != nil {
			h.fail(w, err, "rendering categories")
		}
	}
}

func (h *webHandlers) handleCreateCategory() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		name, err := validateCategory(r.PostFormValue("name"), r.PostFormValue("color_key"))
		if err != nil {
			h.renderCategoryError(w, r, err.Error())
			return
		}
		if _, err := h.queries.CreateCategory(r.Context(), user.ID, name, r.PostFormValue("color_key")); err != nil {
			if errors.Is(err, db.ErrCategoryLimit) {
				h.renderCategoryError(w, r, "You can create up to 50 categories.")
				return
			}
			if isUniqueViolation(err) {
				h.renderCategoryError(w, r, "A category with that name already exists.")
				return
			}
			h.fail(w, err, "creating category")
			return
		}
		redirectCategories(w, r)
	}
}

func redirectCategories(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get(htmxRequestHeader) == htmxRequestValue {
		w.Header().Set("HX-Redirect", "/categories")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/categories", http.StatusSeeOther)
}

func (h *webHandlers) handleUpdateCategory() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		categoryID, err := parsePositiveID(chi.URLParam(r, "id"))
		if err != nil {
			http.Error(w, "invalid category id", http.StatusBadRequest)
			return
		}
		name, err := validateCategory(r.PostFormValue("name"), r.PostFormValue("color_key"))
		if err != nil {
			h.renderCategoryError(w, r, err.Error())
			return
		}
		if _, err := h.queries.UpdateCategory(r.Context(), user.ID, *categoryID, name, r.PostFormValue("color_key")); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.NotFound(w, r)
				return
			}
			if isUniqueViolation(err) {
				h.renderCategoryError(w, r, "A category with that name already exists.")
				return
			}
			h.fail(w, err, "updating category")
			return
		}
		redirectCategories(w, r)
	}
}

func (h *webHandlers) handleDeleteCategory() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		categoryID, err := parsePositiveID(chi.URLParam(r, "id"))
		if err != nil {
			http.Error(w, "invalid category id", http.StatusBadRequest)
			return
		}
		if err := h.queries.DeleteCategory(r.Context(), user.ID, *categoryID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.NotFound(w, r)
				return
			}
			h.fail(w, err, "deleting category")
			return
		}
		redirectCategories(w, r)
	}
}

func (h *webHandlers) handleSetAppCategory() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		appKey := r.PostFormValue("app_key")
		if appKey == "" || appKey != db.CategoryAppKey(appKey) || len(appKey) > icon.MaxKeyBytes {
			http.Error(w, "invalid application", http.StatusBadRequest)
			return
		}
		selection, err := optionalCategoryID(r.PostFormValue("category_id"))
		if err != nil {
			http.Error(w, "invalid category id", http.StatusBadRequest)
			return
		}
		categoryID := selection.ID
		if err := h.queries.SetAppCategory(r.Context(), user.ID, appKey, categoryID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.NotFound(w, r)
				return
			}
			h.fail(w, err, "setting application category")
			return
		}
		redirectCategories(w, r)
	}
}

func (h *webHandlers) handleSetRecordCategory() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user := UserFromContext(r.Context())
		recordID, err := parsePositiveID(chi.URLParam(r, "id"))
		if err != nil {
			http.Error(w, "invalid record id", http.StatusBadRequest)
			return
		}
		scope := r.PostFormValue(categoryScopeField)
		action := r.PostFormValue(categoryActionField)
		selection, err := optionalCategoryID(r.PostFormValue("category_id"))
		if err != nil {
			http.Error(w, "invalid category id", http.StatusBadRequest)
			return
		}
		categoryID := selection.ID

		switch scope {
		case categoryScopeRecord:
			switch action {
			case categoryActionSet:
				if categoryID == nil {
					http.Error(w, "choose a category", http.StatusBadRequest)
					return
				}
				err = h.queries.SetActivityRecordCategoryOverride(r.Context(), user.ID, *recordID, categoryID)
			case categoryActionNone:
				if categoryID != nil {
					http.Error(w, "uncategorized does not take a category", http.StatusBadRequest)
					return
				}
				err = h.queries.SetActivityRecordCategoryOverride(r.Context(), user.ID, *recordID, nil)
			case categoryActionInherit:
				if categoryID != nil {
					http.Error(w, "inherit does not take a category", http.StatusBadRequest)
					return
				}
				err = h.queries.DeleteActivityRecordCategoryOverride(r.Context(), user.ID, *recordID)
			default:
				http.Error(w, "invalid record category action", http.StatusBadRequest)
				return
			}
		case categoryScopeApplication:
			if action != categoryActionSet && action != categoryActionNone {
				http.Error(w, "invalid application category action", http.StatusBadRequest)
				return
			}
			if action == categoryActionSet && categoryID == nil {
				http.Error(w, "choose a category", http.StatusBadRequest)
				return
			}
			if action == categoryActionNone && categoryID != nil {
				http.Error(w, "uncategorized does not take a category", http.StatusBadRequest)
				return
			}
			err = h.queries.SetActivityRecordApplicationCategory(r.Context(), user.ID, *recordID, categoryID)
		default:
			http.Error(w, "invalid record category scope", http.StatusBadRequest)
			return
		}

		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.NotFound(w, r)
				return
			}
			h.fail(w, err, "setting record category")
			return
		}
		redirectRecordCategory(w, r, r.PostFormValue("return_to"))
	}
}

type categorySelection struct {
	ID *int64
}

func optionalCategoryID(raw string) (categorySelection, error) {
	if raw == "" {
		return categorySelection{}, nil
	}
	id, err := parsePositiveID(raw)
	return categorySelection{ID: id}, err
}

func redirectRecordCategory(w http.ResponseWriter, r *http.Request, raw string) {
	target := recordCategoryReturnURL(raw)
	if r.Header.Get(htmxRequestHeader) == htmxRequestValue {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	//nolint:gosec // recordCategoryReturnURL accepts only a re-encoded local activity URL.
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func recordCategoryReturnURL(raw string) string {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil || parsed.Path != "/activity" || parsed.RawQuery == "" {
		return "/"
	}
	query, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "/"
	}
	return "/activity?" + query.Encode()
}

func (h *webHandlers) renderCategoryError(w http.ResponseWriter, r *http.Request, message string) {
	data, err := h.categoriesData(w, r)
	if err != nil {
		h.fail(w, err, "building category error")
		return
	}
	data.Flash = message
	data.FlashKind = flashKindError
	data.CategoryFormName = r.PostFormValue("name")
	data.CategoryFormColor = r.PostFormValue("color_key")
	w.WriteHeader(http.StatusBadRequest)
	if err := h.templates.renderPage(w, pageCategories, data); err != nil {
		h.fail(w, err, "rendering category error")
	}
}

func (h *webHandlers) categoriesData(w http.ResponseWriter, r *http.Request) (*pageData, error) {
	user := UserFromContext(r.Context())
	if user == nil {
		return nil, errors.New("categories requested without a session")
	}
	token, err := h.ensureCSRF(w, r)
	if err != nil {
		return nil, err
	}
	data := h.base(r, token)
	categories, err := h.queries.ListCategories(r.Context(), user.ID)
	if err != nil {
		return nil, err
	}
	applications, err := h.queries.ListKnownApplications(
		r.Context(), user.ID, time.Now().AddDate(0, 0, -categoryRecentDays), db.KnownApplicationLimit,
	)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(applications))
	for _, application := range applications {
		keys = append(keys, application.AppKey)
	}
	assignments, err := h.queries.ListAppCategoryAssignments(r.Context(), user.ID, keys)
	if err != nil {
		return nil, err
	}
	appNames := make([]string, 0, len(applications))
	for _, application := range applications {
		appNames = append(appNames, application.AppName)
	}
	iconsByKey := h.appIcons(r.Context(), user.ID, appNames)
	views := make([]CategoryApplicationView, 0, len(applications))
	for _, application := range applications {
		view := CategoryApplicationView{
			AppKey: application.AppKey, AppName: application.AppName, LastSeen: application.LastSeen,
			Monogram: appMonogram(application.AppName),
		}
		_, view.MonogramBG, view.MonogramFill = appPalette(application.AppName)
		if assignment, ok := assignments[application.AppKey]; ok {
			view.Assignment = &assignment
		}
		if row, ok := firstPresentIcon(iconsByKey, db.AppIconKeys(application.AppName)); ok {
			view.IconURL = "/app-icons/" + strconv.FormatInt(row.ID, 10) + "/" + hex.EncodeToString(row.SHA256) + ".png"
		}
		views = append(views, view)
	}
	data.Categories = categories
	data.KnownApplications = views
	data.CategoryFormColor = categoryColorSky
	return data, nil
}
