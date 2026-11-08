package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zxzharmlesszxz/puppet-forge/internal/auth"
	"github.com/zxzharmlesszxz/puppet-forge/internal/domain"
	"github.com/zxzharmlesszxz/puppet-forge/internal/store"
)

const (
	ReviewMaxExpanded   = 64 << 20
	ReviewMaxCompressed = 128 << 20
	ReviewMaxEntries    = 4096
	ReviewMaxText       = 64 << 10
	ReviewMaxLines      = 2000
)

var (
	ErrReviewLimit     = errors.New("release review limit exceeded")
	ErrReviewForbidden = errors.New("release review requires namespace administration")
	ErrReviewBusy      = errors.New("release review capacity is busy; retry later")
)

type ArchiveFile struct {
	Path   string
	Size   int64
	Mode   int64
	SHA256 string
}

type ArchiveReview struct {
	Release     domain.Release
	Files       []ArchiveFile
	Text        string
	Notice      string
	FilePresent bool
}

type ReleaseReview struct {
	Current ArchiveReview
	Base    *ArchiveReview
}

func validReviewPath(name string) bool {
	return name != "" && len(name) <= 1024 && utf8.ValidString(name) && !strings.ContainsFunc(name, unicode.IsControl) && !strings.Contains(name, "\\") && !path.IsAbs(name) && path.Clean(name) == name && name != "." && name != ".." && !strings.HasPrefix(name, "../") && strings.Count(name, "/") < 32
}

// ReviewReleases never records installation usage. The caller must retain this
// lease through rendering so expensive highlighters share the same concurrency bound.
func (s *ModuleService) ReviewReleases(ctx context.Context, principal auth.Principal, owner, name, version, base, selected, expectedSHA, baseSHA string) (ReleaseReview, func(), error) {
	if !principal.CanDeleteOwner(owner) {
		return ReleaseReview{}, nil, ErrReviewForbidden
	}
	if !validReviewPath(selected) {
		return ReleaseReview{}, nil, invalidInput("invalid review file path")
	}
	select {
	case s.reviewSlots <- struct{}{}:
	default:
		return ReleaseReview{}, nil, ErrReviewBusy
	}
	releaseSlot := func() { <-s.reviewSlots }
	fail := func(err error) (ReleaseReview, func(), error) { releaseSlot(); return ReleaseReview{}, nil, err }
	versions := []string{version}
	if base != "" {
		versions = append(versions, base)
	}
	for _, v := range versions {
		if v == "" {
			return fail(invalidInput("release version is required"))
		}
		release, err := s.modules.GetRelease(ctx, owner, name, v)
		if err != nil {
			return fail(err)
		}
		if release.Source == "upstream" && !principal.CanAdmin {
			return fail(ErrReviewForbidden)
		}
		if release.Source == "upstream" || release.SHA256 == "" {
			release, err = s.GetRelease(ctx, owner, name, v)
			if err == nil {
				_, err = s.EnsureReleaseChecksums(ctx, release)
			}
			if err != nil {
				return fail(err)
			}
		}
	}
	unlock, err := s.modules.LockModule(ctx, owner, name)
	if err != nil {
		return fail(err)
	}
	defer releaseModuleLock(unlock, owner, name)
	current, err := s.inspectStoredReview(ctx, owner, name, version, selected, expectedSHA)
	if err != nil {
		return fail(err)
	}
	if current.Release.Source == "upstream" && !principal.CanAdmin {
		return fail(ErrReviewForbidden)
	}
	result := ReleaseReview{Current: current}
	if base != "" {
		previous, err := s.inspectStoredReview(ctx, owner, name, base, selected, baseSHA)
		if err != nil {
			return fail(err)
		}
		if previous.Release.Source == "upstream" && !principal.CanAdmin {
			return fail(ErrReviewForbidden)
		}
		result.Base = &previous
	}
	return result, releaseSlot, nil
}

func (s *ModuleService) inspectStoredReview(ctx context.Context, owner, name, version, selected, expectedSHA string) (ArchiveReview, error) {
	moduleStatus, err := s.ResourceStatus(ctx, owner, name, "")
	if err != nil {
		return ArchiveReview{}, err
	}
	status, err := s.ResourceStatus(ctx, owner, name, version)
	if err != nil {
		return ArchiveReview{}, err
	}
	if (!moduleStatus.State.Readable() && moduleStatus.State != domain.StatePending) || (!status.State.Readable() && status.State != domain.StatePending) {
		return ArchiveReview{}, ErrUnavailable
	}
	release, err := s.modules.GetRelease(ctx, owner, name, version)
	if err != nil {
		return ArchiveReview{}, err
	}
	if expectedSHA != "" && expectedSHA != release.SHA256 {
		return ArchiveReview{}, fmt.Errorf("%w: reviewed release changed; reopen the release", store.ErrConflict)
	}
	object, err := s.artifacts.Open(ctx, releaseArchivePath(release))
	if err != nil {
		return ArchiveReview{}, err
	}
	defer func() { _ = object.Body.Close() }()
	result, digest, err := scanReviewArchive(ctx, object.Body, owner+"-"+name+"-"+version, selected, ReviewMaxExpanded)
	if err != nil {
		return ArchiveReview{}, err
	}
	if release.SHA256 == "" || digest != release.SHA256 {
		return ArchiveReview{}, fmt.Errorf("%w: review archive checksum mismatch", store.ErrConflict)
	}
	release.Status = status
	result.Release = release
	return result, nil
}

