// mcp-sessoes diagnostica se um MCP server (Streamable HTTP) aguenta sessões
// do jeito que está exposto: abre N sessões (initialize + 5 chamadas de tool)
// e conta quantas falham.
//
// Para que serve: MCP stateful (padrão do go-sdk) com várias réplicas atrás
// de um Service do Kubernetes falha de forma intermitente com "session not
// found" — a sessão vive na réplica que fez o initialize e o kube-proxy
// distribui por conexão. Medido em kind: 13% de falha com 2 réplicas, 17%
// matando um pod no meio; 0% e 1 (a sessão em andamento no pod morto) com o
// mirante na frente, apontando para o Service headless.
//
// Uso (de dentro do cluster, para passar pelo kube-proxy):
//
//	mcp-sessoes <endpoint MCP> [sessões=30] [tool=primeira de tools/list]
//	mcp-sessoes http://hub-server-mcp.k8s-ts-mcp.svc:8443 30
//	mcp-sessoes http://mirante.observabilidade:8080/p/diagnostico/mcp/k8s-ts-mcp 30
//
// Para MCP com autenticação, defina MCP_BEARER_TOKEN.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "uso: mcp-sessoes <endpoint MCP> [sessões=30] [tool]")
		os.Exit(2)
	}
	endpoint, n, tool := os.Args[1], 30, ""
	if len(os.Args) > 2 {
		n, _ = strconv.Atoi(os.Args[2])
	}
	if len(os.Args) > 3 {
		tool = os.Args[3]
	}
	hc := http.DefaultClient
	if t := os.Getenv("MCP_BEARER_TOKEN"); t != "" {
		hc = &http.Client{Transport: bearer{t}}
	}

	ctx := context.Background()
	ok, falhas := 0, 0
	var primeiroErro error
	for i := 0; i < n; i++ {
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "mcp-sessoes", Version: "1"}, nil).
			Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc}, nil)
		if err == nil && tool == "" {
			var lt *mcp.ListToolsResult
			if lt, err = cs.ListTools(ctx, nil); err == nil && len(lt.Tools) > 0 {
				tool = lt.Tools[0].Name
			}
		}
		for j := 0; err == nil && tool != "" && j < 5; j++ {
			_, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{}})
		}
		if err != nil {
			falhas++
			if primeiroErro == nil {
				primeiroErro = err
			}
		} else {
			ok++
		}
		if cs != nil {
			cs.Close()
		}
	}
	fmt.Printf("RESULTADO endpoint=%s tool=%s sessoes_ok=%d sessoes_falhas=%d\n", endpoint, tool, ok, falhas)
	if primeiroErro != nil {
		fmt.Printf("primeiro erro: %v\n", primeiroErro)
	}
	if falhas > 0 {
		fmt.Println("→ falhas com \"session not found\" = MCP stateful com várias réplicas sem afinidade de sessão.")
		fmt.Println("  Solução: mirante na frente, apontando para o Service headless (docs/IMPLANTACAO.md, seção 6).")
		fmt.Println("  Se o erro for de CONEXÃO (timeout/refused), não é sessão: é NetworkPolicy bloqueando a porta.")
		os.Exit(1)
	}
}
