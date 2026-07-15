package httpapi

import (
	"embed"
	"html/template"

	"puppet-forge/internal/auth"
	"puppet-forge/internal/domain"
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
	Principal auth.Principal
	Owners    []string
	Modules   []manageModuleRow
	Message   string
	Error     string
	CSRFToken string
	Query     string
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

var indexPageTemplate = mustParseTemplate("index-page.html")

var modulePageTemplate = mustParseTemplate("module-page.html")
