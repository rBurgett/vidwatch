// Command vidwatch-api serves the vidwatch API. On AWS Lambda (behind API
// Gateway) it handles Lambda events; anywhere else it's a plain HTTP server.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/awslabs/aws-lambda-go-api-proxy/httpadapter"

	"github.com/rBurgett/vidwatch/api/internal/auth"
	"github.com/rBurgett/vidwatch/api/internal/media"
	"github.com/rBurgett/vidwatch/api/internal/server"
	"github.com/rBurgett/vidwatch/api/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	onLambda := os.Getenv("AWS_LAMBDA_RUNTIME_API") != ""
	if !onLambda {
		loadDotEnv(".env")
	}
	if err := run(log, onLambda); err != nil {
		log.Error("startup failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, onLambda bool) error {
	ctx := context.Background()
	env := func(name string) string { return strings.TrimSpace(os.Getenv(name)) }
	required := func(name string) (string, error) {
		if v := env(name); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("%s is required", name)
	}

	table, err := required("TABLE_NAME")
	if err != nil {
		return err
	}
	bucket, err := required("MEDIA_BUCKET")
	if err != nil {
		return err
	}
	mediaBase, err := required("MEDIA_BASE_URL")
	if err != nil {
		return err
	}
	clientID, err := required("COGNITO_CLIENT_ID")
	if err != nil {
		return err
	}
	maxUploadMB, _ := strconv.Atoi(env("MAX_UPLOAD_MB"))

	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}
	// Optional endpoint overrides for running against DynamoDB Local / MinIO.
	ddb := dynamodb.NewFromConfig(awsCfg, func(o *dynamodb.Options) {
		if ep := env("DYNAMODB_ENDPOINT"); ep != "" {
			o.BaseEndpoint = &ep
		}
	})
	s3c := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if ep := env("S3_ENDPOINT"); ep != "" {
			o.BaseEndpoint = &ep
			o.UsePathStyle = true
		}
	})

	// AUTH_ISSUER / AUTH_JWKS_URL override Cognito (tests and local fakes).
	var verifier server.Verifier
	if issuer := env("AUTH_ISSUER"); issuer != "" {
		verifier = auth.NewVerifier(issuer, env("AUTH_JWKS_URL"), clientID)
	} else {
		poolID, err := required("COGNITO_USER_POOL_ID")
		if err != nil {
			return err
		}
		verifier = auth.NewCognitoVerifier(awsCfg.Region, poolID, clientID)
	}

	srv := server.New(server.Config{
		CognitoDomain:      env("COGNITO_DOMAIN"),
		ClientID:           clientID,
		MaxUploadMB:        maxUploadMB,
		OriginVerifySecret: env("ORIGIN_VERIFY_SECRET"),
		StaticDir:          env("STATIC_DIR"),
	}, store.New(ddb, table), media.New(s3c, bucket, mediaBase), verifier, log)
	handler := srv.Handler()

	if onLambda {
		lambda.Start(httpadapter.NewV2(handler).ProxyWithContext)
		return nil
	}
	addr := ":" + cmp(env("PORT"), "8080")
	log.Info("vidwatch API listening", "addr", "http://localhost"+addr)
	hs := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	return hs.ListenAndServe()
}

func cmp(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// loadDotEnv sets KEY=VALUE lines from a local .env file (if there is one)
// without overriding variables that are already set.
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
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
}
