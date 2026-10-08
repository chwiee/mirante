// Comando mirante: painel em tempo real dos agentes (mapa mental + traces +
// alucinação), pensado pra rodar no telão.
//
// Dois jeitos de um agente aparecer no painel:
//   - SDK/HTTP: o agente manda eventos para POST /v1/events (pkg/mirante).
//   - Proxy (zero código): o agente aponta a URL do LLM e/ou dos MCP servers
//     para /p/{agente}/llm/{nome} e /p/{agente}/mcp/{server}.
//
// Com --events-url o binário vira sidecar: só faz proxy e manda os eventos
// para o mirante central (sem UI local).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/chwiee/mirante/internal/demo"
	"github.com/chwiee/mirante/internal/detect"
	"github.com/chwiee/mirante/internal/event"
	"github.com/chwiee/mirante/internal/hub"
	"github.com/chwiee/mirante/internal/proxy"
	"github.com/chwiee/mirante/internal/server"
	"github.com/chwiee/mirante/internal/store"
	"github.com/chwiee/mirante/pkg/mirante"
	"github.com/chwiee/mirante/web"
)

// upstreams é um flag repetível "nome=url" (também aceita lista separada por vírgula).
type upstreams map[string]string

func (u upstreams) String() string {
	keys := make([]string, 0, len(u))
	for k, v := range u {
		keys = append(keys, k+"="+v)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func (u upstreams) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, url, ok := strings.Cut(part, "=")
		if !ok || name == "" || url == "" {
			return fmt.Errorf("esperado nome=url, veio %q", part)
		}
		u[strings.TrimSpace(name)] = strings.TrimSpace(url)
	}
	return nil
}

