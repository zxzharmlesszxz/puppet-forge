package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"cloud.google.com/go/storage"
)

var ErrObjectNotFound = errors.New("object not found")

type Object struct {
	Body        []byte
	ContentType string
}

type ObjectAttrs struct {
	ContentType string
	Size        int64
}

type ArtifactStorage interface {
	Upload(ctx context.Context, objectPath string, contentType string, body []byte) error
	Exists(ctx context.Context, objectPath string) (bool, error)
	Download(ctx context.Context, objectPath string) (Object, error)
	PublicURL(objectPath string) string
	Stat(ctx context.Context, objectPath string) (ObjectAttrs, error)
}

type GCSStorage struct {
	client    *storage.Client
	bucket    string
	projectID string
}

func NewGCSStorage(client *storage.Client, bucket string, projectID string) *GCSStorage {
	return &GCSStorage{
		client:    client,
		bucket:    bucket,
		projectID: projectID,
	}
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
	writer := s.client.Bucket(s.bucket).Object(objectPath).NewWriter(ctx)
	writer.ContentType = contentType

	if _, err := bytes.NewReader(body).WriteTo(writer); err != nil {
		return objectWriterError(err, writer.Close())
	}

	if err := writer.Close(); err != nil {
		return fmt.Errorf("close object writer: %w", err)
	}

	return nil
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
	reader, err := s.client.Bucket(s.bucket).Object(objectPath).NewReader(ctx)
	if err != nil {
		if errors.Is(err, storage.ErrObjectNotExist) {
			return Object{}, ErrObjectNotFound
		}
		return Object{}, fmt.Errorf("open object reader: %w", err)
	}

	body, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil {
		return Object{}, fmt.Errorf("read object: %w", err)
	}
	if closeErr != nil {
		return Object{}, fmt.Errorf("close object reader: %w", closeErr)
	}

	return Object{
		Body:        body,
		ContentType: reader.Attrs.ContentType,
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
	return ObjectAttrs{ContentType: attrs.ContentType, Size: attrs.Size}, nil
}

func (s *GCSStorage) PublicURL(objectPath string) string {
	key := cleanObjectPath(objectPath)
	return fmt.Sprintf("https://storage.googleapis.com/%s/%s", s.bucket, key)
}
