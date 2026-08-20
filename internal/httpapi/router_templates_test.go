package httpapi

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
)

func TestPublicTemplatesRenderContextBreadcrumbs(t *testing.T) {
	t.Parallel()

	var index bytes.Buffer
	if err := indexPageTemplate.Execute(&index, indexPageData{Navigation: newPublicNavigation(
		"/manage",
		"Manage",
		publicBreadcrumb{Label: "Modules", Current: true},
	)}); err != nil {
		t.Fatalf("render index page: %v", err)
	}
	for _, want := range []string{
		`class="public-global-navigation"`,
		`<a href="/" aria-current="page">Modules</a>`,
		`<a href="/manage">Manage</a>`,
		`<span aria-current="page">Modules</span>`,
	} {
		if !strings.Contains(index.String(), want) {
			t.Fatalf("index page misses public navigation %q:\n%s", want, index.String())
		}
	}

	moduleNavigation := newPublicNavigation(
		"/manage",
		"Manage",
		publicBreadcrumb{Label: "Modules", URL: "/"},
		publicBreadcrumb{Label: "teamname", URL: "/?owner=teamname"},
		publicBreadcrumb{Label: "apache", URL: "/modules/teamname/apache"},
	)
	moduleNavigation.Versions = []domain.ModuleVersion{{Version: "2.0.0"}, {Version: "1.0.0"}}
	moduleNavigation.SelectedVersion = "2.0.0"

	var module bytes.Buffer
	if err := modulePageTemplate.Execute(&module, modulePageData{
		Navigation: moduleNavigation,
		Module:     domain.Module{Owner: "teamname", Name: "apache"},
		Release:    domain.Release{Version: "2.0.0"},
	}); err != nil {
		t.Fatalf("render module page: %v", err)
	}
	for _, want := range []string{
		`class="public-global-navigation"`,
		`<a href="/" aria-current="page">Modules</a>`,
		`<a href="/manage">Manage</a>`,
		`<span><a href="/">Modules</a></span>`,
		`<span><a href="/?owner=teamname">teamname</a></span>`,
		`<span><a href="/modules/teamname/apache">apache</a></span>`,
		`<select id="version-select" aria-label="Module version"><option value="2.0.0" selected>2.0.0</option><option value="1.0.0">1.0.0</option></select>`,
		`<button id="copy-version-link" class="public-version-link-copy" type="button" title="Copy link to this version" aria-label="Copy link to this version">`,
		`<nav class="section-links" aria-label="Module sections">`,
		`<a href="#readme">README</a>`,
		`<a href="#install">Install</a>`,
		`<section class="panel" id="readme">`,
		`<details class="panel install-panel" id="install">`,
		`<summary>Install</summary>`,
		`if (installPanel) installPanel.open = true`,
	} {
		if !strings.Contains(module.String(), want) {
			t.Fatalf("module page misses public navigation %q:\n%s", want, module.String())
		}
	}
	if strings.Count(module.String(), `<a href="/modules/teamname/apache">apache</a>`) != 1 {
		t.Fatalf("module page must expose one canonical module breadcrumb:\n%s", module.String())
	}
	if strings.Contains(module.String(), "Back to modules") || strings.Contains(module.String(), `class="back"`) {
		t.Fatalf("module page duplicates catalog navigation outside the breadcrumb:\n%s", module.String())
	}
	if strings.Contains(module.String(), `class="versionbar"`) {
		t.Fatalf("module page duplicates version navigation in the hero:\n%s", module.String())
	}
	if strings.Contains(module.String(), `<details class="panel install-panel" id="install" open>`) {
		t.Fatalf("install panel must be collapsed by default:\n%s", module.String())
	}
}

