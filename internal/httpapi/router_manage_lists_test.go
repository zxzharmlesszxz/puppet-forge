package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
)

func TestRequestedManageListValidatesQueryAndPage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		target    string
		wantQuery string
		wantPage  int
		wantError bool
	}{
		{name: "defaults", target: "/manage/teams", wantPage: 1},
		{name: "query and page", target: "/manage/teams?q=%20platform%20&page=3", wantQuery: "platform", wantPage: 3},
		{name: "non numeric page", target: "/manage/teams?page=next", wantError: true},
		{name: "zero page", target: "/manage/teams?page=0", wantError: true},
		{name: "excessive page", target: "/manage/teams?page=100001", wantError: true},
		{name: "excessive query", target: "/manage/teams?q=" + strings.Repeat("a", maxManageListQueryLength+1), wantError: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			query, page, _, err := requestedManageList(httptest.NewRequest(http.MethodGet, tc.target, nil), "teams-list")
			if (err != nil) != tc.wantError {
				t.Fatalf("requestedManageList() error = %v, wantError %v", err, tc.wantError)
			}
			if err == nil && (query != tc.wantQuery || page != tc.wantPage) {
				t.Fatalf("requestedManageList() = (%q, %d), want (%q, %d)", query, page, tc.wantQuery, tc.wantPage)
			}
		})
	}
}

func TestManageListPaginationPreservesFilter(t *testing.T) {
	t.Parallel()

	pagination, err := manageListPagination(
		"/manage/teams",
		url.Values{"q": {"platform"}, "per_page": {"20"}, "ignored": {"value"}},
		"teams-list",
		2,
		20,
		41,
	)
	if err != nil {
		t.Fatalf("manageListPagination() error = %v", err)
	}
	if pagination.TotalPages != 3 || pagination.PrevURL != "/manage/teams?q=platform#teams-list" || pagination.NextURL != "/manage/teams?page=3&q=platform#teams-list" {
		t.Fatalf("unexpected pagination: %#v", pagination)
	}
	if pagination.SizeAction != "/manage/teams" || pagination.SizeParam != "per_page" || pagination.SizePageParam != "page" || pagination.SizeTarget != "teams-list" || pagination.SizeStorageKey != "manage/teams:per_page:teams-list" || pagination.SizeCookieName != "puppet_forge_page_size_manage_teams_per_page_teams-list" || len(pagination.SizeOptions) != len(allowedPageSizes) {
		t.Fatalf("page-size control is incomplete: %#v", pagination)
	}
	if len(pagination.SizeParams) != 2 || pagination.SizeParams[0] != (queryParameter{Name: "ignored", Value: "value"}) || pagination.SizeParams[1] != (queryParameter{Name: "q", Value: "platform"}) {
		t.Fatalf("page-size parameters = %#v", pagination.SizeParams)
	}
	if page, changed := normalizedListPage(2, 20, 0); page != 1 || !changed {
		t.Fatalf("normalizedListPage() = (%d, %t), want (1, true)", page, changed)
	}
}

func TestHandleCanonicalListPageUsesFragmentHeader(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/manage/modules?page=3", nil)
	req.Header.Set("X-Puppet-Forge-Fragment", "manage-module-list")
	recorder := httptest.NewRecorder()
	if redirected := handleCanonicalListPage(recorder, req, true, "/manage/modules?page=2#manage-module-list"); redirected {
		t.Fatal("fragment request was redirected")
	}
	if got := recorder.Header().Get("X-Puppet-Forge-Canonical-URL"); got != "/manage/modules?page=2#manage-module-list" {
		t.Fatalf("canonical URL header = %q", got)
	}
}

func TestHandleCanonicalListPageRedirectsFullRequest(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/manage/modules?page=3", nil)
	recorder := httptest.NewRecorder()
	if redirected := handleCanonicalListPage(recorder, req, true, "/manage/modules?page=2#manage-module-list"); !redirected {
		t.Fatal("full request was not redirected")
	}
	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/manage/modules?page=2#manage-module-list" {
		t.Fatalf("response = (%d, %q)", recorder.Code, recorder.Header().Get("Location"))
	}
}

