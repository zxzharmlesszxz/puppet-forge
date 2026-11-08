package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/review"
	"github.com/zxzharmlesszxz/puppet-forge/internal/service"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

type reviewFileRow struct {
	Path, Status, URL string
	Selected          bool
	Old, New          *service.ArchiveFile
}

type reviewTreeNode struct {
	Name     string
	File     *reviewFileRow
	Children []*reviewTreeNode
}

type manageReviewData struct {
	Navigation                                         manageNavigationData
	Resource, Version, Base, Selected, Mode, CSRFToken string
	SHA256, BaseSHA256, ModerationURL, DownloadURL     string
	Versions                                           []domain.ModuleVersion
	Current                                            service.ArchiveReview
	Previous                                           *service.ArchiveReview
	Tree                                               []*reviewTreeNode
	File                                               reviewFileRow
	Lines                                              []review.Line
	Hunks                                              []review.Hunk
	Notice                                             string
	Added, Removed, Modified, Unchanged                int
	Comparing, Pending, OldNoNewline, NewNoNewline     bool
}

func manageReviewURL(owner, name, version string) string {
	return "/manage/modules/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/review?version=" + url.QueryEscape(version)
}

func (r *Router) manageReleaseReview(w http.ResponseWriter, req *http.Request, principal auth.Principal, owner, name string) {
	if !principal.CanDeleteOwner(owner) {
		r.audit(req, principal, "review_release", "failure", "forbidden", "space", owner, "module", owner+"/"+name)
		writeError(w, http.StatusForbidden, service.ErrReviewForbidden)
		return
	}
	if req.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, errors.New("method not allowed"))
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()
	query := req.URL.Query()
	version, base := query.Get("version"), query.Get("base")
	selected := query.Get("file")
	if selected == "" {
		selected = "metadata.json"
	}
	mode := query.Get("mode")
	if mode == "" {
		mode = "unified"
	}
	if mode != "unified" && mode != "split" {
		writeError(w, http.StatusBadRequest, errors.New("invalid review mode"))
		return
	}
	versions, err := r.modules.ListReleases(ctx, owner, name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if !query.Has("base") {
		found := false
		for _, candidate := range versions {
			if found && candidate.Status.State.Readable() {
				base = candidate.Version
				break
			}
			if candidate.Version == version {
				found = true
			}
		}
	}
	result, releaseSlot, err := r.modules.ReviewReleases(ctx, principal, owner, name, version, base, selected, query.Get("sha256"), query.Get("base_sha256"))
	if err != nil {
		r.audit(req, principal, "review_release", "failure", auditReason(err), "space", owner, "module", owner+"/"+name, "release", version)
		writeServiceError(w, err)
		return
	}
	defer releaseSlot()
	csrf, err := r.ensureManageCSRFToken(w, req)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	data := manageReviewData{
		Navigation: r.manageNavigation(req, principal, csrf, "modules", ""),
		Resource:   owner + "/" + name, Version: version, Base: base, Selected: selected, Mode: mode,
		CSRFToken: csrf, SHA256: result.Current.Release.SHA256, Versions: versions,
		Current: result.Current, Previous: result.Base, Comparing: result.Base != nil,
		Pending:       result.Current.Release.Status.State == domain.StatePending,
		ModerationURL: manageModerationURL(owner, name, version),
		DownloadURL:   manageModerationURL(owner, name, version) + "&download=true",
	}
	if result.Base != nil {
		data.BaseSHA256 = result.Base.Release.SHA256
	}
	query.Set("version", version)
	query.Set("base", base)
	query.Set("sha256", data.SHA256)
	query.Set("mode", mode)
	if data.BaseSHA256 != "" {
		query.Set("base_sha256", data.BaseSHA256)
	} else {
		query.Del("base_sha256")
	}
	files := reviewFiles(result)
	selectedFound := false
	for index := range files {
		file := &files[index]
		params := cloneValues(query)
		params.Set("file", file.Path)
		file.URL = req.URL.Path + "?" + params.Encode()
		file.Selected = file.Path == selected
		if file.Selected {
			data.File = *file
			selectedFound = true
		}
		switch file.Status {
		case "added":
			data.Added++
		case "removed":
			data.Removed++
		case "modified":
			data.Modified++
		default:
			data.Unchanged++
		}
	}
	if !selectedFound {
		writeServiceError(w, store.ErrNotFound)
		return
	}
	data.Tree = reviewTree(files)
	data.Notice = result.Current.Notice
	if result.Base != nil && result.Base.Notice != "" {
		data.Notice = "Base: " + result.Base.Notice
	}
	data.NewNoNewline = result.Current.Text != "" && !strings.HasSuffix(result.Current.Text, "\n")
	if result.Base != nil {
		data.OldNoNewline = result.Base.Text != "" && !strings.HasSuffix(result.Base.Text, "\n")
	}
	if data.Notice == "" {
		if result.Base != nil {
			data.Hunks, err = review.Compare(ctx, selected, result.Base.Text, result.Current.Text)
		} else {
			data.Lines, err = review.Highlight(ctx, selected, result.Current.Text)
		}
		if errors.Is(err, review.ErrLimit) {
			data.Notice = err.Error()
			err = nil
		}
		if err != nil {
			writeServiceError(w, err)
			return
		}
	}
	if err := ctx.Err(); err != nil {
		writeServiceError(w, err)
		return
	}
	r.audit(req, principal, "review_release", "success", "none", "space", owner, "module", owner+"/"+name, "release", version, "sha256", data.SHA256, "base_release", base, "base_sha256", data.BaseSHA256, "file", selected)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	executeHTMLTemplate(w, manageReviewTemplate, data)
}

func reviewFiles(result service.ReleaseReview) []reviewFileRow {
	rows := make(map[string]reviewFileRow)
	if result.Base != nil {
		for _, file := range result.Base.Files {
			rows[file.Path] = reviewFileRow{Path: file.Path, Old: &file, Status: "removed"}
		}
	}
	for _, file := range result.Current.Files {
		row := rows[file.Path]
		row.Path = file.Path
		row.New = &file
		switch {
		case result.Base == nil:
			row.Status = "file"
		case row.Old == nil:
			row.Status = "added"
		case row.Old.SHA256 != file.SHA256 || row.Old.Mode != file.Mode:
			row.Status = "modified"
		default:
			row.Status = "unchanged"
		}
		rows[file.Path] = row
	}
	files := make([]reviewFileRow, 0, len(rows))
	for _, row := range rows {
		files = append(files, row)
	}
	slices.SortFunc(files, func(a, b reviewFileRow) int { return strings.Compare(a.Path, b.Path) })
	return files
}

func reviewTree(files []reviewFileRow) []*reviewTreeNode {
	root := &reviewTreeNode{}
	for index := range files {
		file := &files[index]
		parent := root
		for part := range strings.SplitSeq(file.Path, "/") {
			var child *reviewTreeNode
			for _, candidate := range parent.Children {
				if candidate.Name == part {
					child = candidate
					break
				}
			}
			if child == nil {
				child = &reviewTreeNode{Name: part}
				parent.Children = append(parent.Children, child)
			}
			parent = child
		}
		parent.File = file
	}
	return root.Children
}

func cloneValues(values url.Values) url.Values {
	cloned := make(url.Values, len(values))
	for key, entries := range values {
		cloned[key] = slices.Clone(entries)
	}
	return cloned
}
