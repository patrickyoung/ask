package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// Google Vertex AI serves Anthropic models on its own wire shape: the
// model moves from the request body into the URL path, the body gains
// anthropic_version, and the endpoint is a per-region Google host
// instead of api.anthropic.com. ask follows Claude Code's environment
// mapping, so a machine configured for one works for the other:
//
//	ANTHROPIC_VERTEX_PROJECT_ID  GCP project (presence enables Vertex)
//	CLOUD_ML_REGION              required: global, us, eu, or a
//	                             specific region like us-east5
//	ANTHROPIC_VERTEX_BASE_URL    optional: a gateway in place of the
//	                             region-derived Google endpoint
//
// ask never holds Google credentials: auth is whatever the invocation's
// descriptor-backed transport carries, or what a proxy injects. The rewrite
// happens below the logging layer,
// so request events keep the normalized shape and replay is untouched.

const vertexVersion = "vertex-2023-10-16"

// vertexOptions returns the client options that reshape Anthropic
// requests for Vertex, or nil when ANTHROPIC_VERTEX_PROJECT_ID is unset.
func vertexOptions() ([]option.RequestOption, error) {
	project := os.Getenv("ANTHROPIC_VERTEX_PROJECT_ID")
	if project == "" {
		return nil, nil
	}
	region := os.Getenv("CLOUD_ML_REGION")
	if region == "" {
		return nil, fmt.Errorf("ANTHROPIC_VERTEX_PROJECT_ID is set but CLOUD_ML_REGION is not")
	}
	base := os.Getenv("ANTHROPIC_VERTEX_BASE_URL")
	if base == "" {
		base = vertexBase(region)
	}
	return []option.RequestOption{
		option.WithBaseURL(base),
		option.WithMiddleware(vertexRewrite(project, region)),
	}, nil
}

// vertexBase maps a region to its Vertex endpoint the way Claude Code
// and the vendor SDKs do: global and the two multi-regions have fixed
// hosts; anything else is a regional host.
func vertexBase(region string) string {
	switch region {
	case "global":
		return "https://aiplatform.googleapis.com/"
	case "us":
		return "https://aiplatform.us.rep.googleapis.com/"
	case "eu":
		return "https://aiplatform.eu.rep.googleapis.com/"
	}
	return fmt.Sprintf("https://%s-aiplatform.googleapis.com/", region)
}

// vertexRewrite reshapes POST /v1/messages — the one endpoint ask calls —
// into Vertex form: model out of the body and into the path,
// anthropic_version into the body, :streamRawPredict or :rawPredict by
// the stream flag. A gateway prefix on the base URL survives the
// rewrite; dated Vertex model ids (claude-sonnet-4-5@20250929) pass
// through the path unescaped, as Vertex expects.
func vertexRewrite(project, region string) option.Middleware {
	return func(r *http.Request, next option.MiddlewareNext) (*http.Response, error) {
		prefix, ok := strings.CutSuffix(r.URL.Path, "/v1/messages")
		if !ok || r.Method != http.MethodPost || r.Body == nil {
			return next(r)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body.Close()
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			return nil, fmt.Errorf("vertex: %w", err)
		}
		var model string
		json.Unmarshal(body["model"], &model)
		if model == "" {
			return nil, fmt.Errorf("vertex: request carries no model")
		}
		delete(body, "model")
		if _, ok := body["anthropic_version"]; !ok {
			body["anthropic_version"] = json.RawMessage(`"` + vertexVersion + `"`)
		}
		verb := "rawPredict"
		if string(body["stream"]) == "true" {
			verb = "streamRawPredict"
		}
		r.URL.Path = fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:%s",
			prefix, project, region, model, verb)
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
		buf := bytes.NewReader(raw)
		r.Body = io.NopCloser(buf)
		r.GetBody = func() (io.ReadCloser, error) {
			_, err := buf.Seek(0, io.SeekStart)
			return io.NopCloser(buf), err
		}
		r.ContentLength = int64(len(raw))
		return next(r)
	}
}
