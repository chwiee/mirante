package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chwiee/mirante/internal/event"
)

// MCP real servido em /mcp (como o ce-k8s-mcp), com /healthz próprio.
func mcpEmMCP(t *testing.T) *httptest.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "eco", Version: "0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "scan_cluster"}, func(_ context.Context, _ *mcp.CallToolRequest, in scanIn) (*mcp.CallToolResult, scanOut, error) {
		return nil, scanOut{Pods: []string{"api-1"}}, nil
	})
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "mcp-ok") })
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// O cliente usa a MESMA URL de sempre (/mcp); o nome dele vem do initialize.
func TestFrontTransparenteIdentificaCliente(t *testing.T) {
	up := mcpEmMCP(t)
	r := newRig(t, Config{})
	front := httptest.NewServer(r.px.Front("eco", up.URL+"/qualquer-caminho-ignorado"))
	defer front.Close()

	ctx := context.Background()
	for _, nome := range []string{"claude-code", "Kiro IDE"} {
		cs, err := mcp.NewClient(&mcp.Implementation{Name: nome, Version: "1"}, nil).
			Connect(ctx, &mcp.StreamableClientTransport{Endpoint: front.URL + "/mcp"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		cs.ListTools(ctx, nil)
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "scan_cluster", Arguments: map[string]any{"cluster_id": "a"}})
		if err != nil || res.IsError {
			t.Fatalf("%s: chamada pelo front: %v %+v", nome, err, res)
		}
		cs.Close()
	}
	for _, want := range []string{"claude-code", "Kiro-IDE"} {
		run := r.waitRun(t, func(x *event.Run) bool { return x.Agent == want && x.Status == "ok" })
		if len(run.Steps) != 1 || run.Steps[0].Server != "eco" || !strings.Contains(string(run.Steps[0].Result), "api-1") {
			t.Fatalf("%s: run %+v", want, run.Steps)
		}
	}

	// rotas que não são MCP passam direto (ex: /healthz do próprio MCP)
	resp, err := http.Get(front.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "mcp-ok" {
		t.Fatalf("/healthz do MCP não passou pelo front: %q", b)
	}
}

func TestFrontHeaderDeAgenteVence(t *testing.T) {
	up := mcpEmMCP(t)
	r := newRig(t, Config{})
	front := httptest.NewServer(r.px.Front("eco", up.URL))
	defer front.Close()
	hc := &http.Client{Transport: headerRT{"X-Mirante-Agent", "time-sre-bot"}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "generico", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: front.URL + "/mcp", HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "scan_cluster", Arguments: map[string]any{"cluster_id": "a"}})
	cs.Close()
	r.waitRun(t, func(x *event.Run) bool { return x.Agent == "time-sre-bot" && x.Status == "ok" })
}

type headerRT struct{ k, v string }

func (h headerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(h.k, h.v)
	return http.DefaultTransport.RoundTrip(r)
}

func TestCleanAgent(t *testing.T) {
	for in, want := range map[string]string{
		"claude-code": "claude-code", "Kiro IDE": "Kiro-IDE", " x/y:z ": "x-y-z",
		"<script>": "script", strings.Repeat("a", 200): strings.Repeat("a", 64), "": "",
	} {
		if got := cleanAgent(in); got != want {
			t.Errorf("cleanAgent(%q) = %q, quer %q", in, got, want)
		}
	}
	if uaProduct("claude-code/1.2 (linux)") != "claude-code" {
		t.Error("uaProduct")
	}
	_ = time.Second
}
