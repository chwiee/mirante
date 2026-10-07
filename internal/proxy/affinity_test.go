package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type quemOut struct {
	Replica string `json:"replica"`
}

// réplica de MCP server stateful (opções padrão do go-sdk, como o
// hub-server do k8s-ts-mcp e o ce-k8s-mcp).
func replicaMCP(t *testing.T, name string) *httptest.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "eco", Version: "0"}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "quem"}, func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, quemOut, error) {
		return nil, quemOut{name}, nil
	})
	// Aqui os "pods" escutam em 127.0.0.1, e o go-sdk (>= 1.4) recusa com 403
	// requisição que chega por loopback com Host não-localhost (proteção
	// contra DNS rebinding). No Kubernetes o mirante chega pelo IP do pod, não
	// por loopback, então a proteção não se aplica — desligá-la só no teste
	// reproduz o cenário real.
	ts := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{DisableLocalhostProtection: true}))
	t.Cleanup(ts.Close)
	return ts
}

// kubeProxy imita o Service ClusterIP: balanceia por CONEXÃO TCP.
func kubeProxy(t *testing.T, backends ...string) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var n atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				up, err := net.Dial("tcp", backends[n.Add(1)%int64(len(backends))])
				if err != nil {
					c.Close()
					return
				}
				go func() { io.Copy(up, c); up.Close() }()
				io.Copy(c, up)
				c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// sessões MCP completas (connect + 5 chamadas); devolve quantas falharam.
func rodarSessoes(t *testing.T, endpoint string, n int) (falhas int) {
	f, _ := rodarSessoesR(t, endpoint, n)
	return f
}

func rodarSessoesR(t *testing.T, endpoint string, n int) (falhas int, replicas map[string]int) {
	replicas = map[string]int{}
	ctx := context.Background()
	for i := 0; i < n; i++ {
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "cliente", Version: "0"}, nil).
			Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint}, nil)
		if err != nil {
			falhas++
			continue
		}
		var fixa string
		for j := 0; j < 5; j++ {
			res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "quem"})
			if err != nil {
				falhas++
				break
			}
			r := res.StructuredContent.(map[string]any)["replica"].(string)
			if fixa == "" {
				fixa = r
				replicas[r]++
			} else if r != fixa {
				t.Errorf("sessão pulou de réplica: %s → %s", fixa, r)
			}
		}
		cs.Close()
	}
	return falhas, replicas
}

func TestAfinidadeDeSessaoMCP(t *testing.T) {
	a, b := replicaMCP(t, "A"), replicaMCP(t, "B")
	pods := []string{a.Listener.Addr().String(), b.Listener.Addr().String()}

	// controle: o problema existe sem afinidade (Service ClusterIP comum)
	vip := kubeProxy(t, pods...)
	if f := rodarSessoes(t, "http://"+vip, 10); f == 0 {
		t.Fatal("controle deveria falhar: 2 réplicas stateful atrás de balanceamento por conexão")
	}

	// mirante com upstream "headless": DNS devolve os dois pods
	r := newRig(t, Config{
		MCP:    map[string]string{"eco": "http://eco-headless.ns.svc:8080"},
		lookup: func(context.Context, string) ([]string, error) { return pods, nil },
	})
	f, reps := rodarSessoesR(t, r.url+"/p/agente/mcp/eco", 20)
	if f != 0 {
		t.Fatalf("com afinidade, %d sessões falharam", f)
	}
	// as sessões se distribuíram entre os pods (não ficou tudo num só)
	if reps["A"] == 0 || reps["B"] == 0 {
		t.Errorf("sessões deveriam se espalhar pelos 2 pods: %v", reps)
	}
	// cliente fechou as sessões (DELETE): o mirante esquece o vínculo
	if n := len(r.px.aff["eco"].sessions); n != 0 {
		t.Errorf("%d vínculos sessão→pod sobraram após o DELETE das sessões", n)
	}
}

