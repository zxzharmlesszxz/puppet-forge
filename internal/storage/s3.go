package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"

	"github.com/zxzharmlesszxz/puppet-forge/internal/httputil"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

type S3Storage struct {
	client        *s3.Client
	bucket        string
	publicBaseURL *url.URL
	pathStyle     bool
}

func NewS3Storage(ctx context.Context, endpoint, region, bucket, accessKeyID, secretAccessKey string, pathStyle bool) (*S3Storage, error) {
	if (accessKeyID == "") != (secretAccessKey == "") {
		return nil, errors.New("ARTIFACT_ACCESS_KEY_ID and ARTIFACT_SECRET_ACCESS_KEY must be set together")
	}
	if region == "" {
		region = "us-east-1"
	}

	loadOptions := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
	}
	if accessKeyID != "" || secretAccessKey != "" {
		loadOptions = append(loadOptions, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, ""),
		))
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	clientOptions := []func(*s3.Options){
		func(options *s3.Options) {
			options.UsePathStyle = pathStyle
		},
	}

	var publicBaseURL *url.URL
	if endpoint != "" {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("parse ARTIFACT_ENDPOINT: %w", err)
		}
		if parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, fmt.Errorf("parse ARTIFACT_ENDPOINT %q: absolute HTTP(S) URL required", endpoint)
		}
		publicBaseURL = parsed
		clientOptions = append(clientOptions, func(options *s3.Options) {
			options.BaseEndpoint = aws.String(endpoint)
		})
	}

	client := s3.NewFromConfig(cfg, clientOptions...)
	return &S3Storage{
		client:        client,
		bucket:        bucket,
		publicBaseURL: publicBaseURL,
		pathStyle:     pathStyle,
	}, nil
}

func (s *S3Storage) Upload(ctx context.Context, objectPath string, contentType string, body []byte) error {
	_, err := s.putObject(ctx, objectPath, contentType, body, false)
	return err
}

func (s *S3Storage) UploadIfAbsent(ctx context.Context, objectPath string, contentType string, body []byte) (bool, error) {
	return s.putObject(ctx, objectPath, contentType, body, true)
}

func (s *S3Storage) UploadReaderIfAbsent(ctx context.Context, objectPath string, contentType string, body io.Reader) (bool, error) {
	return s.putObjectReader(ctx, objectPath, contentType, body, true)
}

func (s *S3Storage) putObject(ctx context.Context, objectPath string, contentType string, body []byte, createOnly bool) (bool, error) {
	return s.putObjectReader(ctx, objectPath, contentType, bytes.NewReader(body), createOnly)
}

func (s *S3Storage) putObjectReader(ctx context.Context, objectPath string, contentType string, body io.Reader, createOnly bool) (bool, error) {
	input := &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(cleanObjectPath(objectPath)),
		Body:        body,
		ContentType: aws.String(contentType),
	}
	if createOnly {
		input.IfNoneMatch = aws.String("*")
	}
	_, err := s.client.PutObject(ctx, input)
	if err != nil {
		if createOnly && isS3PreconditionFailed(err) {
			return false, nil
		}
		return false, fmt.Errorf("put object: %w", err)
	}
	return true, nil
}

func (s *S3Storage) Open(ctx context.Context, objectPath string) (ObjectReader, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(cleanObjectPath(objectPath)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return ObjectReader{}, ErrObjectNotFound
		}
		return ObjectReader{}, fmt.Errorf("get object: %w", err)
	}
	return ObjectReader{
		Body:        resp.Body,
		ContentType: aws.ToString(resp.ContentType),
		Size:        aws.ToInt64(resp.ContentLength),
	}, nil
}

func (s *S3Storage) OpenRange(ctx context.Context, objectPath string, offset, length int64) (ObjectReader, error) {
	byteRange := fmt.Sprintf("bytes=%d-", offset)
	if length > 0 {
		byteRange = fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)
	}
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(cleanObjectPath(objectPath)),
		Range:  aws.String(byteRange),
	})
	if err != nil {
		if isS3NotFound(err) {
			return ObjectReader{}, ErrObjectNotFound
		}
		return ObjectReader{}, fmt.Errorf("get object range: %w", err)
	}
	return ObjectReader{
		Body:        resp.Body,
		ContentType: aws.ToString(resp.ContentType),
		Size:        aws.ToInt64(resp.ContentLength),
	}, nil
}

func isS3PreconditionFailed(err error) bool {
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		return apiErr.ErrorCode() == "PreconditionFailed"
	}
	return false
}

func (s *S3Storage) Delete(ctx context.Context, objectPath string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(cleanObjectPath(objectPath)),
	})
	if err != nil && !isS3NotFound(err) {
		return fmt.Errorf("delete object: %w", err)
	}
	return nil
}

func (s *S3Storage) Exists(ctx context.Context, objectPath string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(cleanObjectPath(objectPath)),
	})
	if err == nil {
		return true, nil
	}

	if isS3NotFound(err) {
		return false, nil
	}

	return false, fmt.Errorf("head object: %w", err)
}

func isS3NotFound(err error) bool {
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey":
			return true
		}
	}
	return false
}

func (s *S3Storage) Stat(ctx context.Context, objectPath string) (ObjectAttrs, error) {
	resp, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(cleanObjectPath(objectPath)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return ObjectAttrs{}, ErrObjectNotFound
		}
		return ObjectAttrs{}, fmt.Errorf("head object: %w", err)
	}
	return ObjectAttrs{
		ContentType: aws.ToString(resp.ContentType),
		Size:        aws.ToInt64(resp.ContentLength),
		ETag:        aws.ToString(resp.ETag),
	}, nil
}

func (s *S3Storage) ListObjects(ctx context.Context, prefix string) ([]string, error) {
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

func (s *S3Storage) ListObjectMetadata(ctx context.Context, prefix string) ([]ListedObject, error) {
	var listed []ListedObject
	err := s.IterateObjectMetadata(ctx, prefix, func(object ListedObject) error {
		listed = append(listed, object)
		return nil
	})
	return listed, err
}

func (s *S3Storage) IterateObjectMetadata(ctx context.Context, prefix string, visit func(ListedObject) error) error {
	paginator := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(cleanObjectPath(prefix)),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list objects: %w", err)
		}
		for _, object := range page.Contents {
			if key := aws.ToString(object.Key); key != "" {
				if err := visit(ListedObject{
					Path:      key,
					Size:      aws.ToInt64(object.Size),
					UpdatedAt: aws.ToTime(object.LastModified),
				}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (s *S3Storage) Download(ctx context.Context, objectPath string) (Object, error) {
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
		return Object{}, fmt.Errorf("close object body: %w", closeErr)
	}

	return Object{
		Body:        body,
		ContentType: object.ContentType,
	}, nil
}

func (s *S3Storage) PublicURL(objectPath string) string {
	if s.publicBaseURL == nil {
		return ""
	}

	base := *s.publicBaseURL
	key := cleanObjectPath(objectPath)
	if s.pathStyle {
		base.Path = httputil.SingleJoiningSlash(base.Path, path.Join(s.bucket, key))
		return base.String()
	}

	host := base.Host
	if host != "" {
		base.Host = s.bucket + "." + host
	}
	base.Path = httputil.SingleJoiningSlash(base.Path, key)
	return base.String()
}
