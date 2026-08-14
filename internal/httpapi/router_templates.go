package httpapi

import (
	"bytes"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

type modulePageData struct {
	Navigation      publicNavigationData
	Module          domain.Module
	Release         domain.Release
	ReadmeHTML      template.HTML
	DownloadPath    string
	IsUpstream      bool
	PublicBaseURL   string
	ModuleInstallID string
	ReadTokenHint   string
}

type indexPageData struct {
	Navigation publicNavigationData
	Modules    []domain.Module
	Owner      string
	Query      string
	Filter     listFilterData
	Pagination paginationData
}

type listFilterData struct {
	ID          string
	InputID     string
	Action      string
	Target      string
	Label       string
	Name        string
	Placeholder string
	Value       string
	ClearURL    string
	Params      []queryParameter
}

func newListFilter(id, inputID, action, target, label, name, placeholder, value, clearURL string, params ...queryParameter) listFilterData {
	filter := listFilterData{
		ID:          id,
		InputID:     inputID,
		Action:      action,
		Target:      target,
		Label:       label,
		Name:        name,
		Placeholder: placeholder,
		Value:       value,
		Params:      params,
	}
	if value != "" {
		filter.ClearURL = clearURL
	}
	return filter
}

type publicNavigationData struct {
	CSPNonce        string
	AuthLink        string
	AuthLabel       string
	ModulesActive   bool
	Breadcrumbs     []publicBreadcrumb
	Versions        []domain.ModuleVersion
	SelectedVersion string
}

type publicBreadcrumb struct {
	Label   string
	URL     string
	Current bool
}

func newPublicNavigation(authLink, authLabel string, breadcrumbs ...publicBreadcrumb) publicNavigationData {
	return publicNavigationData{
		AuthLink:      authLink,
		AuthLabel:     authLabel,
		ModulesActive: true,
		Breadcrumbs:   breadcrumbs,
	}
}

func (r *Router) publicNavigation(req *http.Request, authLink, authLabel string, breadcrumbs ...publicBreadcrumb) publicNavigationData {
	navigation := newPublicNavigation(authLink, authLabel, breadcrumbs...)
	navigation.CSPNonce = cspNonce(req)
	return navigation
}

type manageLoginData struct {
	CSPNonce string
	Error    string
	HasOIDC  bool
}

type managePageData struct {
	Navigation manageNavigationData
	Principal  auth.Principal
	Teams      []string
	Owners     []string
	Modules    []manageModuleRow
	Message    string
	Error      string
	CSRFToken  string
	Query      string
	Filter     listFilterData
	Pagination paginationData
}

type manageOverviewData struct {
	Navigation  manageNavigationData
	Modules     []domain.Module
	ModuleTotal int
	ModulePages paginationData
	Spaces      []manageOverviewSpaceSummary
	SpaceTotal  int
	SpacePages  paginationData
	Teams       []manageOverviewTeamSummary
	TeamTotal   int
	TeamPages   paginationData
}

type manageOverviewSpaceSummary struct {
	Name        string
	ModuleCount int
	ModulesURL  string
}

type manageOverviewTeamSummary struct {
	Name        string
	ModuleCount int
	TeamURL     string
	ModulesURL  string
}

type manageAccessData struct {
	Navigation        manageNavigationData
	AdminOIDCGroups   string
	AdminOIDCEmails   string
	AdminOIDCSubjects string
	Message           string
	Error             string
	CSRFToken         string
}

type manageAccessAddTeamData struct {
	Navigation manageNavigationData
	CSRFToken  string
	Error      string
}

type managePublishSpacesData struct {
	Navigation  manageNavigationData
	Assignments []managePublishSpaceAssignment
	Teams       []string
	Query       string
	Filter      listFilterData
	Pagination  paginationData
	Message     string
	Error       string
	CSRFToken   string
}

type managePublishSpaceAssignment struct {
	Team        string
	TeamURL     string
	Space       string
	ModulesURL  string
	ModuleCount int
	Primary     bool
	Upstream    bool
}

type manageTokenCreatedData struct {
	Navigation manageNavigationData
	Token      string
	Name       string
	Prefix     string
	Team       string
	Kind       string
	Next       string
	CSRFToken  string
}

type manageTeamsData struct {
	Navigation manageNavigationData
	Principal  auth.Principal
	Teams      []manageTeamSummary
	Query      string
	Filter     listFilterData
	Pagination paginationData
	Message    string
	Error      string
	CSRFToken  string
}

type manageTeamData struct {
	Navigation             manageNavigationData
	Principal              auth.Principal
	Team                   accessTeamFormRow
	Spaces                 []string
	PublishSpaces          []string
	Modules                []manageModuleRow
	TokenHistory           []accessTokenFormRow
	TokenPagination        paginationData
	TokenHistoryPagination paginationData
	ActiveTokenQuery       string
	ActiveTokenFilter      listFilterData
	TokenHistoryAvailable  bool
	TokenHistoryQuery      string
	TokenHistoryFilter     listFilterData
	ShowAccess             bool
	ShowTokens             bool
	ShowModules            bool
	Message                string
	Error                  string
	CSRFToken              string
	Query                  string
	ModuleFilter           listFilterData
	Pagination             paginationData
}

type queryParameter struct {
	Name  string
	Value string
}

type manageNavigationData struct {
	CSPNonce            string
	CSRFToken           string
	CanTeams            bool
	CanAdmin            bool
	TeamPage            bool
	TeamURL             string
	OverviewActive      bool
	TeamAccessActive    bool
	TeamTokensActive    bool
	TeamModulesActive   bool
	ModulesActive       bool
	TeamsActive         bool
	AddTeamActive       bool
	PublishSpacesActive bool
	GlobalAccessActive  bool
	Breadcrumbs         []manageBreadcrumb
}

type manageBreadcrumb struct {
	Label string
	URL   string
}

func newManageNavigation(principal auth.Principal, csrfToken, page, team string) manageNavigationData {
	navigation := manageNavigationData{
		CSRFToken: csrfToken,
		CanTeams:  principal.CanAdmin || principal.CanManageTeam,
		CanAdmin:  principal.CanAdmin,
	}

	switch page {
	case "overview":
		navigation.OverviewActive = true
	case "modules":
		navigation.ModulesActive = true
		navigation.Breadcrumbs = []manageBreadcrumb{{Label: "Overview", URL: "/manage"}, {Label: "Modules"}}
	case "teams":
		navigation.TeamsActive = true
		navigation.Breadcrumbs = []manageBreadcrumb{{Label: "Overview", URL: "/manage"}, {Label: "Teams"}}
	case "team-access", "team-tokens", "team-modules":
		navigation.TeamsActive = true
		navigation.TeamPage = true
		navigation.TeamURL = "/manage/teams/" + url.PathEscape(team)
		section := strings.TrimPrefix(page, "team-")
		navigation.TeamAccessActive = section == "access"
		navigation.TeamTokensActive = section == "tokens"
		navigation.TeamModulesActive = section == "modules"
		navigation.Breadcrumbs = []manageBreadcrumb{
			{Label: "Overview", URL: "/manage"},
			{Label: "Teams", URL: "/manage/teams"},
			{Label: team, URL: navigation.TeamURL + "/access"},
			{Label: strings.ToUpper(section[:1]) + section[1:]},
		}
	case "add-team":
		navigation.TeamsActive = true
		navigation.AddTeamActive = true
		navigation.Breadcrumbs = []manageBreadcrumb{{Label: "Overview", URL: "/manage"}, {Label: "Teams", URL: "/manage/teams"}, {Label: "Add team"}}
	case "global-access":
		navigation.GlobalAccessActive = true
		navigation.Breadcrumbs = []manageBreadcrumb{{Label: "Overview", URL: "/manage"}, {Label: "Administration"}, {Label: "Global access"}}
	case "publish-spaces":
		navigation.PublishSpacesActive = true
		navigation.Breadcrumbs = []manageBreadcrumb{{Label: "Overview", URL: "/manage"}, {Label: "Administration"}, {Label: "Publish spaces"}}
	case "token":
		navigation.TeamsActive = true
		navigation.TeamPage = true
		navigation.TeamURL = "/manage/teams/" + url.PathEscape(team)
		navigation.TeamTokensActive = true
		navigation.Breadcrumbs = []manageBreadcrumb{{Label: "Overview", URL: "/manage"}, {Label: "Teams", URL: "/manage/teams"}, {Label: team, URL: "/manage/teams/" + url.PathEscape(team) + "/tokens"}, {Label: "New token"}}
	}
	return navigation
}

func (r *Router) manageNavigation(req *http.Request, principal auth.Principal, csrfToken, page, team string) manageNavigationData {
	navigation := newManageNavigation(principal, csrfToken, page, team)
	navigation.CSPNonce = cspNonce(req)
	return navigation
}

type manageTeamSummary struct {
	Team            string
	Spaces          []string
	ModuleCount     int
	ReadTokens      int
	PublishTokens   int
	PublishGroups   int
	TeamAdminUsers  int
	TeamAdminGroups int
}

type accessTeamFormRow struct {
	Team                string
	Tokens              []accessTokenFormRow
	TokenHistory        []accessTokenFormRow
	ExtraPublishSpaces  string
	OIDCGroups          string
	OIDCTeamAdminEmails string
	OIDCTeamAdminGroups string
	Badges              []string
}

type accessTokenFormRow struct {
	ID          string
	Kind        string
	Prefix      string
	Description string
	Status      string
	CreatedAt   string
	ExpiresAt   string
	ChangedAt   string
	Revoked     bool
	Active      bool
	sortAt      time.Time
}

func accessTokenFormRows(records []auth.AccessTokenRecord, kind string, now time.Time) []accessTokenFormRow {
	rows := make([]accessTokenFormRow, 0, len(records))
	for _, record := range records {
		status := "Active"
		active := true
		sortAt := record.CreatedAt
		if record.RevokedAt != nil {
			status = "Revoked"
			active = false
			sortAt = *record.RevokedAt
		} else if record.ExpiresAt != nil && !now.Before(*record.ExpiresAt) {
			status = "Expired"
			active = false
			sortAt = *record.ExpiresAt
		}
		expiresAt := "Never"
		if record.ExpiresAt != nil {
			expiresAt = record.ExpiresAt.UTC().Format("2006-01-02")
		}
		rows = append(rows, accessTokenFormRow{
			ID:          record.ID,
			Kind:        kind,
			Prefix:      record.Prefix,
			Description: record.Description,
			Status:      status,
			CreatedAt:   record.CreatedAt.UTC().Format("2006-01-02"),
			ExpiresAt:   expiresAt,
			ChangedAt:   sortAt.UTC().Format("2006-01-02"),
			Revoked:     record.RevokedAt != nil,
			Active:      active,
			sortAt:      sortAt,
		})
	}
	return rows
}

type manageModuleRow struct {
	Module    domain.Module
	Versions  []manageVersionRow
	CanDelete bool
}

type manageVersionRow struct {
	Version string
	Active  bool
	Latest  bool
}

type paginationData struct {
	Page           int
	Total          int
	TotalPages     int
	PageSize       int
	HasPrev        bool
	HasNext        bool
	PrevURL        string
	NextURL        string
	SizeAction     string
	SizeParam      string
	SizePageParam  string
	SizeAnchor     string
	SizeTarget     string
	SizeStorageKey string
	SizeCookieName string
	SizeParams     []queryParameter
	SizeOptions    []pageSizeOption
}

type pageSizeOption struct {
	Value    int
	Selected bool
}

var csrfFuncs = template.FuncMap{
	"csrfInput": func(token string) template.HTML {
		// #nosec G203 -- token is HTML-escaped before constructing the fixed hidden input element.
		return template.HTML(`<input type="hidden" name="csrf_token" value="` + template.HTMLEscapeString(token) + `">`)
	},
}

//go:embed templates/*.gohtml
var templateFS embed.FS

func mustParseTemplate(filename string) *template.Template {
	return template.Must(template.New(filename).ParseFS(templateFS, "templates/"+filename))
}

func mustParsePublicTemplate(filename string) *template.Template {
	return template.Must(template.New(filename).ParseFS(templateFS, "templates/public-navigation.gohtml", "templates/list-filter.gohtml", "templates/clipboard.gohtml", "templates/page-size-persistence.gohtml", "templates/async-lists.gohtml", "templates/"+filename))
}

func mustParseCSRFTemplate(filename string) *template.Template {
	return template.Must(template.New(filename).Funcs(csrfFuncs).ParseFS(templateFS, "templates/manage-navigation.gohtml", "templates/list-filter.gohtml", "templates/clipboard.gohtml", "templates/page-size-persistence.gohtml", "templates/async-lists.gohtml", "templates/"+filename))
}

var manageLoginTemplate = mustParseTemplate("manage-login.gohtml")

var managePageTemplate = mustParseCSRFTemplate("manage-page.gohtml")

var manageOverviewTemplate = mustParseCSRFTemplate("manage-overview.gohtml")

var manageAccessTemplate = mustParseCSRFTemplate("manage-access.gohtml")

var manageAccessAddTeamTemplate = mustParseCSRFTemplate("manage-access-add-team.gohtml")

var managePublishSpacesTemplate = mustParseCSRFTemplate("manage-publish-spaces.gohtml")

var manageTokenCreatedTemplate = mustParseCSRFTemplate("manage-token-created.gohtml")

var manageTeamsTemplate = mustParseCSRFTemplate("manage-teams.gohtml")

var manageTeamTemplate = mustParseCSRFTemplate("manage-team.gohtml")

var indexPageTemplate = mustParsePublicTemplate("index-page.gohtml")

var modulePageTemplate = mustParsePublicTemplate("module-page.gohtml")

func executeHTMLTemplate(w http.ResponseWriter, tmpl *template.Template, data any) {
	var body bytes.Buffer
	if err := tmpl.Execute(&body, data); err != nil {
		slog.Error("render html template", "request_id", w.Header().Get(requestIDHeader), "template", tmpl.Name(), "err", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if _, err := body.WriteTo(w); err != nil {
		slog.Warn("write html response", "request_id", w.Header().Get(requestIDHeader), "template", tmpl.Name(), "err", err)
	}
}
