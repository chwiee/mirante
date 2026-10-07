package proxy

import (
	"context"
	"errors"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"
)

// affinity resolve o problema clássico de MCP Streamable HTTP com várias
// réplicas: a sessão (Mcp-Session-Id) vive na memória da réplica que fez o
// initialize, mas o kube-proxy distribui por conexão — e o cliente MCP abre
// mais de uma (stream GET + POSTs). Resultado: "session not found"
// intermitente. Testado: 2 réplicas go-sdk atrás de balanceamento por
// conexão = 20/20 sessões quebradas.
//
// Com o upstream apontando para um Service *headless* (o DNS devolve o IP de
// cada pod), o mirante escolhe um pod no initialize, guarda sessão → pod e
// manda o resto daquela sessão para o mesmo pod. Pod que some do DNS =
// sessões dele esquecidas; a próxima requisição recebe 404 do pod novo e o
// cliente reabre a sessão (comportamento previsto na spec MCP).
//
// O header Host original é sempre preservado (ALB/Ingress roteiam por ele),
// então upstream com 1 IP só (Service ClusterIP, host externo) segue igual.
// Upstream https = desligado (o certificado não valeria para o IP do pod).
type affinity struct {
	mu         sync.Mutex
	scheme     string
	host       string // como no upstream (DNS)
	port       string
	backends   []string // ip:porta atuais
	at         time.Time
	sessions   map[string]string    // Mcp-Session-Id → ip:porta
	bad        map[string]time.Time // pods que recusaram conexão (quarentena)
	refreshing bool                 // consulta DNS em andamento (em segundo plano)
	ttl        time.Duration
	lookup     func(ctx context.Context, host string) ([]string, error)
	disabled   bool
}

const (
	maxSessions = 50000
	quarantine  = 30 * time.Second
)

func newAffinity(upstream string) *affinity {
	a := &affinity{sessions: map[string]string{}, bad: map[string]time.Time{}, ttl: 3 * time.Second, lookup: net.DefaultResolver.LookupHost}
	u, err := url.Parse(upstream)
	if err != nil || u.Scheme != "http" || net.ParseIP(u.Hostname()) != nil || u.Hostname() == "localhost" {
		a.disabled = true // https, IP literal, localhost (sidecar) ou URL inválida: não há o que fixar
		return a
	}
	a.scheme, a.host, a.port = u.Scheme, u.Hostname(), u.Port()
	if a.port == "" {
		a.port = "80"
	}
	return a
}

// refresh atualiza a lista de pods (no máximo a cada ttl). Chamado com mu.
//
// A consulta DNS acontece FORA do lock e, depois da primeira, em segundo
// plano: nenhuma requisição espera DNS (nem fica presa atrás de outra que
// está esperando). Só a primeira requisição, sem lista nenhuma, espera.
func (a *affinity) refresh() {
	if len(a.backends) == 0 {
		a.mu.Unlock()
		a.resolve()
		a.mu.Lock()
		return
	}
	if time.Since(a.at) >= a.ttl && !a.refreshing {
		a.refreshing = true
		go a.resolve()
	}
}

// resolve consulta o DNS e aplica a lista nova. Não pode ser chamado com mu.
func (a *affinity) resolve() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	ips, err := a.lookup(ctx, a.host)
	cancel()

	a.mu.Lock()
	defer a.mu.Unlock()
	a.refreshing = false
	for b, t := range a.bad {
		if time.Since(t) >= quarantine {
			delete(a.bad, b) // quarentena vencida: o IP pode voltar (ou nunca mais aparecer)
		}
	}
	a.at = time.Now()
	if err != nil {
		return // mantém a lista anterior; DNS instável não derruba sessão viva
	}
	backends := make([]string, 0, len(ips))
	alive := map[string]bool{}
	for _, ip := range ips {
		b := ip
		if _, _, err := net.SplitHostPort(ip); err != nil {
			b = net.JoinHostPort(ip, a.port)
		}
		if t, ok := a.bad[b]; ok && time.Since(t) < quarantine {
			continue // DNS ainda lista, mas acabou de recusar conexão
		}
		backends = append(backends, b)
		alive[b] = true
	}
	sort.Strings(backends)
	a.backends = backends
	for sid, b := range a.sessions {
		if !alive[b] {
			delete(a.sessions, sid) // pod sumiu: a sessão morreu com ele
		}
	}
}

