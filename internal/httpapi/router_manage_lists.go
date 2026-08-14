package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
)

const (
	manageListPageSize       = 20
	maxManageListPage        = 100000
	maxManageListQueryLength = 200
	pageSizeCookiePrefix     = "puppet_forge_page_size_"
)

var allowedPageSizes = [...]int{10, 20, 50, 100}

func requestedPageSize(req *http.Request, action, parameter, target string, fallback int) int {
	cookie, err := req.Cookie(pageSizeCookieName(pageSizeStorageKey(action, parameter, target)))
	if err != nil {
		return fallback
	}
	pageSize, err := strconv.Atoi(strings.TrimSpace(cookie.Value))
	if err != nil {
		return fallback
	}
	for _, allowed := range allowedPageSizes {
		if pageSize == allowed {
			return pageSize
		}
	}
	return fallback
}

func configurePageSize(pagination paginationData, current url.Values, action, sizeParameter, pageParameter, anchor, target string) paginationData {
	pagination.SizeAction = action
	pagination.SizeParam = sizeParameter
	pagination.SizePageParam = pageParameter
	pagination.SizeAnchor = anchor
	pagination.SizeTarget = target
	pagination.SizeStorageKey = pageSizeStorageKey(action, sizeParameter, target)
	pagination.SizeCookieName = pageSizeCookieName(pagination.SizeStorageKey)
	pagination.SizeOptions = make([]pageSizeOption, 0, len(allowedPageSizes))
	for _, value := range allowedPageSizes {
		pagination.SizeOptions = append(pagination.SizeOptions, pageSizeOption{Value: value, Selected: value == pagination.PageSize})
	}
	keys := make([]string, 0, len(current))
	for key := range current {
		if key != pageParameter && !isPageSizeParameter(key) && key != "message" && key != "error" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		for _, value := range current[key] {
			pagination.SizeParams = append(pagination.SizeParams, queryParameter{Name: key, Value: value})
		}
	}
	return pagination
}

func pageSizeStorageKey(action, parameter, target string) string {
	storageScope := strings.Trim(action, "/")
	if storageScope == "" {
		storageScope = "public-modules"
	}
	key := storageScope + ":" + parameter
	if target != "" {
		key += ":" + target
	}
	return key
}

func pageSizeCookieName(storageKey string) string {
	return pageSizeCookiePrefix + strings.NewReplacer("/", "_", ":", "_").Replace(storageKey)
}

func isPageSizeParameter(parameter string) bool {
	switch parameter {
	case "per_page", "modules_per_page", "teams_per_page", "spaces_per_page", "token_per_page", "history_per_page":
		return true
	default:
		return false
	}
}

func requestedManageList(req *http.Request, target string) (string, int, int, error) {
	query := strings.TrimSpace(req.URL.Query().Get("q"))
	if len(query) > maxManageListQueryLength {
		return "", 0, 0, errors.New("filter query is too long")
	}
	pageSize := requestedPageSize(req, req.URL.Path, "per_page", target, manageListPageSize)
	page := 1
	if raw := req.URL.Query().Get("page"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > maxManageListPage {
			return "", 0, 0, errors.New("invalid page")
		}
		page = parsed
	}
	return query, page, pageSize, nil
}

func pageItems[T any](items []T, page, pageSize int) []T {
	start := (page - 1) * pageSize
	if start >= len(items) {
		return nil
	}
	return items[start:min(start+pageSize, len(items))]
}

func paginationFor(page, pageSize, total int, pageURL func(int) string) paginationData {
	totalPages := (total + pageSize - 1) / pageSize
	pagination := paginationData{
		Page:       page,
		Total:      total,
		TotalPages: totalPages,
		PageSize:   pageSize,
		HasPrev:    page > 1,
		HasNext:    page < totalPages,
	}
	if pagination.HasPrev {
		pagination.PrevURL = pageURL(page - 1)
	}
	if pagination.HasNext {
		pagination.NextURL = pageURL(page + 1)
	}
	return pagination
}

func manageListPagination(basePath string, current url.Values, anchor string, page, pageSize, total int) (paginationData, error) {
	pageURL := func(target int) string {
		values := url.Values{}
		if query := strings.TrimSpace(current.Get("q")); query != "" {
			values.Set("q", query)
		}
		if target > 1 {
			values.Set("page", strconv.Itoa(target))
		}
		result := basePath
		if encoded := values.Encode(); encoded != "" {
			result += "?" + encoded
		}
		if anchor != "" {
			result += "#" + anchor
		}
		return result
	}
	pagination := paginationFor(page, pageSize, total, pageURL)
	if page > max(1, pagination.TotalPages) {
		return paginationData{}, errors.New("invalid page")
	}
	return configurePageSize(pagination, current, basePath, "per_page", "page", anchor, anchor), nil
}

func (r *Router) loadTeamConfigsAndModuleCounts(ctx context.Context) ([]auth.TeamConfig, map[string]int, error) {
	configs, err := r.modules.LoadTeamConfigs(ctx)
	if err != nil {
		return nil, nil, err
	}
	moduleCounts, err := r.modules.CountModulesByOwner(ctx, configuredPublishSpaces(configs))
	if err != nil {
		return nil, nil, err
	}
	return configs, moduleCounts, nil
}

func configuredPublishSpaces(configs []auth.TeamConfig) []string {
	spaces := make(map[string]struct{})
	for _, cfg := range configs {
		if isGlobalAdminConfig(cfg) {
			continue
		}
		for _, space := range teamPublishSpaces(cfg) {
			spaces[space] = struct{}{}
		}
	}
	result := make([]string, 0, len(spaces))
	for space := range spaces {
		result = append(result, space)
	}
	sort.Strings(result)
	return result
}

func filterManageTeamSummaries(rows []manageTeamSummary, query string) []manageTeamSummary {
	needle := strings.ToLower(query)
	if needle == "" {
		return rows
	}
	filtered := make([]manageTeamSummary, 0, len(rows))
	for _, row := range rows {
		haystack := strings.ToLower(row.Team + " " + strings.Join(row.Spaces, " "))
		if strings.Contains(haystack, needle) {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func filterManagePublishSpaceAssignments(rows []managePublishSpaceAssignment, query string) []managePublishSpaceAssignment {
	needle := strings.ToLower(query)
	if needle == "" {
		return rows
	}
	filtered := make([]managePublishSpaceAssignment, 0, len(rows))
	for _, row := range rows {
		spaceType := "extra"
		if row.Upstream {
			spaceType = "upstream"
		} else if row.Primary {
			spaceType = "primary"
		}
		if strings.Contains(strings.ToLower(row.Team+" "+row.Space+" "+spaceType), needle) {
			filtered = append(filtered, row)
		}
	}
	return filtered
}