func TestPageSizeSelectionPersistsAcrossNavigation(t *testing.T) {
	t.Parallel()

	pagination := paginationData{Page: 1, Total: 40, TotalPages: 2, PageSize: 20, HasNext: true}
	pagination = configurePageSize(pagination, url.Values{"q": {"apache"}}, "/", "per_page", "page", "module-list", "module-list")

	var page bytes.Buffer
	if err := indexPageTemplate.Execute(&page, indexPageData{
		Navigation: newPublicNavigation("/manage", "Manage"),
		Modules:    []domain.Module{{Owner: "teamname", Name: "apache", LatestVersion: "1.0.0"}},
		Pagination: pagination,
	}); err != nil {
		t.Fatalf("render index page: %v", err)
	}

	body := page.String()
	for _, want := range []string{
		`data-page-size-key="public-modules:per_page:module-list"`,
		`data-page-size-cookie="puppet_forge_page_size_public-modules_per_page_module-list"`,
		`data-page-size-param="per_page"`,
		`data-page-param="page"`,
		`data-rendered-page-size="20"`,
		`typeof value === "string" && allowedPageSizes.has(value)`,
		`const rendered = form.dataset.renderedPageSize`,
		`preferred !== rendered && cookieStored`,
		`document.cookie =`,
		`return readCookie(name) === value`,
		`window.localStorage.getItem(storagePrefix + key)`,
		`url.searchParams.delete(form.dataset.pageParam)`,
		`url.searchParams.delete(form.dataset.pageSizeParam)`,
		`data-async-list`,
		`current.replaceWith(replacement)`,
		`window.location.assign(url)`,
		`.pagination-links a:hover`,
		`box-shadow: 0 0 0 3px #dceced`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("index page misses persistent page-size behavior %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `if (form.dataset.asyncFilter)`) {
		t.Fatalf("page-size changes must not leave asynchronously rendered pagination links behind:\n%s", body)
	}
	if strings.Contains(body, `preferred !== select.value`) {
		t.Fatalf("restored form state must not be mistaken for the server-rendered page size:\n%s", body)
	}

	manageNavigation := newManageNavigation(auth.Principal{CanAdmin: true}, "csrf", "overview", "")
	manageNavigation.CSPNonce = "manage-page-size-nonce"
	var managePage bytes.Buffer
	if err := manageOverviewTemplate.Execute(&managePage, manageOverviewData{
		Navigation:  manageNavigation,
		ModulePages: pagination,
	}); err != nil {
		t.Fatalf("render manage overview: %v", err)
	}
	manageBody := managePage.String()
	if strings.Contains(manageBody, `<script nonce="">`) {
		t.Fatalf("manage page renders a script without its CSP nonce:\n%s", manageBody)
	}
	if !strings.Contains(manageBody, `<script nonce="manage-page-size-nonce">`) ||
		!strings.Contains(manageBody, `document.cookie =`) ||
		!strings.Contains(manageBody, `data-rendered-page-size="20"`) ||
		!strings.Contains(manageBody, `.pagination-links a:hover`) ||
		!strings.Contains(manageBody, `.list-pagination-links a:hover`) ||
		!strings.Contains(manageBody, `.module-details > summary.catalog-item`) ||
		!strings.Contains(manageBody, `.module-details > .release-list`) {
		t.Fatalf("manage page misses nonce-authorized page-size persistence:\n%s", manageBody)
	}
}

func TestPaginatedViewsUseSharedAsyncListContract(t *testing.T) {
	t.Parallel()

	navigation := newManageNavigation(auth.Principal{CanAdmin: true}, "csrf", "overview", "")
	render := func(tmpl *template.Template, data any) string {
		t.Helper()
		var page bytes.Buffer
		if err := tmpl.Execute(&page, data); err != nil {
			t.Fatalf("render paginated template: %v", err)
		}
		return page.String()
	}

	pages := []struct {
		body    string
		targets []string
	}{
		{render(manageOverviewTemplate, manageOverviewData{Navigation: navigation}), []string{"modules", "teams", "spaces"}},
		{render(managePageTemplate, managePageData{
			Navigation: navigation,
			Modules:    []manageModuleRow{{Module: domain.Module{Owner: "teamname", Name: "apache"}}},
		}), []string{"manage-module-list"}},
		{render(manageTeamTemplate, manageTeamData{
			Navigation: navigation, Principal: auth.Principal{CanAdmin: true}, Team: accessTeamFormRow{Team: "teamname"},
			ShowTokens: true, TokenHistoryAvailable: true,
		}), []string{"active-tokens", "token-history"}},
		{render(manageTeamTemplate, manageTeamData{
			Navigation: navigation, Principal: auth.Principal{CanAdmin: true}, Team: accessTeamFormRow{Team: "teamname"}, ShowModules: true,
			Modules: []manageModuleRow{{Module: domain.Module{Owner: "teamname", Name: "apache"}}},
		}), []string{"module-list"}},
	}
	for _, page := range pages {
		for _, target := range page.targets {
			if !strings.Contains(page.body, `id="`+target+`"`) || !strings.Contains(page.body, `data-async-list`) {
				t.Fatalf("paginated target %q misses the shared async-list boundary:\n%s", target, page.body)
			}
		}
	}
	if !strings.Contains(pages[1].body, `class="module-details"`) ||
		!strings.Contains(pages[3].body, `class="module-details"`) {
		t.Fatal("global and team module lists must use the shared module-card structure")
	}

	for _, want := range []string{
		`const refreshList = async (targetID, requestURL, options = {}) =>`,
		`const url = requestURL.toString();`,
		`if (requests.get(targetID) !== request) return;`,
		`const responseText = await response.text();`,
		`if (requests.get(targetID) === request)`,
		`Promise.all(ids.map((id) => refreshList(id, new URL(window.location.href))))`,
		`current.replaceWith(replacement)`,
		`document.addEventListener("submit"`,
		`document.addEventListener("input"`,
		`document.addEventListener("click"`,
		`document.addEventListener("change"`,
		`window.addEventListener("popstate"`,
		`window.addEventListener("pageshow"`,
	} {
		if !strings.Contains(pages[0].body, want) {
			t.Fatalf("shared async-list controller misses %q:\n%s", want, pages[0].body)
		}
	}
}

func TestManageNavigationRoleMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		principal auth.Principal
		page      string
		team      string
		present   []string
		absent    []string
	}{
		{
			name:      "publisher",
			principal: auth.Principal{CanPublish: true, PublishOwners: map[string]struct{}{"teamname": {}}},
			page:      "modules",
			present:   []string{`href="/manage/modules" aria-current="page">Modules</a>`},
			absent:    []string{`href="/manage/teams"`, `aria-label="Management context navigation"`},
		},
		{
			name: "team admin",
			principal: auth.Principal{
				CanManageTeam: true,
				ManagedTeams:  map[string]struct{}{"teamname": {}},
			},
			page:    "team-tokens",
			team:    "teamname",
			present: []string{`href="/manage/teams" aria-current="page">Teams</a>`, `aria-label="Management context navigation"`, `href="/manage/teams/teamname/tokens" aria-current="page">Tokens</a>`, `<span aria-current="page">Tokens</span>`},
			absent:  []string{`>Add team</a>`, `>Publish spaces</a>`, `>Global access</a>`},
		},
		{
			name:      "global admin",
			principal: auth.Principal{CanAdmin: true},
			page:      "teams",
			present:   []string{`href="/manage/teams" aria-current="page">Teams</a>`, `href="/manage/teams/new">Add team</a>`, `<details class="administration-menu">`, `<summary>Administration</summary>`, `href="/manage/admin/spaces">Publish spaces</a>`, `href="/manage/admin/access">Global access</a>`},
		},
		{
			name: "combined global and team admin",
			principal: auth.Principal{
				CanAdmin:      true,
				CanManageTeam: true,
				ManagedTeams:  map[string]struct{}{"teamname": {}},
				CanPublish:    true,
				PublishOwners: map[string]struct{}{"teamname": {}},
			},
			page:    "team-modules",
			team:    "teamname",
			present: []string{`href="/manage/teams" aria-current="page">Teams</a>`, `<details class="administration-menu">`, `<summary>Administration</summary>`, `href="/manage/admin/spaces">Publish spaces</a>`, `href="/manage/admin/access">Global access</a>`, `href="/manage/teams/teamname/modules" aria-current="page">Modules</a>`, `<span aria-current="page">Modules</span>`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var page bytes.Buffer
			err := manageTeamsTemplate.Execute(&page, manageTeamsData{
				Navigation: newManageNavigation(tc.principal, "csrf", tc.page, tc.team),
				Principal:  tc.principal,
				CSRFToken:  "csrf",
			})
			if err != nil {
				t.Fatalf("render manage navigation: %v", err)
			}
			body := page.String()
			for _, want := range tc.present {
				if !strings.Contains(body, want) {
					t.Errorf("navigation misses %q:\n%s", want, body)
				}
			}
			for _, forbidden := range tc.absent {
				if strings.Contains(body, forbidden) {
					t.Errorf("navigation exposes %q:\n%s", forbidden, body)
				}
			}
		})
	}
}

