package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chwiee/mirante/internal/event"
)

// MCP em /mcp exigindo Bearer (como o ce-k8s-mcp).
func mcpComToken(t *testing.T, token string) *httptest.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "ce", Version: "0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "logs"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return nil, struct{}{}, nil
	})
	mcp.AddTool(s, &mcp.Tool{Name: "status", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
		return nil, struct{}{}, nil
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func topoDe(r *rig, server string) map[string]bool {
	out := map[string]bool{}
	for _, e := range r.srv.Store.Topology() {
		if e.Server == server {
			out[e.Agent+"/"+e.Tool] = true
		}
	}
	return out
}

// Relato de uso real: "não está mostrando o MCP ao subir". Agora o mirante
// descobre sozinho — server e tools no mapa antes de qualquer cliente.
func TestDescobertaMostraMCPAoSubir(t *testing.T) {
	up := mcpComToken(t, "s3cr3t")
	r := newRig(t, Config{})

	// sem token: não aparece (e não trava nada)
	ctx, cancel := context.WithCancel(context.Background())
	go r.px.Discover(ctx, "ce-k8s-mcp", up.URL, "")
	time.Sleep(300 * time.Millisecond)
	cancel()
	if len(topoDe(r, "ce-k8s-mcp")) != 0 {
		t.Fatal("sem token não deveria descobrir nada")
	}

	// com token, URL sem caminho: acha /mcp sozinho
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	go r.px.Discover(ctx, "ce-k8s-mcp", up.URL, "s3cr3t")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(topoDe(r, "ce-k8s-mcp")) < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	got := topoDe(r, "ce-k8s-mcp")
	if !got["/logs"] || !got["/status"] {
		t.Fatalf("topologia após descoberta: %v", got)
	}
	if runs, _ := r.srv.Store.Runs(); len(runs) != 0 {
		t.Fatalf("a descoberta não pode virar run/cliente no painel: %d runs", len(runs))
	}

	// cliente que não chama tools/list: a lista descoberta vale para checar
	// tool inventada (antes: sem lista, nada a checar)
	front := httptest.NewServer(r.px.Front("ce-k8s-mcp", up.URL))
	defer front.Close()
	hc := &http.Client{Transport: headerRT{"Authorization", "Bearer s3cr3t"}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "cliente-x", Version: "1"}, nil).
		Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: front.URL + "/mcp", HTTPClient: hc}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "reiniciar_tudo"})
	cs.Close()
	run := r.waitRun(t, func(x *event.Run) bool { return x.Agent == "cliente-x" && x.Status == "ok" })
	if len(run.Steps) == 0 || len(run.Steps[0].Flags) == 0 || run.Steps[0].Flags[0].Code != "tool_inexistente" {
		t.Fatalf("tool inventada não detectada com a lista descoberta: %+v", run.Steps)
	}
}

func TestDicasDaDescoberta(t *testing.T) {
	for msg, want := range map[string]string{
		"x: 401 Unauthorized":                    "MIRANTE_MCP_TOKEN",
		"dial tcp: connection refused":           "port-forward",
		"Post \"http://h/\": 404 page not found": "caminho",
	} {
		if got := discoverHint(errString(msg)); !strings.Contains(got, want) {
			t.Errorf("%q → %q (queria algo com %q)", msg, got, want)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }
