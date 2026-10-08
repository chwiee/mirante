package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/chwiee/mirante/internal/event"
)

// Descoberta: sem ela, o mirante só fica sabendo que um MCP existe quando um
// cliente passa e faz tools/list — ao subir, o mapa ficava vazio (relato de
// uso real). Aqui o próprio mirante se conecta ao MCP como cliente, lista as
// tools e publica server+tools no mapa (sem agente ligado), e repete de tempos
// em tempos para acompanhar tools novas.
//
// A sessão de descoberta fala DIRETO com o upstream (não passa pelo proxy),
// então não vira run nem aparece como cliente no painel.

const (
	discoverEvery = 5 * time.Minute
	discoverRetry = 15 * time.Second
)

// Discover roda em segundo plano até ctx acabar. token (opcional) vai como
// "Authorization: Bearer". rawURL pode trazer o caminho do MCP (ex: /mcp);
// sem caminho, tenta "/" e depois "/mcp".
func (p *Proxy) Discover(ctx context.Context, server, rawURL, token string) {
	hc := &http.Client{Timeout: 15 * time.Second, Transport: p.transport}
	if token != "" {
		hc.Transport = bearerRT{token: token, base: p.transport}
	}
	lastErr, announced := "", false
	for {
		specs, endpoint, err := discoverOnce(ctx, hc, rawURL)
		wait := discoverEvery
		if err != nil {
			wait = discoverRetry
			if msg := err.Error(); msg != lastErr { // não repete o mesmo aviso a cada 15s
				p.cfg.Log.Warn("descoberta do MCP falhou: o server só vai aparecer no mapa quando um cliente passar",
					"server", server, "url", rawURL, "motivo", msg, "dica", discoverHint(err))
				lastErr = msg
			}
		} else {
			if !announced || lastErr != "" {
				p.cfg.Log.Info("MCP descoberto", "server", server, "endpoint", endpoint, "tools", len(specs))
				announced = true
			}
			lastErr = ""
			p.corr.mcpDiscovered(server, specs)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func discoverOnce(ctx context.Context, hc *http.Client, rawURL string) ([]event.ToolSpec, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, "", err
	}
	candidates := []string{rawURL}
	if u.Path == "" || u.Path == "/" {
		base := strings.TrimRight(rawURL, "/")
		candidates = []string{base + "/", base + "/mcp"}
	}
	var firstErr error
	for _, endpoint := range candidates {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		cs, err := mcp.NewClient(&mcp.Implementation{Name: "mirante-descoberta", Version: "1"}, nil).
			Connect(cctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc}, nil)
		if err == nil {
			var res *mcp.ListToolsResult
			res, err = cs.ListTools(cctx, nil)
			cs.Close()
			if err == nil {
				cancel()
				return specsFromTools(res.Tools), endpoint, nil
			}
		}
		cancel()
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %w", endpoint, err)
		}
	}
	return nil, "", firstErr
}

func specsFromTools(tools []*mcp.Tool) []event.ToolSpec {
	out := make([]event.ToolSpec, 0, len(tools))
	for _, t := range tools {
		spec := event.ToolSpec{Name: t.Name, Description: t.Description}
		if m, ok := t.InputSchema.(map[string]any); ok {
			spec.Schema = m
		}
		if a := t.Annotations; a != nil {
			switch {
			case a.DestructiveHint != nil && *a.DestructiveHint:
				ro := false
				spec.ReadOnly = &ro
			case a.ReadOnlyHint:
				ro := true
				spec.ReadOnly = &ro
			}
		}
		out = append(out, spec)
	}
	return out
}

// discoverHint traduz os erros mais comuns em ação.
func discoverHint(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "401") || strings.Contains(s, "403") || strings.Contains(s, "unauthorized") || strings.Contains(s, "forbidden"):
		return "o MCP exige token: defina MIRANTE_MCP_TOKEN=<server>=<token> (só para a descoberta; os clientes continuam mandando o próprio token)"
	case strings.Contains(s, "connection refused") || strings.Contains(s, "recusou") || strings.Contains(s, "no such host"):
		return "o mirante não alcança o MCP: confira a URL/porta (e o port-forward, se estiver testando local)"
	case strings.Contains(s, "404"):
		return "caminho errado: informe o caminho do MCP na URL (ex: http://host:porta/mcp)"
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(s, "timeout"):
		return "o MCP não respondeu a tempo: NetworkPolicy bloqueando a porta?"
	}
	return "confira a URL do MCP"
}

type bearerRT struct {
	token string
	base  http.RoundTripper
}

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}
