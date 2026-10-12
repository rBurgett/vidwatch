// Package media handles uploaded files in S3: presigned browser uploads,
// checking what was uploaded, public URLs, downloads and cleanup.
package media

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

var ErrNotFound = errors.New("object not found")

type Store struct {
	client  *s3.Client
	presign *s3.PresignClient
	bucket  string
	baseURL string // public URL prefix, e.g. https://media.vidwatch.burgett.dev
}

func New(client *s3.Client, bucket, baseURL string) *Store {
	return &Store{
		client:  client,
		presign: s3.NewPresignClient(client),
		bucket:  bucket,
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

// URL is the public URL of a key, or "" for no key.
func (m *Store) URL(key string) string {
	if key == "" {
		return ""
	}
	return m.baseURL + "/" + key
}

// Upload is what the browser needs to POST a file straight to S3.
type Upload struct {
	Key    string            `json:"key"`
	URL    string            `json:"url"`
	Fields map[string]string `json:"fields"`
}

// PresignUpload signs a browser POST upload for exactly this key, content
// type and size range. S3 rejects anything else.
func (m *Store) PresignUpload(ctx context.Context, key, contentType string, maxBytes int64) (*Upload, error) {
	req, err := m.presign.PresignPostObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(m.bucket),
		Key:    aws.String(key),
	}, func(o *s3.PresignPostOptions) {
		o.Expires = time.Hour
		o.Conditions = []any{
			map[string]string{"Content-Type": contentType},
			[]any{"content-length-range", 1, maxBytes},
		}
	})
	if err != nil {
		return nil, err
	}
	fields := req.Values
	fields["Content-Type"] = contentType
	return &Upload{Key: key, URL: req.URL, Fields: fields}, nil
}

// Object describes an uploaded file.
type Object struct {
	Size        int64
	ContentType string
}

func (m *Store) Head(ctx context.Context, key string) (*Object, error) {
	res, err := m.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(m.bucket), Key: aws.String(key)})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchKey") {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &Object{Size: aws.ToInt64(res.ContentLength), ContentType: aws.ToString(res.ContentType)}, nil
}

// Delete removes an object; deleting a missing key is not an error.
func (m *Store) Delete(ctx context.Context, key string) error {
	if key == "" {
		return nil
	}
	_, err := m.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(m.bucket), Key: aws.String(key)})
	return err
}

// DownloadURL is a short-lived URL that makes the browser save the object as filename.
func (m *Store) DownloadURL(ctx context.Context, key, filename string) (string, error) {
	// filename= is an ASCII fallback; filename*= carries the real (UTF-8) name.
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, filename)
	disposition := fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, url.PathEscape(filename))
	req, err := m.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket:                     aws.String(m.bucket),
		Key:                        aws.String(key),
		ResponseContentDisposition: aws.String(disposition),
	}, s3.WithPresignExpires(15*time.Minute))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}
