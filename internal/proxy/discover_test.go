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

// mcpCom sobe um MCP server com as tools dadas (sem args, retorno vazio).
func mcpCom(t *testing.T, tools ...string) *httptest.Server {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "0"}, nil)
	for _, name := range tools {
		mcp.AddTool(s, &mcp.Tool{Name: name}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, struct{}, error) {
			return nil, struct{}{}, nil
		})
	}
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil))
	t.Cleanup(ts.Close)
	return ts
}

// Reproduz o lab do MidTS: o cliente chama tools/list só no mid-collector e
// depois chama buscar_incidentes no midts-conhecimento, sem listar ali. Antes,
// as 100 chamadas saíam como tool_inexistente ("não está entre as tools
// disponíveis (list_collectors, ...)"). Agora as tools descobertas do
// midts-conhecimento entram no run na 1ª chamada a ele.
func TestClienteSemToolsListNoServerUsaDescoberta(t *testing.T) {
	collector := mcpCom(t, "list_collectors", "get_queue_depth")
	conhecimento := mcpCom(t, "buscar_incidentes", "registrar_licao")
	r := newRig(t, Config{MCP: map[string]string{"mid-collector": collector.URL, "midts-conhecimento": conhecimento.URL}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.px.Discover(ctx, "midts-conhecimento", conhecimento.URL, "")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(topoDe(r, "midts-conhecimento")) < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if len(topoDe(r, "midts-conhecimento")) < 2 {
		t.Fatal("descoberta do midts-conhecimento não aconteceu")
	}

	connect := func(server string) *mcp.ClientSession {
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "midts", Version: "0"}, nil).
			Connect(ctx, &mcp.StreamableClientTransport{Endpoint: r.url + "/p/midts/mcp/" + server}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return cs
	}
	cs := connect("mid-collector")
	if _, err := cs.ListTools(ctx, nil); err != nil {
		t.Fatal(err)
	}
	cs.Close()

	cs = connect("midts-conhecimento") // sem ListTools de propósito
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "buscar_incidentes"}); err != nil {
		t.Fatal(err)
	}
	cs.CallTool(ctx, &mcp.CallToolParams{Name: "apagar_incidentes"})
	cs.Close()

	run := r.waitRun(t, func(x *event.Run) bool { return x.Agent == "midts" && x.Status == "ok" && len(x.Steps) == 2 })
	if f := run.Steps[0].Flags; len(f) != 0 {
		t.Fatalf("buscar_incidentes existe no midts-conhecimento e virou alerta: %+v", f)
	}
	if f := run.Steps[1].Flags; len(f) == 0 || f[0].Code != "tool_inexistente" {
		t.Fatalf("apagar_incidentes não existe no server e não foi sinalizada: %+v", f)
	}
	for _, e := range r.srv.Store.Topology() {
		if e.Agent == "midts" && e.Tool == "buscar_incidentes" && !e.Declared {
			t.Fatalf("tool real marcada como fantasma no mapa: %+v", e)
		}
	}
}
