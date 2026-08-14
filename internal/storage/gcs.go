package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/zxzharmlesszxz/puppet-forge/internal/httputil"
)

var ErrObjectNotFound = errors.New("object not found")

type Object struct {
	Body        []byte
	ContentType string
}

type ObjectAttrs struct {
	ContentType string
	Size        int64
	ETag        string
}

type ListedObject struct {
	Path      string
	Size      int64
	UpdatedAt time.Time
}

type ObjectReader struct {
	Body        io.ReadCloser
	ContentType string
	Size        int64
}

type ArtifactStorage interface {
	Upload(ctx context.Context, objectPath string, contentType string, body []byte) error
	UploadIfAbsent(ctx context.Context, objectPath string, contentType string, body []byte) (bool, error)
	UploadReaderIfAbsent(ctx context.Context, objectPath string, contentType string, body io.Reader) (bool, error)
	Delete(ctx context.Context, objectPath string) error
	Exists(ctx context.Context, objectPath string) (bool, error)
	Download(ctx context.Context, objectPath string) (Object, error)
	Open(ctx context.Context, objectPath string) (ObjectReader, error)
	PublicURL(objectPath string) string
	Stat(ctx context.Context, objectPath string) (ObjectAttrs, error)
}

type RangeArtifactStorage interface {
	OpenRange(ctx context.Context, objectPath string, offset, length int64) (ObjectReader, error)
}

type ArtifactLister interface {
	ListObjects(ctx context.Context, prefix string) ([]string, error)
}

type ArtifactMetadataLister interface {
	ListObjectMetadata(ctx context.Context, prefix string) ([]ListedObject, error)
}

type ArtifactMetadataIterator interface {
	IterateObjectMetadata(ctx context.Context, prefix string, visit func(ListedObject) error) error
}

type GCSStorage struct {
	client        *storage.Client
	bucket        string
	projectID     string
	publicBaseURL *url.URL
}

func NewGCSStorage(client *storage.Client, bucket, projectID, endpoint string) (*GCSStorage, error) {
	if endpoint == "" {
		endpoint = "https://storage.googleapis.com"
	}
	publicBaseURL, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse GCS artifact endpoint %q: %w", endpoint, err)
	}
	if publicBaseURL.Scheme == "" || publicBaseURL.Host == "" {
		return nil, fmt.Errorf("parse GCS artifact endpoint %q: absolute HTTP(S) URL required", endpoint)
	}
	if publicBaseURL.Scheme != "http" && publicBaseURL.Scheme != "https" {
		return nil, fmt.Errorf("parse GCS artifact endpoint %q: unsupported URL scheme %q", endpoint, publicBaseURL.Scheme)
	}
	return &GCSStorage{
		client:        client,
		bucket:        bucket,
		projectID:     projectID,
		publicBaseURL: publicBaseURL,
	}, nil
}

func (s *GCSStorage) EnsureBucket(ctx context.Context) error {
	_, err := s.client.Bucket(s.bucket).Attrs(ctx)
	if err == nil {
		return nil
	}
	if !errors.Is(err, storage.ErrBucketNotExist) {
		return fmt.Errorf("get bucket attrs: %w", err)
	}
	if err := s.client.Bucket(s.bucket).Create(ctx, s.projectID, nil); err != nil {
		return fmt.Errorf("create bucket: %w", err)
	}
	return nil
}

func (s *GCSStorage) Upload(ctx context.Context, objectPath string, contentType string, body []byte) error {
	_, err := s.upload(ctx, s.client.Bucket(s.bucket).Object(objectPath), contentType, body, false)
	return err
}

func (s *GCSStorage) UploadIfAbsent(ctx context.Context, objectPath string, contentType string, body []byte) (bool, error) {
	object := s.client.Bucket(s.bucket).Object(objectPath).If(storage.Conditions{DoesNotExist: true})
	return s.upload(ctx, object, contentType, body, true)
}

func (s *GCSStorage) UploadReaderIfAbsent(ctx context.Context, objectPath string, contentType string, body io.Reader) (bool, error) {
	object := s.client.Bucket(s.bucket).Object(objectPath).If(storage.Conditions{DoesNotExist: true})
	return s.uploadReader(ctx, object, contentType, body, true)
}

func (s *GCSStorage) upload(ctx context.Context, object *storage.ObjectHandle, contentType string, body []byte, createOnly bool) (bool, error) {
	return s.uploadReader(ctx, object, contentType, bytes.NewReader(body), createOnly)
}

