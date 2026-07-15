package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"

	"puppet-forge/internal/httputil"

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
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(cleanObjectPath(objectPath)),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return fmt.Errorf("put object: %w", err)
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
	}, nil
}

func (s *S3Storage) Download(ctx context.Context, objectPath string) (Object, error) {
	resp, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(cleanObjectPath(objectPath)),
	})
	if err != nil {
		if isS3NotFound(err) {
			return Object{}, ErrObjectNotFound
		}
		return Object{}, fmt.Errorf("get object: %w", err)
	}

	body, err := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if err != nil {
		return Object{}, fmt.Errorf("read object: %w", err)
	}
	if closeErr != nil {
		return Object{}, fmt.Errorf("close object body: %w", closeErr)
	}

	return Object{
		Body:        body,
		ContentType: aws.ToString(resp.ContentType),
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
