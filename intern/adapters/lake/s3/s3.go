// Package s3 is a Lake on an S3-compatible bucket (Hetzner Object Storage,
// MinIO, AWS).
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core/domain"
	"github.com/avero-it/laker/intern/core/ports/out"
)

// partSize is both the largest single-request upload and the size of each
// multipart part; one part is held in memory at a time. A var for the tests.
var partSize int64 = 64 << 20

var _ out.Lake = (*Lake)(nil)

type Options struct {
	Endpoint  string
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	PathStyle bool
	// ConditionalPut sends If-None-Match: * so the bucket refuses overwrites.
	ConditionalPut bool
}

type Lake struct {
	client *s3.Client
	opts   Options
	logger zerolog.Logger
}

func New(opts Options, logger zerolog.Logger) *Lake {
	client := s3.New(s3.Options{
		Region:       opts.Region,
		BaseEndpoint: aws.String(opts.Endpoint),
		UsePathStyle: opts.PathStyle,
		Credentials:  credentials.NewStaticCredentialsProvider(opts.AccessKey, opts.SecretKey, ""),
		// S3-compatible services do not all accept the checksum headers newer
		// SDKs add to every request; send them only when an operation needs them.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &Lake{client: client, opts: opts, logger: logger}
}

func (l *Lake) ifNoneMatch() *string {
	if l.opts.ConditionalPut {
		return aws.String("*")
	}
	return nil
}

// PutIfAbsent skips keys that exist (HEAD), then uploads in one request up to
// partSize, in parts above it. With ConditionalPut the bucket also rejects
// the write if someone created the key in between, which is reported as
// created=false.
func (l *Lake) PutIfAbsent(ctx context.Context, key string, r io.Reader, size int64) (bool, error) {
	if err := domain.ValidKey(key); err != nil {
		return false, err
	}
	exists, err := l.Exists(ctx, key)
	if err != nil || exists {
		return false, err
	}
	return l.put(ctx, key, r, size)
}

// put uploads without looking first; a 412 from the bucket is created=false.
func (l *Lake) put(ctx context.Context, key string, r io.Reader, size int64) (bool, error) {
	if size <= partSize {
		body, err := readExactly(r, size)
		if err != nil {
			return false, fmt.Errorf("ERR310: cannot read %s: %w", key, err)
		}
		_, err = l.client.PutObject(ctx, &s3.PutObjectInput{
			Bucket:        &l.opts.Bucket,
			Key:           &key,
			Body:          bytes.NewReader(body),
			ContentLength: aws.Int64(size),
			IfNoneMatch:   l.ifNoneMatch(),
		})
		if status(err) == http.StatusPreconditionFailed {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("ERR311: cannot put %s: %w", key, err)
		}
		return true, nil
	}

	return l.multipart(ctx, key, r, size)
}

func (l *Lake) multipart(ctx context.Context, key string, r io.Reader, size int64) (bool, error) {
	up, err := l.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &l.opts.Bucket, Key: &key})
	if err != nil {
		return false, fmt.Errorf("ERR312: cannot start the upload of %s: %w", key, err)
	}
	// An unfinished upload is not an object of the lake: aborting it only
	// frees its parts.
	abort := func() {
		_, aerr := l.client.AbortMultipartUpload(context.WithoutCancel(ctx), &s3.AbortMultipartUploadInput{
			Bucket: &l.opts.Bucket, Key: &key, UploadId: up.UploadId,
		})
		if aerr != nil {
			l.logger.Warn().Str("key", key).Msgf("WRN310: cannot abort the multipart upload: %v", aerr)
		}
	}

	var parts []types.CompletedPart
	for n, left := int32(1), size; left > 0; n++ {
		chunk := min(left, partSize)
		body, err := readExactly(r, chunk)
		if err != nil {
			abort()
			return false, fmt.Errorf("ERR313: cannot read %s: %w", key, err)
		}
		res, err := l.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: &l.opts.Bucket, Key: &key, UploadId: up.UploadId,
			PartNumber: aws.Int32(n), Body: bytes.NewReader(body), ContentLength: aws.Int64(chunk),
		})
		if err != nil {
			abort()
			return false, fmt.Errorf("ERR314: cannot upload part %d of %s: %w", n, key, err)
		}
		parts = append(parts, types.CompletedPart{ETag: res.ETag, PartNumber: aws.Int32(n)})
		left -= chunk
	}

	_, err = l.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &l.opts.Bucket, Key: &key, UploadId: up.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
		IfNoneMatch:     l.ifNoneMatch(),
	})
	if status(err) == http.StatusPreconditionFailed {
		abort()
		return false, nil
	}
	if err != nil {
		abort()
		return false, fmt.Errorf("ERR315: cannot complete the upload of %s: %w", key, err)
	}
	return true, nil
}

func (l *Lake) Exists(ctx context.Context, key string) (bool, error) {
	if err := domain.ValidKey(key); err != nil {
		return false, err
	}
	_, err := l.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &l.opts.Bucket, Key: &key})
	if status(err) == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ERR316: cannot stat %s: %w", key, err)
	}
	return true, nil
}

func (l *Lake) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := domain.ValidKey(key); err != nil {
		return nil, err
	}
	res, err := l.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &l.opts.Bucket, Key: &key})
	if err != nil {
		return nil, fmt.Errorf("ERR317: cannot get %s: %w", key, err)
	}
	return res.Body, nil
}

func (l *Lake) List(ctx context.Context, prefix string, fn func(key string) error) error {
	p := s3.NewListObjectsV2Paginator(l.client, &s3.ListObjectsV2Input{Bucket: &l.opts.Bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("ERR318: cannot list %q: %w", prefix, err)
		}
		for _, o := range page.Contents {
			if err := fn(aws.ToString(o.Key)); err != nil {
				return err
			}
		}
	}
	return nil
}

// Check reaches the bucket and reads its Object Lock configuration; it writes
// nothing, since nothing written to the lake can be removed.
func (l *Lake) Check(ctx context.Context) (domain.LakeInfo, error) {
	info := domain.LakeInfo{Kind: "s3", Location: l.opts.Endpoint + "/" + l.opts.Bucket}

	if _, err := l.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &l.opts.Bucket}); err != nil {
		return info, fmt.Errorf("ERR319: cannot reach bucket %s: %w", l.opts.Bucket, err)
	}

	lock, err := l.client.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: &l.opts.Bucket})
	if err != nil {
		// a bucket without Object Lock answers with an error, not with "disabled"
		l.logger.Debug().Msgf("DBG310: no object lock configuration: %v", err)
		return info, nil
	}
	if c := lock.ObjectLockConfiguration; c != nil && c.ObjectLockEnabled == types.ObjectLockEnabledEnabled {
		info.ObjectLock = true
		if c.Rule != nil && c.Rule.DefaultRetention != nil {
			d := c.Rule.DefaultRetention
			switch {
			case d.Days != nil:
				info.Retention = fmt.Sprintf("%s %dd", d.Mode, *d.Days)
			case d.Years != nil:
				info.Retention = fmt.Sprintf("%s %dy", d.Mode, *d.Years)
			}
		}
	}
	return info, nil
}

// status is the HTTP status of a failed S3 call, 0 if err is nil or carries none.
func status(err error) int {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}

func readExactly(r io.Reader, n int64) ([]byte, error) {
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