func (s *GCSStorage) uploadReader(ctx context.Context, object *storage.ObjectHandle, contentType string, body io.Reader, createOnly bool) (bool, error) {
	writer := object.NewWriter(ctx)
	writer.ContentType = contentType

	if _, err := io.Copy(writer, body); err != nil {
		combined := objectWriterError(err, writer.Close())
		if createOnly && isGCSPreconditionFailed(combined) {
			return false, nil
		}
		return false, combined
	}

	if err := writer.Close(); err != nil {
		if createOnly && isGCSPreconditionFailed(err) {
			return false, nil
		}
		return false, fmt.Errorf("close object writer: %w", err)
	}

	return true, nil
}

func (s *GCSStorage) Open(ctx context.Context, objectPath string) (ObjectReader, error) {
	reader, err := s.client.Bucket(s.bucket).Object(objectPath).NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return ObjectReader{}, ErrObjectNotFound
		}
		return ObjectReader{}, fmt.Errorf("open object reader: %w", err)
	}
	return ObjectReader{Body: reader, ContentType: reader.Attrs.ContentType, Size: reader.Attrs.Size}, nil
}

func (s *GCSStorage) OpenRange(ctx context.Context, objectPath string, offset, length int64) (ObjectReader, error) {
	reader, err := s.client.Bucket(s.bucket).Object(objectPath).NewRangeReader(ctx, offset, length)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return ObjectReader{}, ErrObjectNotFound
		}
		return ObjectReader{}, fmt.Errorf("open object range reader: %w", err)
	}
	return ObjectReader{Body: reader, ContentType: reader.Attrs.ContentType, Size: length}, nil
}

func isGCSPreconditionFailed(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusPreconditionFailed ||
		status.Code(err) == codes.FailedPrecondition
}

func (s *GCSStorage) Delete(ctx context.Context, objectPath string) error {
	err := s.client.Bucket(s.bucket).Object(objectPath).Delete(ctx)
	if err == nil || errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	return fmt.Errorf("delete object: %w", err)
}

func objectWriterError(writeErr, closeErr error) error {
	if closeErr == nil {
		return fmt.Errorf("write object: %w", writeErr)
	}
	return errors.Join(
		fmt.Errorf("write object: %w", writeErr),
		fmt.Errorf("close object writer: %w", closeErr),
	)
}

func (s *GCSStorage) Exists(ctx context.Context, objectPath string) (bool, error) {
	_, err := s.client.Bucket(s.bucket).Object(objectPath).Attrs(ctx)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, storage.ErrObjectNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("get object attrs: %w", err)
}

func (s *GCSStorage) Download(ctx context.Context, objectPath string) (Object, error) {
	object, err := s.Open(ctx, objectPath)
	if err != nil {
		return Object{}, err
	}

	body, err := io.ReadAll(object.Body)
	closeErr := object.Body.Close()
	if err != nil {
		return Object{}, fmt.Errorf("read object: %w", err)
	}
	if closeErr != nil {
		return Object{}, fmt.Errorf("close object reader: %w", closeErr)
	}

	return Object{
		Body:        body,
		ContentType: object.ContentType,
	}, nil
}

func (s *GCSStorage) Stat(ctx context.Context, objectPath string) (ObjectAttrs, error) {
	attrs, err := s.client.Bucket(s.bucket).Object(objectPath).Attrs(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return ObjectAttrs{}, ErrObjectNotFound
		}
		return ObjectAttrs{}, fmt.Errorf("get object attrs: %w", err)
	}
	return ObjectAttrs{ContentType: attrs.ContentType, Size: attrs.Size, ETag: attrs.Etag}, nil
}

func (s *GCSStorage) ListObjects(ctx context.Context, prefix string) ([]string, error) {
	objects, err := s.ListObjectMetadata(ctx, prefix)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(objects))
	for _, object := range objects {
		paths = append(paths, object.Path)
	}
	return paths, nil
}

func (s *GCSStorage) ListObjectMetadata(ctx context.Context, prefix string) ([]ListedObject, error) {
	var listed []ListedObject
	err := s.IterateObjectMetadata(ctx, prefix, func(object ListedObject) error {
		listed = append(listed, object)
		return nil
	})
	return listed, err
}

func (s *GCSStorage) IterateObjectMetadata(ctx context.Context, prefix string, visit func(ListedObject) error) error {
	objects := s.client.Bucket(s.bucket).Objects(ctx, &storage.Query{Prefix: strings.TrimPrefix(prefix, "/")})
	for {
		attrs, err := objects.Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("list objects: %w", err)
		}
		if err := visit(ListedObject{Path: attrs.Name, Size: attrs.Size, UpdatedAt: attrs.Updated}); err != nil {
			return err
		}
	}
}

func (s *GCSStorage) PublicURL(objectPath string) string {
	if s.publicBaseURL == nil {
		return ""
	}
	base := *s.publicBaseURL
	key := cleanObjectPath(objectPath)
	base.Path = httputil.SingleJoiningSlash(base.Path, path.Join(s.bucket, key))
	return base.String()
}
