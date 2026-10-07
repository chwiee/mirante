// Package proxy é o modo plug-and-play do mirante: o agente não importa SDK
// nenhum — só troca a URL do LLM e/ou dos MCP servers para passar pelo
// mirante, que encaminha tudo de forma transparente e observa no caminho.
//
//	/p/{agente}/llm/{nome}/...   → upstream de LLM (Ollama /api/chat ou OpenAI-compatible /chat/completions)
//	/p/{agente}/mcp/{server}/... → upstream MCP (Streamable HTTP)
//
// O nome do agente vai na URL, então um mirante serve vários agentes.
package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chwiee/mirante/internal/event"
	"github.com/chwiee/mirante/pkg/mirante"
)

// DefaultRedactKeys casa chaves JSON cujo valor nunca deve aparecer no painel.
var DefaultRedactKeys = regexp.MustCompile(`(?i)(pass(word|wd)?|secret|token|api[_-]?key|authorization|credential|private[_-]?key)`)

type Config struct {
	LLM             map[string]string // nome → URL base (ex: ollama → http://ollama:11434)
	MCP             map[string]string // server → endpoint (ex: k8s-ts-mcp → http://hub:8443/mcp)
	InjectReasoning bool              // injeta reason/confidence nas tools (só requisições sem stream)
	RedactKeys      *regexp.Regexp    // nil = não redige
	Emit            func(*event.Event)
	Log             *slog.Logger
}

type Proxy struct {
	cfg  Config
	corr *correlator
	stop chan struct{}
}

func New(cfg Config) *Proxy {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	p := &Proxy{cfg: cfg, corr: newCorrelator(cfg.Emit, redactor(cfg.RedactKeys)), stop: make(chan struct{})}
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				p.corr.sweep()
			case <-p.stop:
				return
			}
		}
	}()
	return p
}

func (p *Proxy) Close() { close(p.stop) }

// Register monta as rotas do proxy no mux.
func (p *Proxy) Register(mux *http.ServeMux) {
	for _, m := range []string{"GET", "POST", "DELETE"} {
		mux.HandleFunc(m+" /p/{agent}/llm/{name}/{rest...}", p.handleLLM)
		mux.HandleFunc(m+" /p/{agent}/mcp/{server}", p.handleMCP)
		mux.HandleFunc(m+" /p/{agent}/mcp/{server}/{rest...}", p.handleMCP)
	}
}

// Upstreams descreve o que está configurado (usado pela tela "conectar agente").
func (p *Proxy) Upstreams() map[string]any {
	keys := func(m map[string]string) []string {
		out := make([]string, 0, len(m))
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	return map[string]any{"llm": keys(p.cfg.LLM), "mcp": keys(p.cfg.MCP), "inject_reasoning": p.cfg.InjectReasoning}
}

// ---------- LLM ----------

func (p *Proxy) handleLLM(w http.ResponseWriter, r *http.Request) {
	agent, name, rest := r.PathValue("agent"), r.PathValue("name"), r.PathValue("rest")
	up, ok := p.cfg.LLM[name]
	if !ok {
		http.Error(w, fmt.Sprintf("mirante: upstream de LLM %q não configurado (use --llm %s=URL)", name, name), http.StatusNotFound)
		return
	}
	dialect := detectDialect(rest)
	if r.Method != http.MethodPost || dialect == "" {
		p.forward(w, r, up, rest, nil, nil, nil) // /api/tags, /v1/models… passam direto
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	req, raw, ok := parseChatReq(dialect, body)
	if !ok {
		p.forward(w, r, up, rest, body, nil, nil)
		return
	}

	t := p.corr.llmRequest(agent, req)
	inject := p.cfg.InjectReasoning && !req.Stream && len(req.Tools) > 0
	if inject {
		body = injectReasoning(raw)
	}

	modify := func(resp *http.Response) error {
		if resp.StatusCode >= 400 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			p.corr.llmError(t, fmt.Sprintf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b))))
			replaceBody(resp, b)
			return nil
		}
		if inject {
			b, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return err
			}
			if parsed, ok := parseChatResp(dialect, b, false); ok {
				p.corr.llmResponse(t, parsed)
				b = stripReasoning(dialect, b)
			}
			replaceBody(resp, b)
			return nil
		}
		resp.Body = observe(resp.Body, func(b []byte) {
			if parsed, ok := parseChatResp(dialect, b, req.Stream); ok {
				p.corr.llmResponse(t, parsed)
			} else {
				p.corr.llmError(t, "resposta do LLM ilegível ou interrompida")
			}
		})
		return nil
	}
	p.forward(w, r, up, rest, body, modify, func(err error) { p.corr.llmError(t, err.Error()) })
}

// ---------- MCP ----------