func main() {
	llm, mcp := upstreams{}, upstreams{}
	addr := flag.String("addr", envOr("MIRANTE_ADDR", ":8080"), "endereço HTTP")
	maxRuns := flag.Int("max-runs", 500, "quantos runs manter em memória")
	minConf := flag.Float64("min-confidence", detect.Default.MinConfidence, "confiança abaixo disso vira incerteza (0 desliga)")
	margin := flag.Float64("ambiguity-margin", detect.Default.AmbiguityMargin, "diferença mínima de score entre escolhida e alternativa")
	actionTools := flag.String("action-tools", os.Getenv("MIRANTE_ACTION_TOOLS"), "tools que executam ações mesmo sem o nome dizer, separadas por vírgula (ex: troubleshoot,approve_action)")
	token := flag.String("ingest-token", os.Getenv("MIRANTE_INGEST_TOKEN"), "bearer token exigido em /v1/events (vazio = aberto)")
	demoMode := flag.Bool("demo", false, "gera tráfego sintético com as tools reais do ecossistema")
	flag.Var(llm, "llm", "upstream de LLM para o proxy, nome=url (repetível; env MIRANTE_LLM)")
	flag.Var(mcp, "mcp", "upstream MCP para o proxy, server=url (repetível; env MIRANTE_MCP)")
	inject := flag.Bool("inject-reasoning", os.Getenv("MIRANTE_INJECT_REASONING") == "true", "proxy injeta reason/confidence no schema das tools e remove na resposta")
	redact := flag.String("redact-keys", envOr("MIRANTE_REDACT_KEYS", proxy.DefaultRedactKeys.String()), "regex de chaves JSON redigidas no proxy (vazio desliga)")
	eventsURL := flag.String("events-url", os.Getenv("MIRANTE_EVENTS_URL"), "modo sidecar: manda eventos do proxy para este mirante central")
	publicURL := flag.String("public-url", os.Getenv("MIRANTE_PUBLIC_URL"), "URL pela qual os agentes alcançam este mirante (vai nas instruções para IA em /ia; vazio = deduz da requisição)")
	front := flag.String("front", os.Getenv("MIRANTE_FRONT"), "modo front: o mirante se passa pelo MCP server, na mesma URL que os clientes já usam (server=URL; ex: k8s-ts-mcp=http://hub-server-mcp-headless.k8s-ts-mcp.svc:8443). Clientes não mudam nada; o nome do cliente vem do initialize. O caminho na URL (ex: /mcp) é usado só para o mirante descobrir as tools ao subir; o caminho do cliente é sempre preservado")
	frontAddr := flag.String("front-addr", envOr("MIRANTE_FRONT_ADDR", ":8081"), "porta do modo front (o Service/Ingress do MCP aponta para ela)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	for env, dst := range map[string]upstreams{"MIRANTE_LLM": llm, "MIRANTE_MCP": mcp} {
		if v := os.Getenv(env); v != "" {
			if err := dst.Set(v); err != nil {
				log.Error("config", "env", env, "err", err)
				os.Exit(2)
			}
		}
	}
	var redactRe *regexp.Regexp
	if *redact != "" {
		redactRe = regexp.MustCompile(*redact)
	}

	var (
		handler http.Handler
		emit    func(*event.Event)
		closeFn = func() {}
		srv     *server.Server
	)
	if *eventsURL != "" {
		// sidecar: eventos vão em lote para o central; agente nunca espera o painel
		mc := mirante.New(mirante.Config{URL: strings.TrimRight(*eventsURL, "/"), Token: *token})
		// o evento vai como está: o SDK serializa uma vez, em lote, fora da
		// requisição (antes: json.Marshal aqui, no caminho, e de novo no lote)
		emit = func(e *event.Event) { mc.Send(e) }
		closeFn = mc.Close
	} else {
		srv = &server.Server{
			Store:       store.New(*maxRuns, detect.Config{MinConfidence: *minConf, AmbiguityMargin: *margin, ActionTools: splitList(*actionTools)}),
			Hub:         hub.New(),
			IngestToken: *token,
			PublicURL:   *publicURL,
			UI:          web.FS,
			Log:         log,
		}
		emit, _ = srv.AsyncIngest(16384) // fora do caminho da requisição
	}

	px := proxy.New(proxy.Config{LLM: llm, MCP: mcp, InjectReasoning: *inject, RedactKeys: redactRe, Emit: emit, Log: log})
	defer px.Close()

	if srv != nil {
		srv.Mount = px.Register
		srv.Upstreams = px.Upstreams
		handler = srv.Handler()
	} else {
		mux := http.NewServeMux()
		px.Register(mux)
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
		handler = mux
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	hs := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := hs.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Error("serve", "err", err)
			stop()
		}
	}()
	base := "http://" + displayAddr(ln.Addr().String())
	mode := "central"
	if *eventsURL != "" {
		mode = "sidecar → " + *eventsURL
	}
	log.Info("mirante no ar", "url", base, "modo", mode, "llm", llm.String(), "mcp", mcp.String(), "inject_reasoning", *inject, "demo", *demoMode)

	// descoberta: o mirante se conecta a cada MCP configurado e mostra server +
	// tools no mapa ao subir, antes de qualquer cliente passar. Tokens só para
	// isso, por env (não por flag, para não aparecerem na lista de processos):
	// MIRANTE_MCP_TOKEN="server=token,outro=token".
	tokens := upstreams{}
	if v := os.Getenv("MIRANTE_MCP_TOKEN"); v != "" {
		if err := tokens.Set(v); err != nil {
			log.Error("config", "env", "MIRANTE_MCP_TOKEN", "err", err)
		}
	}
	for name, u := range mcp {
		go px.Discover(ctx, name, u, tokens[name])
	}
	if name, u, ok := strings.Cut(*front, "="); ok && *front != "" {
		go px.Discover(ctx, name, u, tokens[name])
	}

	if *demoMode && srv != nil {
		go demo.Run(ctx, base)
	}

	// modo front: listener próprio, no lugar do MCP (a raiz "/" aqui é do MCP,
	// não do painel). Saúde do próprio mirante em /_mirante/healthz.
	var fs *http.Server
	if *front != "" {
		name, up, ok := strings.Cut(*front, "=")
		if !ok || name == "" || up == "" {
			log.Error("config", "err", "--front espera server=URL", "valor", *front)
			os.Exit(2)
		}
		fmux := http.NewServeMux()
		fmux.HandleFunc("GET /_mirante/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
		fmux.Handle("/", px.Front(name, up))
		fln, err := net.Listen("tcp", *frontAddr)
		if err != nil {
			log.Error("listen front", "err", err)
			os.Exit(1)
		}
		fs = &http.Server{Handler: fmux, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := fs.Serve(fln); err != nil && err != http.ErrServerClosed {
				log.Error("serve front", "err", err)
				stop()
			}
		}()
		log.Info("modo front no ar: clientes do MCP entram por aqui", "server", name, "upstream", up, "addr", fln.Addr().String())
	}

	<-ctx.Done()
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hs.Shutdown(shut)
	if fs != nil {
		fs.Shutdown(shut)
	}
	closeFn()
}

func displayAddr(a string) string {
	if strings.HasPrefix(a, "[::]") || strings.HasPrefix(a, "0.0.0.0") {
		_, port, _ := net.SplitHostPort(a)
		return "localhost:" + port
	}
	return a
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