func scanReviewArchive(ctx context.Context, source io.Reader, expectedRoot, selected string, expandedLimit int64) (ArchiveReview, string, error) {
	var result ArchiveReview
	compressed := &io.LimitedReader{R: contextReader{ctx: ctx, reader: source}, N: ReviewMaxCompressed + 1}
	archiveHash := sha256.New()
	gz, err := gzip.NewReader(io.TeeReader(compressed, archiveHash))
	if err != nil {
		return result, "", err
	}
	defer func() { _ = gz.Close() }()
	expanded := &io.LimitedReader{R: contextReader{ctx: ctx, reader: gz}, N: expandedLimit + 1}
	reader := tar.NewReader(expanded)
	seen := make(map[string]struct{})
	regularFiles := make(map[string]struct{})
	directories := make(map[string]struct{})
	pathBytes := 0
	for count := 0; ; count++ {
		header, err := reader.Next()
		if expanded.N <= 0 || compressed.N <= 0 {
			return result, "", ErrReviewLimit
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, "", err
		}
		if count >= ReviewMaxEntries {
			return result, "", ErrReviewLimit
		}
		clean, root, err := validateArchiveEntry(header)
		if err != nil {
			return result, "", err
		}
		if root != expectedRoot {
			return result, "", errors.New("review archive root does not match release identity")
		}
		if clean != root && !validReviewPath(strings.TrimPrefix(clean, root+"/")) {
			return result, "", invalidInput("invalid archive entry path")
		}
		if _, duplicate := seen[clean]; duplicate {
			return result, "", errors.New("review archive contains duplicate paths")
		}
		seen[clean] = struct{}{}
		for parent := path.Dir(clean); parent != "."; parent = path.Dir(parent) {
			if _, file := regularFiles[parent]; file {
				return result, "", errors.New("archive directory conflicts with a file")
			}
			directories[parent] = struct{}{}
		}
		if header.Typeflag == tar.TypeDir {
			directories[clean] = struct{}{}
			continue
		}
		filePath := strings.TrimPrefix(clean, root+"/")
		if !validReviewPath(filePath) {
			return result, "", invalidInput("invalid archive file path")
		}
		pathBytes += len(filePath)
		if pathBytes > 256<<10 {
			return result, "", ErrReviewLimit
		}
		if _, directory := directories[clean]; directory {
			return result, "", errors.New("archive file conflicts with a directory")
		}
		regularFiles[clean] = struct{}{}
		if header.Size < 0 || header.Size > expandedLimit {
			return result, "", ErrReviewLimit
		}
		hash := sha256.New()
		var body bytes.Buffer
		var sink io.Writer = hash
		if filePath == selected && header.Size <= ReviewMaxText {
			sink = io.MultiWriter(hash, &body)
		}
		_, copyErr := io.CopyN(sink, reader, header.Size)
		if expanded.N <= 0 {
			return result, "", ErrReviewLimit
		}
		if copyErr != nil {
			return result, "", copyErr
		}
		result.Files = append(result.Files, ArchiveFile{Path: filePath, Size: header.Size, Mode: header.Mode & 0o7777, SHA256: hex.EncodeToString(hash.Sum(nil))})
		if filePath == selected {
			result.FilePresent = true
			switch {
			case header.Size > ReviewMaxText:
				result.Notice = "Text preview exceeds 64 KiB. Download the archive to inspect this file."
			case !utf8.Valid(body.Bytes()) || bytes.IndexByte(body.Bytes(), 0) >= 0:
				result.Notice = "Binary or non-UTF-8 file; text preview is unavailable."
			case bytes.Count(body.Bytes(), []byte{'\n'})+boolInt(body.Len() > 0 && !bytes.HasSuffix(body.Bytes(), []byte{'\n'})) > ReviewMaxLines:
				result.Notice = "Text preview exceeds 2000 lines. Download the archive to inspect this file."
			default:
				result.Text = body.String()
			}
		}
	}
	// Consume padding and the gzip trailer: a truncated or concatenated hidden
	// archive must not be displayed as a successful, complete review.
	buffer := make([]byte, 8192)
	for {
		n, err := expanded.Read(buffer)
		if expanded.N <= 0 || compressed.N <= 0 {
			return result, "", ErrReviewLimit
		}
		for _, b := range buffer[:n] {
			if b != 0 {
				return result, "", errors.New("unexpected data after review archive")
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, "", err
		}
	}
	slices.SortFunc(result.Files, func(a, b ArchiveFile) int { return strings.Compare(a.Path, b.Path) })
	return result, hex.EncodeToString(archiveHash.Sum(nil)), ctx.Err()
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