// pick devolve o backend (ip:porta) para a requisição, ou "" para usar o
// upstream como está (afinidade desligada ou desnecessária).
func (a *affinity) pick(sessionID string) string {
	if a.disabled {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refresh()
	if len(a.backends) == 0 {
		return "" // DNS ainda sem resposta: usa o upstream como está
	}
	if sessionID != "" {
		if b, ok := a.sessions[sessionID]; ok {
			return b
		}
	}
	// sessão nova: pod aleatório, como o kube-proxy (round-robin aqui viciava:
	// o cliente faz um número par de requisições sem sessão por conexão)
	return a.backends[rand.IntN(len(a.backends))]
}

func (a *affinity) bind(sessionID, backend string) {
	if sessionID == "" || backend == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.sessions) >= maxSessions {
		a.sessions = map[string]string{} // proteção de memória; clientes reabrem sessão
	}
	a.sessions[sessionID] = backend
}

func (a *affinity) forget(sessionID string) {
	if sessionID == "" {
		return
	}
	a.mu.Lock()
	delete(a.sessions, sessionID)
	a.mu.Unlock()
}

// target devolve o upstream reescrito para o backend escolhido.
func (a *affinity) target(upstream, backend string) string {
	if backend == "" {
		return upstream
	}
	u, _ := url.Parse(upstream)
	u.Host = backend
	return u.String()
}

// wrap acrescenta à resposta o registro sessão → pod.
func (a *affinity) wrap(next func(*http.Response) error, method, sessionID, backend string) func(*http.Response) error {
	return func(resp *http.Response) error {
		switch {
		case method == http.MethodDelete || resp.StatusCode == http.StatusNotFound:
			a.forget(sessionID)
		default:
			if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
				served := backend
				if backend != "" && resp.Request != nil {
					served = resp.Request.URL.Host // o pod que de fato atendeu (pode ser outro após retentativa)
				}
				a.bind(sid, served)
			}
		}
		if next != nil {
			return next(resp)
		}
		return nil
	}
}

func (p *Proxy) affinityFor(server, upstream string) *affinity {
	p.affMu.Lock()
	defer p.affMu.Unlock()
	if p.aff == nil {
		p.aff = map[string]*affinity{}
	}
	a := p.aff[server]
	if a == nil {
		a = newAffinity(upstream)
		if p.cfg.lookup != nil {
			a.lookup = p.cfg.lookup
		}
		p.aff[server] = a
	}
	return a
}

// hostHeader é o Host a enviar quando o destino foi trocado pelo IP do pod.
func (a *affinity) hostHeader(backend string) string {
	if backend == "" {
		return ""
	}
	if a.port == "80" {
		return a.host
	}
	return net.JoinHostPort(a.host, a.port)
}

// failed tira na hora um pod que recusou conexão (morreu antes de o DNS
// headless refletir) e força nova consulta ao DNS na próxima requisição —
// fecha a janela em que sessões novas ainda iriam para o IP morto.
func (a *affinity) failed(backend string) {
	if backend == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	kept := a.backends[:0]
	for _, b := range a.backends {
		if b != backend {
			kept = append(kept, b)
		}
	}
	a.backends = kept
	a.bad[backend] = time.Now()
	for sid, b := range a.sessions {
		if b == backend {
			delete(a.sessions, sid)
		}
	}
	a.at = time.Time{}
}

// onErr encadeia a remoção do pod com o tratamento de erro original.
func (a *affinity) onErr(backend string, next func(error)) func(error) {
	return func(err error) {
		a.failed(backend)
		if next != nil {
			next(err)
		}
	}
}

// roundTripper: se o pod escolhido recusar a CONEXÃO (morreu e o DNS ainda
// não refletiu), a requisição nunca chegou ao MCP — então é seguro tentar
// outro pod. Só para requisições sem sessão (initialize): uma sessão presa
// ao pod morto morreu com ele, e reenviar para outro pod daria 404 de
// qualquer jeito.
func (a *affinity) roundTripper(base http.RoundTripper, sessionID string) http.RoundTripper {
	if a.disabled || sessionID != "" {
		return base
	}
	return rtFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(req)
		for tries := 0; err != nil && isDialErr(err) && tries < 2; tries++ {
			a.failed(req.URL.Host)
			next := a.pick("")
			if next == "" || next == req.URL.Host || req.GetBody == nil && req.Body != nil && req.Body != http.NoBody {
				break
			}
			r2 := req.Clone(req.Context())
			r2.URL.Host = next
			if req.GetBody != nil {
				if r2.Body, err = req.GetBody(); err != nil {
					break
				}
			}
			resp, err = base.RoundTrip(r2)
		}
		return resp, err
	})
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func isDialErr(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}