func TestManageAdministrationDropdownMarksCurrentSection(t *testing.T) {
	t.Parallel()

	var page bytes.Buffer
	err := manageTeamsTemplate.Execute(&page, manageTeamsData{
		Navigation: newManageNavigation(auth.Principal{CanAdmin: true}, "csrf", "publish-spaces", ""),
		Principal:  auth.Principal{CanAdmin: true},
		CSRFToken:  "csrf",
	})
	if err != nil {
		t.Fatalf("render administration navigation: %v", err)
	}
	body := page.String()
	for _, want := range []string{
		`<details class="administration-menu active">`,
		`<summary>Administration</summary>`,
		`href="/manage/admin/spaces" aria-current="page">Publish spaces</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("administration dropdown misses %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `aria-label="Management context navigation"`) {
		t.Fatalf("administration page renders an empty context navigation row:\n%s", body)
	}
}

func TestManageModulesShowsAllManagedTeams(t *testing.T) {
	t.Parallel()

	principal := auth.Principal{
		Team:          "alpha",
		CanRead:       true,
		CanPublish:    true,
		CanManageTeam: true,
		ManagedTeams:  map[string]struct{}{"beta": {}, "alpha": {}},
		PublishOwners: map[string]struct{}{"shared": {}, "beta": {}, "alpha": {}},
	}
	teams := manageableTeams(principal)
	owners := manageableOwners(principal)
	if got := strings.Join(teams, ","); got != "alpha,beta" {
		t.Fatalf("manageableTeams() = %q", got)
	}

	var page bytes.Buffer
	err := managePageTemplate.Execute(&page, managePageData{
		Navigation: newManageNavigation(principal, "csrf", "modules", ""),
		Principal:  principal,
		Teams:      teams,
		Owners:     owners,
		CSRFToken:  "csrf",
	})
	if err != nil {
		t.Fatalf("render manage modules: %v", err)
	}
	rendered := page.String()
	if strings.Contains(rendered, "\n    details {\n      margin-top: 14px;") {
		t.Fatalf("manage modules applies content spacing to the Administration details element:\n%s", rendered)
	}
	if !strings.Contains(rendered, ".module-details {\n      margin-top: 14px;") {
		t.Fatalf("manage modules does not scope details spacing to content disclosures:\n%s", rendered)
	}
	if !strings.Contains(rendered, ".primary {\n      height: 42px;\n      margin-top: 16px;") {
		t.Fatalf("manage module form actions do not keep vertical spacing from their fields:\n%s", rendered)
	}
	body := strings.Join(strings.Fields(rendered), " ")
	if !strings.Contains(body, "Teams: alpha, beta · spaces: alpha, beta, shared") {
		t.Fatalf("manage modules page does not show all teams and spaces:\n%s", body)
	}

	principal.CanAdmin = true
	var combinedPage bytes.Buffer
	err = managePageTemplate.Execute(&combinedPage, managePageData{
		Navigation: newManageNavigation(principal, "csrf", "modules", ""),
		Principal:  principal,
		Teams:      manageableTeams(principal),
		Owners:     owners,
		CSRFToken:  "csrf",
	})
	if err != nil {
		t.Fatalf("render combined-role manage modules: %v", err)
	}
	combinedBody := strings.Join(strings.Fields(combinedPage.String()), " ")
	if !strings.Contains(combinedBody, "Global administrator · Teams: alpha, beta · spaces: alpha, beta, shared") {
		t.Fatalf("combined role loses team context:\n%s", combinedBody)
	}

	principal.ManagedTeams = nil
	principal.Team = auth.GlobalAdminTeam
	if teams := manageableTeams(principal); len(teams) != 0 {
		t.Fatalf("pure global admin exposes a synthetic team: %#v", teams)
	}
}

func TestManageBreadcrumbLinksHaveInteractiveStates(t *testing.T) {
	t.Parallel()

	var page bytes.Buffer
	err := manageTeamsTemplate.Execute(&page, manageTeamsData{
		Navigation: newManageNavigation(
			auth.Principal{CanManageTeam: true, ManagedTeams: map[string]struct{}{"teamname": {}}},
			"csrf",
			"team-access",
			"teamname",
		),
		CSRFToken: "csrf",
	})
	if err != nil {
		t.Fatalf("render manage breadcrumbs: %v", err)
	}
	body := page.String()
	for _, want := range []string{
		`.breadcrumbs a:hover,`,
		`.breadcrumbs a:focus-visible`,
		`background: rgba(47, 111, 115, 0.11);`,
		`<span><a href="/manage">Overview</a></span>`,
		`<span><a href="/manage/teams/teamname/access">teamname</a></span>`,
		`<span aria-current="page">Access</span>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("manage breadcrumbs miss %q:\n%s", want, body)
		}
	}
}

