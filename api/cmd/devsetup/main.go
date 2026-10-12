// Command devsetup creates the DynamoDB table and media bucket in local
// emulators (DynamoDB Local and LocalStack) so the API can run offline.
// It reads the same .env as the API and refuses to touch real AWS.
//
//	go run ./cmd/devsetup
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/rBurgett/vidwatch/api/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "devsetup:", err)
		os.Exit(1)
	}
}

func run() error {
	loadDotEnv(".env")
	table, bucket := os.Getenv("TABLE_NAME"), os.Getenv("MEDIA_BUCKET")
	ddbEndpoint, s3Endpoint := os.Getenv("DYNAMODB_ENDPOINT"), os.Getenv("S3_ENDPOINT")
	if table == "" || bucket == "" {
		return errors.New("TABLE_NAME and MEDIA_BUCKET must be set (see .env.example)")
	}
	if ddbEndpoint == "" || s3Endpoint == "" {
		return errors.New("DYNAMODB_ENDPOINT and S3_ENDPOINT must point at local emulators; this tool never touches real AWS")
	}
	origin := os.Getenv("LOCAL_ORIGIN")
	if origin == "" {
		origin = "http://localhost:8080"
	}

	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}

	ddb := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) { o.BaseEndpoint = &ddbEndpoint })
	_, err = ddb.CreateTable(ctx, store.TableDefinition(table))
	var inUse *ddbtypes.ResourceInUseException
	switch {
	case errors.As(err, &inUse):
		fmt.Println("table", table, "already exists")
	case err != nil:
		return fmt.Errorf("create table: %w", err)
	default:
		fmt.Println("created table", table)
	}

	s3c := s3.NewFromConfig(cfg, func(o *s3.Options) { o.BaseEndpoint = &s3Endpoint; o.UsePathStyle = true })
	_, err = s3c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket})
	var owned *s3types.BucketAlreadyOwnedByYou
	switch {
	case errors.As(err, &owned):
		fmt.Println("bucket", bucket, "already exists")
	case err != nil:
		return fmt.Errorf("create bucket: %w", err)
	default:
		fmt.Println("created bucket", bucket)
	}
	// Browsers upload straight to the bucket, so it needs the same CORS rule
	// as the real one.
	_, err = s3c.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: &bucket,
		CORSConfiguration: &s3types.CORSConfiguration{CORSRules: []s3types.CORSRule{{
			AllowedMethods: []string{"POST", "GET", "HEAD"},
			AllowedOrigins: []string{origin},
			AllowedHeaders: []string{"*"},
			ExposeHeaders:  []string{"ETag"},
			MaxAgeSeconds:  aws.Int32(3000),
		}}},
	})
	if err != nil {
		return fmt.Errorf("bucket CORS: %w", err)
	}
	fmt.Println("allowed uploads from", origin)
	return nil
}

// loadDotEnv mirrors the API's: KEY=VALUE lines, existing variables win.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
			if _, set := os.LookupEnv(k); !set {
				os.Setenv(k, v)
			}
		}
	}
}
