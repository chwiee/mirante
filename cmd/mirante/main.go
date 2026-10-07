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
	"encoding/json"
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
		emit = func(e *event.Event) {
			b, _ := json.Marshal(e)
			mc.Send(json.RawMessage(b))
		}
		closeFn = mc.Close
	} else {
		srv = &server.Server{
			Store:       store.New(*maxRuns, detect.Config{MinConfidence: *minConf, AmbiguityMargin: *margin, ActionTools: splitList(*actionTools)}),
			Hub:         hub.New(),
			IngestToken: *token,
			UI:          web.FS,
			Log:         log,
		}
		emit = func(e *event.Event) { srv.Ingest([]*event.Event{e}) }
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

	if *demoMode && srv != nil {
		go demo.Run(ctx, base)
	}

	<-ctx.Done()
	shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	hs.Shutdown(shut)
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