func TestManageActionNoticesAutoDismissAccessibly(t *testing.T) {
	t.Parallel()

	var page bytes.Buffer
	err := managePageTemplate.Execute(&page, managePageData{
		Navigation: newManageNavigation(auth.Principal{CanAdmin: true}, "csrf", "modules", ""),
		Principal:  auth.Principal{CanAdmin: true},
		Message:    "module published",
		Error:      "module rejected",
		CSRFToken:  "csrf",
	})
	if err != nil {
		t.Fatalf("render management notices: %v", err)
	}
	body := page.String()
	for _, want := range []string{
		`class="notice ok" role="status"`,
		`class="notice error" role="alert"`,
		`notice.classList.contains("error") ? 12000 : 6000`,
		`currentURL.searchParams.delete("message")`,
		`currentURL.searchParams.delete("error")`,
		`dismissButton.setAttribute("aria-label", "Dismiss notification")`,
		`notice.addEventListener("mouseenter", pause)`,
		`document.addEventListener("visibilitychange"`,
		`@media (prefers-reduced-motion: reduce)`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("management notice behavior misses %q:\n%s", want, body)
		}
	}

	for _, templateName := range []string{
		"manage-access-add-team.gohtml",
		"manage-access.gohtml",
		"manage-page.gohtml",
		"manage-publish-spaces.gohtml",
		"manage-team.gohtml",
		"manage-teams.gohtml",
	} {
		contents, readErr := templateFS.ReadFile("templates/" + templateName)
		if readErr != nil {
			t.Fatalf("read %s: %v", templateName, readErr)
		}
		if !strings.Contains(string(contents), `class="notice error" role="alert"`) {
			t.Errorf("%s does not expose errors as alerts", templateName)
		}
	}
}

