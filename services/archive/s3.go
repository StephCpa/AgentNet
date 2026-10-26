package archive

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

type S3Config struct {
	Endpoint string `json:"endpoint"`
	Region   string `json:"region"`
	Bucket   string `json:"bucket"`
	Prefix   string `json:"prefix"`
}
type S3Store struct {
	client   *s3.Client
	cfg      S3Config
	identity string
}

// Credentials are supplied from the operator environment, never persisted in the
// archive catalog or manifests. HTTPS is mandatory except for loopback fixtures.
func NewS3(cfg S3Config, accessKey, secretKey, sessionToken string, client *http.Client) (*S3Store, error) {
	if cfg.Region == "" || cfg.Bucket == "" || cfg.Prefix == "" || accessKey == "" || secretKey == "" {
		return nil, errors.New("S3 region, bucket, unique database prefix and credentials required")
	}
	cfg.Prefix = strings.Trim(cfg.Prefix, "/")
	if cfg.Prefix == "" {
		return nil, errors.New("empty archive prefix")
	}
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("invalid S3 endpoint")
		}
		if u.Scheme != "https" {
			ip := net.ParseIP(u.Hostname())
			if u.Scheme != "http" || ip == nil || !ip.IsLoopback() {
				return nil, errors.New("S3 endpoint requires HTTPS")
			}
		}
		cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	}
	opts := s3.Options{Region: cfg.Region, Credentials: credentials.NewStaticCredentialsProvider(accessKey, secretKey, sessionToken), UsePathStyle: true, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired}
	if cfg.Endpoint != "" {
		opts.BaseEndpoint = aws.String(cfg.Endpoint)
	}
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Minute}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	opts.HTTPClient = &copyClient
	return &S3Store{client: s3.New(opts), cfg: cfg, identity: cfg.Endpoint + "|" + cfg.Region + "|" + cfg.Bucket + "|" + cfg.Prefix}, nil
}
func (s *S3Store) ID() string { return "s3:" + s.identity }
func (s *S3Store) Put(ctx context.Context, key string, body io.ReadSeeker, size int64) (string, error) {
	result, err := s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(s.cfg.Prefix + "/" + key), Body: body, ContentLength: aws.Int64(size), IfNoneMatch: aws.String("*")})
	if err != nil {
		var api smithy.APIError
		if errors.As(err, &api) && api.ErrorCode() == "PreconditionFailed" {
			return "", nil
		}
		return "", err
	}
	return aws.ToString(result.VersionId), nil
}
func (s *S3Store) Open(ctx context.Context, key, version string) (Object, error) {
	in := &s3.GetObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(s.cfg.Prefix + "/" + key)}
	if version != "" {
		in.VersionId = aws.String(version)
	}
	result, err := s.client.GetObject(ctx, in)
	if err != nil {
		return Object{}, err
	}
	return Object{Body: result.Body, Version: aws.ToString(result.VersionId)}, nil
}
