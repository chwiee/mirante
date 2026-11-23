# Integrar um agente ao mirante: proxy, HTTP direto ou SDK Go

O mirante enxerga um agente de três jeitos:

| Jeito | O que o mirante vê | Quando usar |
|---|---|---|
| **Proxy** (`/p/<agente>/llm/...` e `/p/<agente>/mcp/...`) | chamadas ao LLM e às tools, sem mudar código | LLM sem assinatura (Ollama, OpenAI-compatible) e MCP |
| **Eventos por HTTP direto** (`POST /v1/events`) | tudo que o agente quiser contar: pergunta, usuário, contexto, decisões, guardrails, resposta | **LLM assinado (Bedrock/SigV4)**, raciocínio interno, sinais próprios — e quando o mirante roda **no mesmo cluster** |
| **SDK Go** (`github.com/chwiee/mirante/pkg/mirante`) | o mesmo que o HTTP direto, com buffer e lote prontos | agentes Go que preferem um cliente pronto |

Proxy e eventos combinam: o MCP continua passando pelo proxy e os eventos contam o que o proxy
não vê.

## 1. Recomendação: no cluster, chame o mirante direto por HTTP

Com o mirante no cluster, o agente fala com ele pelo Service interno:

```
http://mirante.observabilidade.svc.cluster.local:8080/v1/events
```

Por que HTTP direto é o caminho recomendado:

- **Desacoplamento:** o agente não fica preso à versão do SDK; o contrato JSON é estável e pequeno.
- **Qualquer linguagem:** o mesmo contrato serve para Go, Python, Node.
- **Custo:** ~60 linhas de cliente dentro do próprio agente (exemplo na seção 1.3).

### 1.1 Contrato

`POST /v1/events` com **um evento** (objeto JSON) ou **um lote** (array). Resposta `202 Accepted`.
Limite de 8 MB por requisição. Se o mirante subir com `--ingest-token` (`MIRANTE_INGEST_TOKEN`),
mande `Authorization: Bearer <token>`.

Campos comuns a todos os eventos: `type`, `run_id`, `agent`, `time` (RFC 3339). Um **run** é uma
requisição atendida pelo agente; `run_id` é gerado pelo agente (qualquer string única).

| `type` | Quando | Campos próprios |
|---|---|---|
| `run_start` | a pergunta chegou | `input`, `user`, `tools` (lista de `{server,name,description,schema,read_only}`) |
| `decision` | o LLM decidiu | `span_id`, `model`, `reasoning`, `chosen` (tools escolhidas), `alternatives`, `confidence`, `tokens_in`, `tokens_out`, `duration_ms` |
| `tool_call` | a tool começou | `span_id`, `parent_id` (a decisão), `server`, `tool`, `args`, `rationale`, `confidence` |
| `tool_result` | a tool terminou | `span_id`, `result`, `error`, `duration_ms` |
| `flag` | sinal do próprio agente (ex.: LLM-judge, guardrail) | `span_id` (opcional), `flag: {level: hallucination\|uncertain, code, reason}` |
| `run_end` | o agente respondeu | `output`, `error` |
| `tools` | o agente conectou num MCP e listou as tools (sem run) | `tools` |

Exemplo de um run completo (um lote):

```json
[
  {"type":"run_start","run_id":"r-123","agent":"meu-agente","time":"2026-10-13T12:00:00Z",
   "user":"maria","input":"Os publishers do Rabbit de pagamentos travaram..."},
  {"type":"decision","run_id":"r-123","span_id":"d1","agent":"meu-agente","time":"2026-10-13T12:00:01Z",
   "model":"qwen3-next-80b","chosen":["rabbitmq_api"],"tokens_in":5120,"tokens_out":80,"duration_ms":1400},
  {"type":"tool_call","run_id":"r-123","span_id":"t1","parent_id":"d1","agent":"meu-agente","time":"2026-10-13T12:00:01Z",
   "server":"coletor-mcp","tool":"rabbitmq_api","args":{"consulta":"alarmes"}},
  {"type":"tool_result","run_id":"r-123","span_id":"t1","agent":"meu-agente","time":"2026-10-13T12:00:02Z",
   "result":{"status":200},"duration_ms":150},
  {"type":"run_end","run_id":"r-123","agent":"meu-agente","time":"2026-10-13T12:00:05Z",
   "output":"**Causa provável** ..."}
]
```