func TestAuthenticatedManagePagesUseConsistentWidth(t *testing.T) {
	t.Parallel()

	pages := []string{
		"manage-overview.gohtml",
		"manage-page.gohtml",
		"manage-teams.gohtml",
		"manage-team.gohtml",
		"manage-access-add-team.gohtml",
		"manage-access.gohtml",
		"manage-publish-spaces.gohtml",
		"manage-token-created.gohtml",
	}
	for _, page := range pages {
		contents, err := templateFS.ReadFile("templates/" + page)
		if err != nil {
			t.Fatalf("read %s: %v", page, err)
		}
		if !strings.Contains(string(contents), "max-width: 1080px") {
			t.Errorf("%s does not use the shared management page width", page)
		}
	}
}

func TestAsyncFiltersDoNotUseExceptionsForExpectedFallbacks(t *testing.T) {
	t.Parallel()

	for _, templateName := range []string{"index-page.gohtml", "manage-navigation.gohtml"} {
		contents, err := templateFS.ReadFile("templates/" + templateName)
		if err != nil {
			t.Fatalf("read %s: %v", templateName, err)
		}
		if strings.Contains(string(contents), "throw new Error") {
			t.Errorf("%s uses exceptions for an expected navigation fallback", templateName)
		}
	}
}

func TestManageTeamSectionsRenderAsSeparatePages(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		page        string
		showAccess  bool
		showTokens  bool
		showModules bool
		present     string
		absent      []string
	}{
		{name: "access", page: "team-access", showAccess: true, present: `id="team-access"`, absent: []string{`id="team-tokens"`, `id="team-modules"`}},
		{name: "tokens", page: "team-tokens", showTokens: true, present: `id="team-tokens"`, absent: []string{`id="team-access"`, `id="team-modules"`}},
		{name: "modules", page: "team-modules", showModules: true, present: `id="team-modules"`, absent: []string{`id="team-access"`, `id="team-tokens"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var page bytes.Buffer
			err := manageTeamTemplate.Execute(&page, manageTeamData{
				Navigation:    newManageNavigation(auth.Principal{CanAdmin: true}, "csrf", tc.page, "teamname"),
				Principal:     auth.Principal{CanAdmin: true},
				Team:          accessTeamFormRow{Team: "teamname"},
				Spaces:        []string{"teamname"},
				PublishSpaces: []string{"teamname"},
				ShowAccess:    tc.showAccess,
				ShowTokens:    tc.showTokens,
				ShowModules:   tc.showModules,
				CSRFToken:     "csrf",
			})
			if err != nil {
				t.Fatalf("render team %s page: %v", tc.name, err)
			}
			body := page.String()
			if !strings.Contains(body, tc.present) {
				t.Fatalf("team %s page misses %q:\n%s", tc.name, tc.present, body)
			}
			for _, forbidden := range tc.absent {
				if strings.Contains(body, forbidden) {
					t.Fatalf("team %s page exposes %q:\n%s", tc.name, forbidden, body)
				}
			}
		})
	}
}

func TestTeamAdminCanDiscoverAndManageOwnTeamTokens(t *testing.T) {
	t.Parallel()

	principal := auth.Principal{
		Team:          "teamname",
		CanManageTeam: true,
		ManagedTeams:  map[string]struct{}{"teamname": {}},
	}
	var page bytes.Buffer
	err := manageTeamTemplate.Execute(&page, manageTeamData{
		Navigation: newManageNavigation(principal, "csrf", "team-tokens", "teamname"),
		Principal:  principal,
		Team: accessTeamFormRow{
			Team: "teamname",
			Tokens: []accessTokenFormRow{
				{ID: "read-id", Kind: "Read", Prefix: "pf_read_example", Description: "Production r10k"},
				{ID: "publish-id", Kind: "Publish", Prefix: "pf_publish_example", Description: "Release CI"},
			},
		},
		CSRFToken:  "csrf",
		ShowTokens: true,
	})
	if err != nil {
		t.Fatalf("render delegated team management: %v", err)
	}

	body := page.String()
	for _, want := range []string{
		`href="/manage/teams" aria-current="page">Teams</a>`,
		`href="/manage/teams/teamname/tokens" aria-current="page">Tokens</a>`,
		`<section id="team-tokens" class="panel">`,
		`action="/manage/access/token"`,
		`name="name" required`,
		`name="role" required`,
		`<option value="read">Read</option>`,
		`<option value="publish">Publish</option>`,
		`Production r10k`,
		`Release CI`,
		`name="action" value="create"`,
		`name="action" value="rotate"`,
		`name="action" value="revoke"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("delegated team page misses token control %q:\n%s", want, body)
		}
	}
	if strings.Count(body, `class="token-create"`) != 1 {
		t.Fatalf("delegated team page renders more than one token creation form:\n%s", body)
	}
	for _, forbidden := range []string{`>Add team</a>`, `>Global access</a>`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("delegated team page exposes global control %q:\n%s", forbidden, body)
		}
	}
}