type rpcMsg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func (p *Proxy) handleMCP(w http.ResponseWriter, r *http.Request) {
	agent, server, rest := r.PathValue("agent"), r.PathValue("server"), r.PathValue("rest")
	up, ok := p.cfg.MCP[server]
	if !ok {
		http.Error(w, fmt.Sprintf("mirante: MCP server %q não configurado (use --mcp %s=URL)", server, server), http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		p.forward(w, r, up, rest, nil, nil, nil) // GET (stream do servidor) e DELETE (fim de sessão)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if p.cfg.InjectReasoning {
		body = p.sanitizeCalls(agent, body)
	}

	var mu sync.Mutex
	calls := map[string]*mcpCall{}
	lists := map[string]bool{}
	for _, m := range decodeRPC(body) {
		switch m.Method {
		case "tools/call":
			var prm struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			json.Unmarshal(m.Params, &prm)
			if len(prm.Arguments) == 0 {
				prm.Arguments = json.RawMessage(`{}`)
			}
			calls[string(m.ID)] = p.corr.mcpCallStart(agent, server, prm.Name, prm.Arguments)
		case "tools/list":
			lists[string(m.ID)] = true
		}
	}
	if len(calls) == 0 && len(lists) == 0 {
		p.forward(w, r, up, rest, body, nil, nil)
		return
	}

	failAll := func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		for id, c := range calls {
			p.corr.mcpCallDone(c, nil, msg)
			delete(calls, id)
		}
	}
	modify := func(resp *http.Response) error {
		if resp.StatusCode >= 400 {
			failAll(fmt.Sprintf("HTTP %d do MCP server", resp.StatusCode))
			return nil
		}
		ct := resp.Header.Get("Content-Type")
		resp.Body = observe(resp.Body, func(b []byte) {
			for _, m := range decodeRPCResponses(ct, b) {
				id := string(m.ID)
				mu.Lock()
				c := calls[id]
				delete(calls, id)
				mu.Unlock()
				switch {
				case c != nil:
					if m.Error != nil {
						p.corr.mcpCallDone(c, nil, fmt.Sprintf("JSON-RPC %d: %s", m.Error.Code, m.Error.Message))
					} else {
						res, errMsg := toolResult(m.Result)
						p.corr.mcpCallDone(c, res, errMsg)
					}
				case lists[id] && m.Result != nil:
					p.corr.mcpTools(agent, server, toolSpecs(m.Result))
				}
			}
			failAll("MCP server não devolveu resposta para a chamada")
		})
		return nil
	}
	p.forward(w, r, up, rest, body, modify, func(err error) { failAll(err.Error()) })
}

// sanitizeCalls é a rede de segurança da injeção de raciocínio: se um campo
// reason/confidence (ou variação, ex: "confiança") escapou da limpeza da
// resposta do LLM e chegou num tools/call, ele é removido aqui — a menos que
// a tool declare esse campo de verdade no schema. Sem isso, um MCP server com
// additionalProperties:false rejeita a chamada (visto ao vivo com qwen2.5:7b).
func (p *Proxy) sanitizeCalls(agent string, body []byte) []byte {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return body
	}
	changed := false
	fix := func(m any) {
		mm, _ := m.(map[string]any)
		if mm["method"] != "tools/call" {
			return
		}
		params, _ := mm["params"].(map[string]any)
		args, _ := params["arguments"].(map[string]any)
		tool, _ := params["name"].(string)
		for k := range args {
			if mirante.IsReasoningKey(k) && !p.corr.declares(agent, tool, k) {
				delete(args, k)
				changed = true
			}
		}
	}
	if arr, ok := v.([]any); ok {
		for _, m := range arr {
			fix(m)
		}
	} else {
		fix(v)
	}
	if !changed {
		return body
	}
	b, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return b
}

func decodeRPC(b []byte) []rpcMsg {
	b = bytes.TrimSpace(b)
	var many []rpcMsg
	if len(b) > 0 && b[0] == '[' {
		json.Unmarshal(b, &many)
		return many
	}
	var one rpcMsg
	if json.Unmarshal(b, &one) == nil {
		return []rpcMsg{one}
	}
	return nil
}