// Pod morre: as sessões dele são esquecidas e o cliente reabre sessão
// em outro pod (404 → novo initialize, como a spec MCP prevê).
func TestAfinidadePodMorre(t *testing.T) {
	a, b := replicaMCP(t, "A"), replicaMCP(t, "B")
	pods := []string{a.Listener.Addr().String(), b.Listener.Addr().String()}
	vivos := func() []string { return pods }
	r := newRig(t, Config{
		MCP:    map[string]string{"eco": "http://eco-headless.ns.svc:8080"},
		lookup: func(context.Context, string) ([]string, error) { return vivos(), nil },
	})
	aff := r.px.affinityFor("eco", "http://eco-headless.ns.svc:8080")
	aff.ttl = 0 // reconsulta o DNS a cada requisição

	if f := rodarSessoes(t, r.url+"/p/agente/mcp/eco", 4); f != 0 {
		t.Fatalf("antes: %d falhas", f)
	}
	a.Close()
	pods = []string{b.Listener.Addr().String()} // DNS headless já não lista o pod A
	if f := rodarSessoes(t, r.url+"/p/agente/mcp/eco", 6); f != 0 {
		t.Fatalf("depois de o pod A morrer: %d falhas", f)
	}
	for sid, p := range aff.sessions {
		if p == pods[0] {
			continue
		}
		t.Errorf("sessão %s ainda presa ao pod morto %s", sid, p)
	}
}

func TestAfinidadeNaoMexeEmServiceComum(t *testing.T) {
	for _, up := range []string{"https://hub.empresa.com/mcp", "http://10.0.0.5:8443/mcp"} {
		if a := newAffinity(up); !a.disabled {
			t.Errorf("%s: afinidade deveria ficar desligada", up)
		}
	}
	if !strings.Contains(newAffinity("http://x:1/mcp").target("http://x:1/mcp", ""), "x:1") {
		t.Error("sem backend o upstream fica igual")
	}
}

// Destino trocado pelo IP, mas o Host original preservado: um ALB/Ingress
// que roteia por nome continua funcionando (e o pod nem olha o Host).
func TestAfinidadePreservaHost(t *testing.T) {
	var gotHost atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost.Store(r.Host)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	defer up.Close()
	r := newRig(t, Config{
		MCP:    map[string]string{"x": "http://mcp-interno.empresa.com:8443/mcp"},
		lookup: func(context.Context, string) ([]string, error) { return []string{up.Listener.Addr().String()}, nil },
	})
	resp, err := http.Post(r.url+"/p/a/mcp/x", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if h, _ := gotHost.Load().(string); h != "mcp-interno.empresa.com:8443" {
		t.Fatalf("Host recebido pelo upstream = %q, quer o nome original", h)
	}
}

// Pod morreu mas o DNS headless ainda o lista (atraso de propagação): no
// máximo a primeira sessão nova falha; o pod vai para quarentena e as
// seguintes vão para o pod vivo.
func TestAfinidadeQuarentenaDePodMorto(t *testing.T) {
	a, b := replicaMCP(t, "A"), replicaMCP(t, "B")
	stale := []string{a.Listener.Addr().String(), b.Listener.Addr().String()}
	r := newRig(t, Config{
		MCP:    map[string]string{"eco": "http://eco-headless.ns.svc:8080"},
		lookup: func(context.Context, string) ([]string, error) { return stale, nil }, // DNS desatualizado
	})
	r.px.affinityFor("eco", "http://eco-headless.ns.svc:8080").ttl = 0
	a.Close()
	f, reps := rodarSessoesR(t, r.url+"/p/agente/mcp/eco", 20)
	if f > 1 {
		t.Fatalf("%d sessões falharam com o pod morto ainda no DNS (máximo aceitável: 1)", f)
	}
	if reps["A"] != 0 || reps["B"] < 19 {
		t.Fatalf("distribuição: %v", reps)
	}
}