func TestAccessTeamFormRowsSeparateActiveTokensFromHistory(t *testing.T) {
	t.Parallel()

	now := time.Date(2020, time.January, 2, 12, 0, 0, 0, time.UTC)
	expiredAt := now.Add(-time.Hour)
	revokedAt := now.Add(time.Minute)
	future := time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC)
	rows := accessTeamFormRows([]auth.TeamConfig{{
		Team: "teamname",
		ReadTokenRecords: []auth.AccessTokenRecord{
			{ID: "active-read", Prefix: "active-read", Digest: "digest-1", CreatedAt: now.Add(-time.Hour)},
			{ID: "expired-read", Prefix: "expired-read", Digest: "digest-2", CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: &expiredAt},
		},
		PublishTokenRecords: []auth.AccessTokenRecord{
			{ID: "active-publish", Prefix: "active-publish", Digest: "digest-3", CreatedAt: now, ExpiresAt: &future},
			{ID: "revoked-publish", Prefix: "revoked-publish", Digest: "digest-4", CreatedAt: now.Add(-3 * time.Hour), RevokedAt: &revokedAt},
		},
	}}, auth.Principal{CanAdmin: true})
	if len(rows) != 1 || len(rows[0].Tokens) != 2 || len(rows[0].TokenHistory) != 2 {
		t.Fatalf("unexpected active/history partition: %#v", rows)
	}
	if rows[0].Tokens[0].ID != "active-publish" || rows[0].Tokens[1].ID != "active-read" {
		t.Fatalf("active tokens are not newest-first across roles: %#v", rows[0].Tokens)
	}
	if rows[0].TokenHistory[0].ID != "revoked-publish" || rows[0].TokenHistory[1].ID != "expired-read" {
		t.Fatalf("token history is not newest-first: %#v", rows[0].TokenHistory)
	}
}

func TestParseManageTeamPathSeparatesTeamPages(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		path      string
		team      string
		section   manageTeamSection
		canonical bool
		ok        bool
	}{
		{path: "/manage/teams/teamname", team: "teamname", section: manageTeamAccessSection, ok: true},
		{path: "/manage/teams/teamname/access", team: "teamname", section: manageTeamAccessSection, canonical: true, ok: true},
		{path: "/manage/teams/teamname/tokens", team: "teamname", section: manageTeamTokensSection, canonical: true, ok: true},
		{path: "/manage/teams/teamname/modules", team: "teamname", section: manageTeamModulesSection, canonical: true, ok: true},
		{path: "/manage/teams/teamname/unknown"},
		{path: "/manage/teams/teamname/modules/extra"},
	} {
		team, section, canonical, ok := parseManageTeamPath(tc.path)
		if team != tc.team || section != tc.section || canonical != tc.canonical || ok != tc.ok {
			t.Errorf("parseManageTeamPath(%q) = (%q, %q, %t, %t)", tc.path, team, section, canonical, ok)
		}
	}
}

func TestTokenHistoryPaginationPreservesTeamPageState(t *testing.T) {
	t.Parallel()

	query := url.Values{"q": {"apache"}, "page": {"2"}, "token_page": {"2"}, "history_page": {"2"}, "token_query": {"revoked deploy"}}
	pagination := tokenPagination("/manage/teams/teamname/tokens", query, "history_page", "history_per_page", "token-history", 2, 20, 70)
	if pagination.TotalPages != 4 || !pagination.HasPrev || !pagination.HasNext {
		t.Fatalf("unexpected token history pagination: %#v", pagination)
	}
	if pagination.PrevURL != "/manage/teams/teamname/tokens?page=2&q=apache&token_page=2&token_query=revoked+deploy#token-history" {
		t.Fatalf("PrevURL = %q", pagination.PrevURL)
	}
	if pagination.NextURL != "/manage/teams/teamname/tokens?history_page=3&page=2&q=apache&token_page=2&token_query=revoked+deploy#token-history" {
		t.Fatalf("NextURL = %q", pagination.NextURL)
	}
}

func TestFilterTokensMatchesVisibleMetadata(t *testing.T) {
	t.Parallel()

	history := []accessTokenFormRow{
		{ID: "read", Kind: "Read", Status: "Expired", Prefix: "pf_read_ci", Description: "Production r10k"},
		{ID: "publish", Kind: "Publish", Status: "Revoked", Prefix: "pf_publish_release", Description: "Release pipeline"},
	}
	for query, wantID := range map[string]string{
		" READ ":     "read",
		"expired":    "read",
		"R10K":       "read",
		"pf_publish": "publish",
		"revoked":    "publish",
	} {
		filtered := filterTokens(history, query)
		if len(filtered) != 1 || filtered[0].ID != wantID {
			t.Fatalf("filterTokens(%q) = %#v, want %q", query, filtered, wantID)
		}
	}
	if filtered := filterTokens(history, "missing"); len(filtered) != 0 {
		t.Fatalf("missing filter returned %#v", filtered)
	}
	if filtered := filterTokens(history, "  "); len(filtered) != len(history) {
		t.Fatalf("blank filter returned %#v", filtered)
	}
}