func TestRequestedPageSizeAllowsOnlyBoundedOptions(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"10", "20", "50", "100"} {
		req := httptest.NewRequest(http.MethodGet, "/manage/teams", nil)
		req.AddCookie(&http.Cookie{Name: "puppet_forge_page_size_manage_teams_per_page_teams-list", Value: value})
		pageSize := requestedPageSize(req, "/manage/teams", "per_page", "teams-list", 20)
		want, parseErr := strconv.Atoi(value)
		if parseErr != nil || pageSize != want {
			t.Fatalf("requestedPageSize(%q) = %d, want %d", value, pageSize, want)
		}
	}
	for _, value := range []string{"0", "25", "101", "all", "-1"} {
		req := httptest.NewRequest(http.MethodGet, "/manage/teams", nil)
		req.AddCookie(&http.Cookie{Name: "puppet_forge_page_size_manage_teams_per_page_teams-list", Value: value})
		if pageSize := requestedPageSize(req, "/manage/teams", "per_page", "teams-list", 20); pageSize != 20 {
			t.Fatalf("requestedPageSize(%q) = %d, want fallback 20", value, pageSize)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/manage/teams?per_page=100", nil)
	if pageSize := requestedPageSize(req, "/manage/teams", "per_page", "teams-list", 20); pageSize != 20 {
		t.Fatalf("query parameter changed page size to %d", pageSize)
	}
}

func TestManageListFiltersTeamsAndPublishSpaces(t *testing.T) {
	t.Parallel()

	teams := []manageTeamSummary{
		{Team: "platform", Spaces: []string{"platform", "sharedmodules"}},
		{Team: "security", Spaces: []string{"security"}},
	}
	for query, want := range map[string]string{"PLATFORM": "platform", "shared": "platform", "security": "security"} {
		filtered := filterManageTeamSummaries(teams, query)
		if len(filtered) != 1 || filtered[0].Team != want {
			t.Fatalf("filterManageTeamSummaries(%q) = %#v, want team %q", query, filtered, want)
		}
	}

	spaces := []managePublishSpaceAssignment{
		{Team: "platform", Space: "platform", Primary: true},
		{Team: "platform", Space: "sharedmodules"},
		{Space: "puppetlabs", Upstream: true},
	}
	for query, want := range map[string]string{"primary": "platform", "extra": "sharedmodules", "SHARED": "sharedmodules", "upstream": "puppetlabs"} {
		filtered := filterManagePublishSpaceAssignments(spaces, query)
		if len(filtered) != 1 || filtered[0].Space != want {
			t.Fatalf("filterManagePublishSpaceAssignments(%q) = %#v, want space %q", query, filtered, want)
		}
	}
}

func TestConfiguredPublishSpacesReturnsSortedUniqueNonGlobalSpaces(t *testing.T) {
	t.Parallel()

	configs := []auth.TeamConfig{
		{Team: auth.GlobalAdminTeam, PublishOwners: []string{"ignored"}},
		{Team: "platform", PublishOwners: []string{"shared", "platform"}},
		{Team: "security", PublishOwners: []string{"shared"}},
	}
	want := []string{"platform", "security", "shared"}
	if got := configuredPublishSpaces(configs); !slices.Equal(got, want) {
		t.Fatalf("configuredPublishSpaces() = %#v, want %#v", got, want)
	}
}

func TestManagePublishSpaceAssignmentsIncludeReadOnlyUpstreamOwners(t *testing.T) {
	t.Parallel()

	assignments, teams := managePublishSpaceAssignments(
		[]auth.TeamConfig{{Team: "platform", PublishOwners: []string{"platform", "shared"}}},
		map[string]int{"platform": 2, "shared": 1},
		map[string]int{"puppetlabs": 4, "platform": 1},
	)
	if !slices.Equal(teams, []string{"platform"}) || len(assignments) != 4 {
		t.Fatalf("managePublishSpaceAssignments() = (%#v, %#v)", assignments, teams)
	}
	upstream := assignments[2:]
	if upstream[0].Space != "platform" || !upstream[0].Upstream || upstream[0].ModuleCount != 1 || upstream[0].ModulesURL != "/manage/modules?q=platform%2F" {
		t.Fatalf("shared-name upstream assignment = %#v", upstream[0])
	}
	if upstream[1].Space != "puppetlabs" || !upstream[1].Upstream || upstream[1].ModuleCount != 4 || upstream[1].Team != "" {
		t.Fatalf("upstream assignment = %#v", upstream[1])
	}
}

func TestManageListTemplatesExposeAsyncFiltersAndPagination(t *testing.T) {
	t.Parallel()

	pagination := paginationData{
		Page: 1, Total: 21, TotalPages: 2, HasNext: true,
		NextURL: "/manage/teams?page=2&q=platform#teams-list",
	}
	var teamsPage bytes.Buffer
	if err := manageTeamsTemplate.Execute(&teamsPage, manageTeamsData{
		Teams:      []manageTeamSummary{{Team: "platform"}},
		Query:      "platform",
		Pagination: pagination,
		Filter: listFilterData{ID: "teams-filter", InputID: "teams-query", Action: "/manage/teams", Target: "teams-list",
			Label: "teams", Name: "q", Placeholder: "Filter teams", Value: "platform", ClearURL: "/manage/teams"},
	}); err != nil {
		t.Fatalf("render teams page: %v", err)
	}
	for _, want := range []string{
		`id="teams-list" data-async-list`,
		`data-async-filter="teams-list"`,
		`value="platform"`,
		`href="/manage/teams?page=2&amp;q=platform#teams-list"`,
	} {
		if !strings.Contains(teamsPage.String(), want) {
			t.Fatalf("teams page misses %q:\n%s", want, teamsPage.String())
		}
	}

	var spacesPage bytes.Buffer
	if err := managePublishSpacesTemplate.Execute(&spacesPage, managePublishSpacesData{
		Assignments: []managePublishSpaceAssignment{
			{Team: "platform", Space: "platform", ModulesURL: "/manage/modules?q=platform%2F", Primary: true},
			{Space: "puppetlabs", ModulesURL: "/manage/modules?q=puppetlabs%2F", ModuleCount: 4, Upstream: true},
		},
		Query:      "primary",
		Pagination: paginationData{Page: 1, Total: 21, TotalPages: 2, HasNext: true, NextURL: "/manage/admin/spaces?page=2&q=primary#publish-space-list"},
		Filter: listFilterData{ID: "publish-space-filter", InputID: "publish-space-query", Action: "/manage/admin/spaces", Target: "publish-space-list",
			Label: "publish spaces", Name: "q", Placeholder: "Filter spaces", Value: "primary", ClearURL: "/manage/admin/spaces"},
	}); err != nil {
		t.Fatalf("render publish spaces page: %v", err)
	}
	for _, want := range []string{
		`id="publish-space-list" data-async-list`,
		`data-async-filter="publish-space-list"`,
		`value="primary"`,
		`href="/manage/admin/spaces?page=2&amp;q=primary#publish-space-list"`,
		`data-href="/manage/modules?q=platform%2F"`,
		`aria-label="View modules in platform publish space"`,
		`event.target.closest("a, button, input, select, textarea, form")`,
		`event.key !== "Enter" && event.key !== " "`,
		`<span class="muted">Official Forge</span>`,
		`<span class="badge upstream">Upstream</span>`,
		`<span class="badge read-only">Read only</span>`,
	} {
		if !strings.Contains(spacesPage.String(), want) {
			t.Fatalf("publish spaces page misses %q:\n%s", want, spacesPage.String())
		}
	}
}