Com `output` preenchido, a detecção de alucinação da resposta final (`resposta_sem_base`,
`acao_nao_executada`) passa a valer para o agente — o que o proxy não consegue quando o LLM é assinado.

### 1.2 Regras para o cliente
- **Nunca bloquear nem derrubar o agente:** envie em background, com buffer; buffer cheio ou mirante
  fora do ar = descarta o evento.
- **Timeout curto** (2–3 s) e **lotes** (a cada ~200 ms ou N eventos).
- **URL vazia = desligado** (no-op), para a instrumentação ficar no código sem `if` em volta.
- **Não mande segredo:** o mirante mascara chaves que casam com `--redact-keys`, mas o ideal é o agente
  já não enviar senha/token.

### 1.3 Cliente mínimo em Go (copie para o agente)

```go
package painel

import (
	"bytes"
	"encoding/json"
	"net/http"
	"time"
)

// Cliente manda eventos ao mirante sem nunca travar o agente. nil = desligado.
type Cliente struct {
	url, token, agente string
	fila               chan map[string]any
	http               *http.Client
}

func Novo(url, token, agente string) *Cliente {
	if url == "" {
		return nil
	}
	c := &Cliente{url: url + "/v1/events", token: token, agente: agente,
		fila: make(chan map[string]any, 4096), http: &http.Client{Timeout: 3 * time.Second}}
	go c.enviar()
	return c
}

// Evento enfileira; fila cheia descarta (o painel é secundário ao agente).
func (c *Cliente) Evento(tipo, runID string, campos map[string]any) {
	if c == nil {
		return
	}
	campos["type"], campos["run_id"], campos["agent"], campos["time"] = tipo, runID, c.agente, time.Now().UTC()
	select {
	case c.fila <- campos:
	default:
	}
}

func (c *Cliente) enviar() {
	t := time.NewTicker(200 * time.Millisecond)
	var lote []map[string]any
	for {
		select {
		case ev := <-c.fila:
			if lote = append(lote, ev); len(lote) < 100 {
				continue
			}
		case <-t.C:
			if len(lote) == 0 {
				continue
			}
		}
		corpo, _ := json.Marshal(lote)
		req, _ := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(corpo))
		req.Header.Set("Content-Type", "application/json")
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		if res, err := c.http.Do(req); err == nil {
			res.Body.Close()
		}
		lote = lote[:0]
	}
}
```

### 1.4 Kubernetes
- Variáveis no agente: `MIRANTE_URL=http://mirante.observabilidade.svc.cluster.local:8080`,
  `MIRANTE_TOKEN` (do Secret, se usar `--ingest-token`).
- **NetworkPolicy:** se o namespace do mirante restringir entrada, libere a porta 8080 a partir do
  namespace do agente; se o do agente restringir saída, libere o destino `observabilidade:8080`.
- Mantenha o painel e o `/v1/events` **só na rede interna** (sem Ingress público). Se precisar expor,
  use `--ingest-token`.

## 2. SDK Go

Para agentes em Go, o SDK traz o mesmo cliente pronto (buffer, lote, no-op com URL vazia):

```bash
go get github.com/chwiee/mirante@latest
```

```go
import "github.com/chwiee/mirante/pkg/mirante"

mc := mirante.New(mirante.Config{URL: os.Getenv("MIRANTE_URL"), Agent: "meu-agente"}) // URL vazia = no-op
defer mc.Close()
```

O SDK só depende da biblioteca padrão do Go. Ainda assim, se preferir não adicionar dependência ao
agente, o cliente da seção 1.3 faz o mesmo em ~60 linhas.