func TestManageTeamActiveTokenFilterRemainsUsableWithoutMatches(t *testing.T) {
	t.Parallel()

	var page bytes.Buffer
	err := manageTeamTemplate.Execute(&page, manageTeamData{
		Navigation:       newManageNavigation(auth.Principal{CanAdmin: true}, "csrf", "team-tokens", "teamname"),
		Principal:        auth.Principal{CanAdmin: true},
		Team:             accessTeamFormRow{Team: "teamname"},
		ActiveTokenQuery: "missing token",
		ActiveTokenFilter: listFilterData{
			ID: "active-token-filter", InputID: "active-token-query", Action: "/manage/teams/teamname/tokens#active-tokens", Target: "active-tokens",
			Label: "active tokens", Name: "active_token_query", Placeholder: "Filter tokens", Value: "missing token",
			ClearURL: "/manage/teams/teamname/tokens?token_query=revoked#active-tokens", Params: []queryParameter{{Name: "token_query", Value: "revoked"}},
		},
		ShowTokens: true,
	})
	if err != nil {
		t.Fatalf("render filtered active tokens: %v", err)
	}
	body := page.String()
	for _, want := range []string{
		`data-section-key="active-tokens" data-async-list`,
		`action="/manage/teams/teamname/tokens#active-tokens" data-async-filter="active-tokens"`,
		`name="token_query" value="revoked"`,
		`name="active_token_query" type="search"`,
		`value="missing token"`,
		`href="/manage/teams/teamname/tokens?token_query=revoked#active-tokens"`,
		`No active tokens match this filter.`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("filtered active tokens miss %q:\n%s", want, body)
		}
	}
}

func TestPopulateManageTeamTokensFiltersActiveTokensBeforePagination(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/manage/teams/teamname/tokens?active_token_query=release&token_query=expired", nil)
	data := manageTeamData{Team: accessTeamFormRow{
		Tokens: []accessTokenFormRow{
			{ID: "read", Kind: "Read", Prefix: "pf_read_ci", Description: "Production r10k"},
			{ID: "publish", Kind: "Publish", Prefix: "pf_publish_release", Description: "Release pipeline"},
		},
		TokenHistory: []accessTokenFormRow{{ID: "expired", Kind: "Read", Status: "Expired"}},
	}}
	if redirected, err := populateManageTeamTokens(httptest.NewRecorder(), req, "/manage/teams/teamname/tokens", &data); err != nil {
		t.Fatalf("populateManageTeamTokens() error = %v", err)
	} else if redirected {
		t.Fatal("populateManageTeamTokens() redirected unexpectedly")
	}
	if len(data.Team.Tokens) != 1 || data.Team.Tokens[0].ID != "publish" || data.TokenPagination.Total != 1 {
		t.Fatalf("active token filter result = %#v, pagination = %#v", data.Team.Tokens, data.TokenPagination)
	}
	if data.ActiveTokenQuery != "release" || data.ActiveTokenFilter.ClearURL != "/manage/teams/teamname/tokens?token_query=expired#active-tokens" {
		t.Fatalf("active token filter state = query %q, clear URL %q", data.ActiveTokenQuery, data.ActiveTokenFilter.ClearURL)
	}
}

func TestManageTeamTokenHistoryFilterRemainsUsableWithoutMatches(t *testing.T) {
	t.Parallel()

	var page bytes.Buffer
	err := manageTeamTemplate.Execute(&page, manageTeamData{
		Navigation:            newManageNavigation(auth.Principal{CanAdmin: true}, "csrf", "team-tokens", "teamname"),
		Principal:             auth.Principal{CanAdmin: true},
		Team:                  accessTeamFormRow{Team: "teamname"},
		TokenHistoryAvailable: true,
		TokenHistoryQuery:     "missing token",
		TokenHistoryFilter: listFilterData{
			ID: "token-history-filter", InputID: "token-history-query", Action: "/manage/teams/teamname/tokens#token-history", Target: "token-history",
			Label: "revoked and expired tokens", Name: "token_query", Placeholder: "Filter tokens", Value: "missing token",
			ClearURL: "/manage/teams/teamname/tokens?q=apache#token-history", Params: []queryParameter{{Name: "q", Value: "apache"}},
		},
		ShowTokens: true,
	})
	if err != nil {
		t.Fatalf("render filtered token history: %v", err)
	}
	body := page.String()
	for _, want := range []string{
		`data-section-key="token-history" data-async-list open`,
		`action="/manage/teams/teamname/tokens#token-history" data-async-filter="token-history"`,
		`Revoked and expired tokens`,
		`name="q" value="apache"`,
		`name="token_query" type="search"`,
		`value="missing token"`,
		`href="/manage/teams/teamname/tokens?q=apache#token-history"`,
		`No revoked or expired tokens match this filter.`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("filtered token history misses %q:\n%s", want, body)
		}
	}
}

