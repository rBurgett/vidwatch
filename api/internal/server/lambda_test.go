package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/aws/aws-lambda-go/events"
	"github.com/awslabs/aws-lambda-go-api-proxy/httpadapter"
)

// TestLambdaAdapter runs requests shaped like API Gateway HTTP API (payload
// v2) events through the same adapter main.go uses on Lambda.
func TestLambdaAdapter(t *testing.T) {
	srv := New(Config{ClientID: "client-1", CognitoDomain: "https://auth.example", OriginVerifySecret: "s3cret"},
		nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	proxy := httpadapter.NewV2(srv.Handler()).ProxyWithContext

	event := func(path string, headers map[string]string) events.APIGatewayV2HTTPRequest {
		return events.APIGatewayV2HTTPRequest{
			Version: "2.0", RouteKey: "ANY /api/{proxy+}", RawPath: path, Headers: headers,
			RequestContext: events.APIGatewayV2HTTPRequestContext{
				HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{Method: "GET", Path: path, SourceIP: "203.0.113.9"},
			},
		}
	}

	res, err := proxy(context.Background(), event("/api/config", map[string]string{"x-origin-verify": "s3cret"}))
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(res.Body), &cfg); err != nil || res.StatusCode != 200 || cfg["clientId"] != "client-1" {
		t.Fatalf("config: %d %q %v", res.StatusCode, res.Body, err)
	}

	if res, _ := proxy(context.Background(), event("/api/config", nil)); res.StatusCode != 403 {
		t.Fatalf("without origin header: %d", res.StatusCode)
	}
	if res, _ := proxy(context.Background(), event("/api/nope", map[string]string{"x-origin-verify": "s3cret"})); res.StatusCode != 404 {
		t.Fatalf("unknown route: %d %q", res.StatusCode, res.Body)
	}
}
