package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestVertexBase pins the region → endpoint table Claude Code and the
// vendor SDKs share: fixed hosts for global and the two multi-regions,
// a regional host for everything else.
func TestVertexBase(t *testing.T) {
	for region, want := range map[string]string{
		"global":       "https://aiplatform.googleapis.com/",
		"us":           "https://aiplatform.us.rep.googleapis.com/",
		"eu":           "https://aiplatform.eu.rep.googleapis.com/",
		"us-east5":     "https://us-east5-aiplatform.googleapis.com/",
		"europe-west1": "https://europe-west1-aiplatform.googleapis.com/",
	} {
		if got := vertexBase(region); got != want {
			t.Errorf("vertexBase(%q) = %q, want %q", region, got, want)
		}
	}
}

// TestVertexEnv pins enablement: off when the project is unset, a loud
// error when the project is set without a region.
func TestVertexEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "")
	if opts, err := vertexOptions(); opts != nil || err != nil {
		t.Fatalf("without project: opts=%v err=%v, want nil, nil", opts, err)
	}

	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "proj")
	t.Setenv("CLOUD_ML_REGION", "")
	if _, err := vertexOptions(); err == nil {
		t.Fatal("project without CLOUD_ML_REGION must error, got nil")
	}

	t.Setenv("CLOUD_ML_REGION", "us-east5")
	opts, err := vertexOptions()
	if err != nil || len(opts) != 2 {
		t.Fatalf("project+region: %d opts, err=%v, want 2, nil", len(opts), err)
	}
}

// TestVertexGatewayEndToEnd drives the whole Claude Code mapping through
// the environment: OAuth bearer from ASK_AUTH_URL, ANTHROPIC_VERTEX_BASE_URL
// pointing at a prefixed gateway, and a dated Vertex model id. The wire
// must show the Vertex shape — model in the path, anthropic_version in
// the body — while the stream still honors the provider contract.
func TestVertexGatewayEndToEnd(t *testing.T) {
	tok, _, _ := tokenServer(t, 3600)
	var gotPath, gotAuth string
	var gotBody map[string]json.RawMessage
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &gotBody); err != nil {
			t.Errorf("gateway body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, anthropicWire)
	}))
	defer gw.Close()

	t.Setenv("ASK_AUTH_URL", tok.URL)
	t.Setenv("ASK_AUTH_CLIENT_ID", "cid")
	t.Setenv("ASK_AUTH_CLIENT_SECRET", "shh")
	t.Setenv("ASK_AUTH_REFRESH_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "") // Vertex has no vendor key
	t.Setenv("ANTHROPIC_BASE_URL", "https://ignored.example")
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "proj")
	t.Setenv("CLOUD_ML_REGION", "us-east5")
	t.Setenv("ANTHROPIC_VERTEX_BASE_URL", gw.URL+"/vertex")

	p, model, err := New("anthropic/claude-sonnet-4-5@20250929")
	if err != nil {
		t.Fatalf("New on Vertex without a vendor key: %v", err)
	}
	req := Request{Model: model, MaxTokens: 16, Messages: []Message{{Role: User, Blocks: []Block{{Type: Text, Text: "hi"}}}}}
	d := checkContract(t, p.Stream(context.Background(), req))
	if d.stop != "end" {
		t.Fatalf("stop = %q, want end", d.stop)
	}

	want := "/vertex/v1/projects/proj/locations/us-east5/publishers/anthropic/models/claude-sonnet-4-5@20250929:streamRawPredict"
	if gotPath != want {
		t.Errorf("wire path = %q\nwant        %q", gotPath, want)
	}
	if gotAuth != "Bearer tok-1" {
		t.Errorf("wire auth = %q, want the gateway bearer", gotAuth)
	}
	if _, ok := gotBody["model"]; ok {
		t.Error("model must move out of the body and into the path")
	}
	if v := string(gotBody["anthropic_version"]); v != `"`+vertexVersion+`"` {
		t.Errorf("anthropic_version = %s, want %q", v, vertexVersion)
	}
	if string(gotBody["stream"]) != "true" {
		t.Errorf("stream = %s, want true (ask always streams)", gotBody["stream"])
	}
}

// TestVertexRewriteNonStreaming pins the :rawPredict verb and the
// bare-region path (no gateway prefix), matching the SDK's own vertex
// middleware behavior.
func TestVertexRewriteNonStreaming(t *testing.T) {
	var gotPath string
	mw := vertexRewrite("proj", "global")
	r := httptest.NewRequest(http.MethodPost, "https://aiplatform.googleapis.com/v1/messages",
		bytes.NewReader([]byte(`{"model":"claude-opus-4-8","max_tokens":1}`)))
	_, err := mw(r, func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "/v1/projects/proj/locations/global/publishers/anthropic/models/claude-opus-4-8:rawPredict"
	if gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}