func TestManageTeamTokenFiltersSubmitAfterTypingPause(t *testing.T) {
	t.Parallel()

	var page bytes.Buffer
	err := manageTeamTemplate.Execute(&page, manageTeamData{
		Navigation:            newManageNavigation(auth.Principal{CanAdmin: true}, "csrf", "team-tokens", "teamname"),
		Principal:             auth.Principal{CanAdmin: true},
		Team:                  accessTeamFormRow{Team: "teamname"},
		TokenHistoryAvailable: true,
		ShowTokens:            true,
	})
	if err != nil {
		t.Fatalf("render token filters: %v", err)
	}
	body := page.String()
	for _, want := range []string{
		`data-async-list`,
		`const targetForForm = (form) => form.dataset.asyncFilter`,
		`document.addEventListener("input"`,
		`inputTimers.set(form, window.setTimeout(() =>`,
		`requests.get(targetForForm(form))?.abort()`,
		`const response = await window.fetch(url`,
		`current.replaceWith(replacement)`,
		`input.focus({ preventScroll: true })`,
		`event.preventDefault()`,
		`refreshList(targetForForm(form), formURL(form)`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("token filters miss automatic submission hook %q:\n%s", want, body)
		}
	}
}

func TestPaginateTokenHistoryBoundsLargeHistory(t *testing.T) {
	t.Parallel()

	history := make([]accessTokenFormRow, 300)
	for i := range history {
		history[i].ID = fmt.Sprintf("token-%03d", i)
	}
	first := pageItems(history, 1, 20)
	last := pageItems(history, 15, 20)
	if len(first) != 20 || first[0].ID != "token-000" || first[19].ID != "token-019" {
		t.Fatalf("unexpected first token history page: %#v", first)
	}
	if len(last) != 20 || last[0].ID != "token-280" || last[19].ID != "token-299" {
		t.Fatalf("unexpected last token history page: %#v", last)
	}
}

func TestCreatedTokenCopyMatchesModuleCodeCopyBehavior(t *testing.T) {
	t.Parallel()

	var page bytes.Buffer
	err := manageTokenCreatedTemplate.Execute(&page, manageTokenCreatedData{
		Navigation: newManageNavigation(auth.Principal{CanAdmin: true}, "csrf", "token", "teamname"),
		Token:      "pf_read_secret",
		Prefix:     "pf_read_",
		Team:       "teamname",
		Kind:       "Read",
		Next:       "/manage/teams/teamname/tokens",
	})
	if err != nil {
		t.Fatalf("render created token page: %v", err)
	}
	body := page.String()
	for _, want := range []string{
		`window['copyTextToClipboard'] = async (text)`,
		`navigator.clipboard.writeText(text)`,
		`document.execCommand('copy')`,
		`setCopyStatus(await copyText(value) ? 'Copied' : 'Failed')`,
		`}, 1400);`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("created token page misses copy behavior %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "window.prompt") {
		t.Fatalf("created token copy opens an extra prompt:\n%s", body)
	}
}

func TestModuleCopySupportsSecureAndHTTPOrigins(t *testing.T) {
	t.Parallel()

	var page bytes.Buffer
	err := modulePageTemplate.Execute(&page, modulePageData{
		Navigation: publicNavigationData{
			Versions:        []domain.ModuleVersion{{Version: "1.0.0"}},
			SelectedVersion: "1.0.0",
		},
		Module:  domain.Module{Owner: "teamname", Name: "apache"},
		Release: domain.Release{Version: "1.0.0"},
	})
	if err != nil {
		t.Fatalf("render module page: %v", err)
	}
	body := page.String()
	for _, want := range []string{
		`navigator.clipboard.writeText(text)`,
		`document.execCommand('copy')`,
		`await copyText(url.toString())`,
		`await copyText(text) ? 'Copied' : 'Failed'`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("module page misses clipboard behavior %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Copy requires HTTPS") {
		t.Fatalf("module copy remains disabled for HTTP origins:\n%s", body)
	}
}

func TestExecuteHTMLTemplateHidesRenderFailureAndWritesNoPartialPage(t *testing.T) {
	tmpl := template.Must(template.New("broken").Parse(`prefix {{template "missing" .}}`))
	recorder := httptest.NewRecorder()

	executeHTMLTemplate(recorder, tmpl, nil)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "prefix") || strings.Contains(body, "missing") {
		t.Fatalf("response leaked partial output or template details: %q", body)
	}
	if body != "internal server error\n" {
		t.Fatalf("body = %q, want generic error", body)
	}
}
