package httpapi

import (
	"embed"
	"html/template"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

type modulePageData struct {
	Module          domain.Module
	Release         domain.Release
	Versions        []domain.ModuleVersion
	SelectedVersion string
	ReadmeHTML      template.HTML
	DownloadPath    string
	IsUpstream      bool
	PublicBaseURL   string
	ModuleInstallID string
	ReadTokenHint   string
}

type indexPageData struct {
	Modules   []domain.Module
	AuthLink  string
	AuthLabel string
}

type manageLoginData struct {
	Error   string
	HasOIDC bool
}

type managePageData struct {
	Principal  auth.Principal
	Owners     []string
	Modules    []manageModuleRow
	Message    string
	Error      string
	CSRFToken  string
	Query      string
	Pagination paginationData
}

type manageAccessData struct {
	ConfigJSON        string
	Teams             []accessTeamFormRow
	AdminOIDCGroups   string
	AdminOIDCEmails   string
	AdminOIDCSubjects string
	CanAdmin          bool
	Message           string
	Error             string
	CSRFToken         string
}

type manageAccessAddTeamData struct {
	CSRFToken string
	Error     string
}

type manageTeamsData struct {
	Principal auth.Principal
	Teams     []manageTeamSummary
	Message   string
	Error     string
	CSRFToken string
}

type manageTeamData struct {
	Principal     auth.Principal
	Team          accessTeamFormRow
	Spaces        []string
	PublishSpaces []string
	Modules       []manageModuleRow
	Message       string
	Error         string
	CSRFToken     string
	Query         string
	Pagination    paginationData
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
	ReadTokens          string
	PublishTokens       string
	ExtraPublishSpaces  string
	OIDCGroups          string
	OIDCTeamAdminEmails string
	OIDCTeamAdminGroups string
	Badges              []string
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
	Page       int
	Total      int
	TotalPages int
	HasPrev    bool
	HasNext    bool
	PrevURL    string
	NextURL    string
}

var csrfFuncs = template.FuncMap{
	"csrfInput": func(token string) template.HTML {
		return template.HTML(`<input type="hidden" name="csrf_token" value="` + template.HTMLEscapeString(token) + `">`)
	},
}

//go:embed templates/*.html
var templateFS embed.FS

func mustParseTemplate(filename string) *template.Template {
	return template.Must(template.New(filename).ParseFS(templateFS, "templates/"+filename))
}

func mustParseCSRFTemplate(filename string) *template.Template {
	return template.Must(template.New(filename).Funcs(csrfFuncs).ParseFS(templateFS, "templates/"+filename))
}

var manageLoginTemplate = mustParseTemplate("manage-login.html")

var managePageTemplate = mustParseCSRFTemplate("manage-page.html")

var manageAccessTemplate = mustParseCSRFTemplate("manage-access.html")

var manageAccessAddTeamTemplate = mustParseCSRFTemplate("manage-access-add-team.html")

var manageTeamsTemplate = mustParseCSRFTemplate("manage-teams.html")

var manageTeamTemplate = mustParseCSRFTemplate("manage-team.html")

var indexPageTemplate = mustParseTemplate("index-page.html")

var modulePageTemplate = mustParseTemplate("module-page.html")
