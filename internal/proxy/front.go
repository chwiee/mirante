package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Front é o modo "na frente do MCP": o mirante escuta no lugar do MCP server,
// na MESMA URL que os clientes já usam (o Ingress/Service passa a apontar
// para ele), repassa tudo igual e observa no caminho. Os clientes não mudam
// nada — é para quem não tem agente próprio: o MCP é usado por Claude Code,
// Kiro, outros times…
//
// Sem /p/<agente>/ na URL, o nome do cliente vem, nesta ordem, de:
//  1. header X-Mirante-Agent (quem quiser se identificar melhor);
//  2. clientInfo.name do initialize (todo cliente MCP manda: "claude-code"…),
//     lembrado por Mcp-Session-Id para o resto da sessão;
//  3. o produto do User-Agent (MCP sem sessão);
//  4. "desconhecido".
//
// O caminho da requisição é preservado: o upstream é só esquema://host:porta
// (o caminho dele, se houver, é ignorado).
func (p *Proxy) Front(server, upstream string) http.Handler {
	origin := upstream
	if u, err := url.Parse(upstream); err == nil {
		u.Path, u.RawPath, u.RawQuery = "", "", ""
		origin = u.String()
	}
	sessions := &frontSessions{m: map[string]string{}}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sid := r.Header.Get("Mcp-Session-Id")
		agent := cleanAgent(r.Header.Get("X-Mirante-Agent"))
		initName := ""
		if r.Method == http.MethodPost {
			body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			initName = cleanAgent(clientName(body))
		}
		if agent == "" {
			agent = sessions.get(sid)
		}
		if agent == "" {
			agent = initName
		}
		if agent == "" {
			agent = cleanAgent(uaProduct(r.UserAgent()))
		}
		if agent == "" {
			agent = "desconhecido"
		}

		after := func(resp *http.Response) {
			switch {
			case r.Method == http.MethodDelete:
				sessions.del(sid)
			case initName != "" || r.Header.Get("X-Mirante-Agent") != "":
				if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
					sessions.set(s, agent)
				}
			}
		}
		p.serveMCP(w, r, agent, server, origin, strings.TrimPrefix(r.URL.Path, "/"), after)
	})
}

// clientName extrai clientInfo.name de um initialize (objeto ou lote).
func clientName(body []byte) string {
	if !bytes.Contains(body, []byte(`"initialize"`)) {
		return "" // só o 1º POST da sessão tem; os demais nem são decodificados aqui
	}
	for _, m := range decodeRPC(body) {
		if m.Method != "initialize" {
			continue
		}
		var prm struct {
			ClientInfo struct {
				Name string `json:"name"`
			} `json:"clientInfo"`
		}
		if json.Unmarshal(m.Params, &prm) == nil && prm.ClientInfo.Name != "" {
			return prm.ClientInfo.Name
		}
	}
	return ""
}

// uaProduct: "claude-code/1.2.3 (…)" → "claude-code".
func uaProduct(ua string) string {
	ua = strings.TrimSpace(ua)
	if i := strings.IndexAny(ua, "/ "); i > 0 {
		ua = ua[:i]
	}
	return ua
}

// cleanAgent deixa o nome seguro para o painel: sem espaços/controle, curto.
func cleanAgent(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ' || r == '/' || r == ':':
			b.WriteRune('-')
		}
		if b.Len() >= 64 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

type frontSessions struct {
	mu sync.Mutex
	m  map[string]string
}

func (f *frontSessions) get(sid string) string {
	if sid == "" {
		return ""
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.m[sid]
}

func (f *frontSessions) set(sid, agent string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.m) >= maxSessions {
		f.m = map[string]string{} // proteção de memória; sessões antigas viram "desconhecido"
	}
	f.m[sid] = agent
}

func (f *frontSessions) del(sid string) {
	if sid == "" {
		return
	}
	f.mu.Lock()
	delete(f.m, sid)
	f.mu.Unlock()
}