// decodeRPCResponses lê respostas JSON-RPC de um corpo JSON ou de um stream
// SSE (cada evento: linhas "data:" que juntas formam um JSON).
func decodeRPCResponses(contentType string, b []byte) []rpcMsg {
	if !strings.HasPrefix(contentType, "text/event-stream") {
		return decodeRPC(b)
	}
	var out []rpcMsg
	var data strings.Builder
	flush := func() {
		if data.Len() > 0 {
			out = append(out, decodeRPC([]byte(data.String()))...)
			data.Reset()
		}
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	flush()
	return out
}

func toolResult(raw json.RawMessage) (json.RawMessage, string) {
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
		IsError    bool            `json:"isError"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return raw, ""
	}
	var text strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	out := raw
	switch {
	case len(r.Structured) > 0 && string(r.Structured) != "null":
		out = r.Structured
	case len(r.Content) == 1 && r.Content[0].Type == "text":
		out = jsonString(r.Content[0].Text)
	}
	if r.IsError {
		msg := text.String()
		if msg == "" {
			msg = "a tool devolveu isError=true"
		}
		return out, msg
	}
	return out, ""
}

func toolSpecs(raw json.RawMessage) []event.ToolSpec {
	var r struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
			Annotations *struct {
				ReadOnlyHint    *bool `json:"readOnlyHint"`
				DestructiveHint *bool `json:"destructiveHint"`
			} `json:"annotations"`
		} `json:"tools"`
	}
	json.Unmarshal(raw, &r)
	out := make([]event.ToolSpec, 0, len(r.Tools))
	for _, t := range r.Tools {
		spec := event.ToolSpec{Name: t.Name, Description: t.Description, Schema: t.InputSchema}
		if a := t.Annotations; a != nil {
			switch {
			case a.DestructiveHint != nil && *a.DestructiveHint:
				ro := false
				spec.ReadOnly = &ro
			case a.ReadOnlyHint != nil:
				spec.ReadOnly = a.ReadOnlyHint
			}
		}
		out = append(out, spec)
	}
	return out
}

// ---------- encaminhamento ----------

func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, upstream, rest string, body []byte,
	modify func(*http.Response) error, onErr func(error)) {
	target, err := url.Parse(upstream)
	if err != nil {
		http.Error(w, "mirante: upstream inválido: "+err.Error(), http.StatusInternalServerError)
		return
	}
	observing := modify != nil
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = joinPath(target.Path, rest)
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = r.URL.RawQuery
			if observing {
				pr.Out.Header.Del("Accept-Encoding") // corpo sem gzip para conseguir ler
			}
		},
		FlushInterval:  -1, // streaming (SSE / NDJSON) passa sem buffer
		ModifyResponse: modify,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if onErr != nil {
				onErr(err)
			}
			p.cfg.Log.Warn("proxy: upstream falhou", "upstream", upstream, "err", err)
			http.Error(w, "mirante proxy: "+err.Error(), http.StatusBadGateway)
		},
	}
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	}
	rp.ServeHTTP(w, r)
}

func joinPath(base, rest string) string {
	if rest == "" {
		if base == "" {
			return "/"
		}
		return base
	}
	return strings.TrimRight(base, "/") + "/" + rest
}

func replaceBody(resp *http.Response, b []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(b))
	resp.ContentLength = int64(len(b))
	resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
	resp.Header.Del("Content-Encoding")
}

// observe copia o corpo enquanto ele passa para o cliente e chama fn uma vez
// no fim (EOF ou Close). Até 32MB; além disso para de copiar.
func observe(rc io.ReadCloser, fn func([]byte)) io.ReadCloser {
	return &observed{rc: rc, fn: fn}
}

type observed struct {
	rc   io.ReadCloser
	buf  bytes.Buffer
	once sync.Once
	fn   func([]byte)
}

func (o *observed) Read(b []byte) (int, error) {
	n, err := o.rc.Read(b)
	if n > 0 && o.buf.Len() < 32<<20 {
		o.buf.Write(b[:n])
	}
	if err == io.EOF {
		o.finish()
	}
	return n, err
}

func (o *observed) Close() error {
	err := o.rc.Close()
	o.finish()
	return err
}

func (o *observed) finish() { o.once.Do(func() { o.fn(o.buf.Bytes()) }) }

// ---------- redação ----------

func redactor(re *regexp.Regexp) func(json.RawMessage) json.RawMessage {
	return func(raw json.RawMessage) json.RawMessage {
		if re == nil || len(raw) == 0 {
			return raw
		}
		t := bytes.TrimSpace(raw)
		if len(t) == 0 || (t[0] != '{' && t[0] != '[') {
			return raw
		}
		var v any
		if json.Unmarshal(t, &v) != nil {
			return raw
		}
		changed := false
		var walk func(any) any
		walk = func(v any) any {
			switch x := v.(type) {
			case map[string]any:
				for k, val := range x {
					if re.MatchString(k) {
						if s, ok := val.(string); !ok || s != "" {
							x[k] = "***"
							changed = true
						}
						continue
					}
					x[k] = walk(val)
				}
			case []any:
				for i := range x {
					x[i] = walk(x[i])
				}
			}
			return v
		}
		v = walk(v)
		if !changed {
			return raw
		}
		b, _ := json.Marshal(v)
		return b
	}
}
