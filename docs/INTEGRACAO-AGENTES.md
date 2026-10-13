# Integrar um agente ao mirante: HTTP direto (recomendado) ou SDK privado

O mirante enxerga um agente de três jeitos:

| Jeito | O que o mirante vê | Quando usar |
|---|---|---|
| **Proxy** (`/p/<agente>/llm/...` e `/p/<agente>/mcp/...`) | chamadas ao LLM e às tools, sem mudar código | LLM sem assinatura (Ollama, OpenAI-compatible) e MCP |
| **Eventos por HTTP direto** (`POST /v1/events`) | tudo que o agente quiser contar: pergunta, usuário, contexto, decisões, guardrails, resposta | **LLM assinado (Bedrock/SigV4)**, raciocínio interno, sinais próprios — e quando o mirante roda **no mesmo cluster** |
| **SDK Go** (`github.com/chwiee/mirante/pkg/mirante`) | o mesmo que o HTTP direto, com buffer e lote prontos | agentes Go que **conseguem** baixar o módulo privado |

Proxy e eventos combinam: o MCP continua passando pelo proxy e os eventos contam o que o proxy
não vê.

## 1. Recomendação: no cluster, chame o mirante direto por HTTP

Com o mirante no cluster, o agente fala com ele pelo Service interno:

```
http://mirante.observabilidade.svc.cluster.local:8080/v1/events
```

Por que **não** importar o SDK:

- **Repositório privado:** importar o SDK obriga todo build do agente (máquina do dev, CI, imagem) a ter
  credencial do GitHub para o repo do mirante. No GitHub da empresa (EMU) isso ainda passa por
  política de tokens e IP allow list (seção 3). O HTTP direto não precisa de nada disso.
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
  {"type":"run_start","run_id":"r-123","agent":"midts","time":"2026-10-13T12:00:00Z",
   "user":"joana","input":"Os publishers do Rabbit de pagamentos travaram..."},
  {"type":"decision","run_id":"r-123","span_id":"d1","agent":"midts","time":"2026-10-13T12:00:01Z",
   "model":"qwen3-next-80b","chosen":["rabbitmq_api"],"tokens_in":5120,"tokens_out":80,"duration_ms":1400},
  {"type":"tool_call","run_id":"r-123","span_id":"t1","parent_id":"d1","agent":"midts","time":"2026-10-13T12:00:01Z",
   "server":"mid-collector","tool":"rabbitmq_api","args":{"consulta":"alarmes"}},
  {"type":"tool_result","run_id":"r-123","span_id":"t1","agent":"midts","time":"2026-10-13T12:00:02Z",
   "result":{"status":200},"duration_ms":150},
  {"type":"run_end","run_id":"r-123","agent":"midts","time":"2026-10-13T12:00:05Z",
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

## 2. Se mesmo assim quiser o SDK (módulo Go privado)

O SDK mora em `github.com/chwiee/mirante/pkg/mirante` (repositório privado). Para o `go` baixar um
módulo privado:

```bash
# 1) Não usar o proxy/checksum público para este caminho
go env -w GOPRIVATE=github.com/chwiee/*

# 2) Credencial para o git/HTTPS (token com leitura do repo)
git config --global url."https://x-access-token:${GITHUB_TOKEN}@github.com/".insteadOf "https://github.com/"
#    ou ~/.netrc (o Go 1.24+ também usa o .netrc via GOAUTH=netrc):
#    machine github.com login x-access-token password <TOKEN>

# 3) Baixar
go get github.com/chwiee/mirante@latest
```

No build da imagem:

- **Imagem montada em Go (sem Docker, como no MidTS):** o `go build` roda na sua máquina/CI, então
  basta o passo acima no ambiente que compila.
- **Dockerfile:** use *secret* do BuildKit (o token não fica em camada nenhuma):
  ```dockerfile
  RUN --mount=type=secret,id=netrc,target=/root/.netrc \
      GOPRIVATE=github.com/chwiee/* go mod download
  ```
  `docker buildx build --secret id=netrc,src=$HOME/.netrc ...`
- **Sem acesso ao GitHub no build:** `go mod vendor` e versione a pasta `vendor/` no agente.
- **Proxy corporativo de Go** (Artifactory/Nexus/Athens): cadastre o repo privado como remoto
  autenticado e use `GOPROXY=https://<proxy>/go,direct` + `GONOSUMDB=github.com/chwiee/*`.

## 3. Repositório no GitHub da empresa com EMU (Enterprise Managed Users)

No EMU as contas são criadas e controladas pelo provedor de identidade da empresa. Isso muda o jogo:

1. **O repo precisa estar na organização da empresa.** Contas gerenciadas (`usuario_sufixo`) não
   enxergam repositórios privados de contas pessoais (ex.: `github.com/chwiee/...`). Transfira ou
   crie o mirante numa org da enterprise.
2. **O caminho do módulo muda junto.** O `module` do `go.mod` precisa bater com o caminho do repo:
   ```diff
   - module github.com/chwiee/mirante
   + module github.com/<org-da-empresa>/mirante
   ```
   e os imports internos também (`go mod edit -module ...` + trocar os imports). Sem isso, o `go get`
   falha com *"module declares its path as ... but was required as ..."*.
3. **Host com residência de dados (GHE.com):** se a enterprise estiver em `<empresa>.ghe.com`, o
   módulo vira `<empresa>.ghe.com/<org>/mirante` e o `GOPRIVATE` precisa cobrir esse host
   (`GOPRIVATE=<empresa>.ghe.com`). Coloque o token no `.netrc` para esse host: o `go` faz uma
   requisição HTTPS de descoberta (`?go-get=1`) antes do `git clone`, e ela também precisa de credencial.
4. **Tokens:** as políticas da enterprise/org podem bloquear PAT clássico, exigir aprovação de
   *fine-grained PAT* ou limitar a validade. Para CI, prefira um **GitHub App** da org com permissão
   `contents: read` no repo do mirante e gere o token na hora
   (ex.: `actions/create-github-app-token`). O `GITHUB_TOKEN` padrão do Actions só lê o próprio repo.
5. **IP allow list:** se a enterprise restringir IPs, os runners de CI e as máquinas que compilam
   precisam estar na lista (runners self-hosted dentro da rede da empresa resolvem).
6. **Proxy de Go corporativo:** em muitas empresas o `GOPROXY` aponta para o Artifactory/Nexus. Peça
   ao time de plataforma para cadastrar o repo privado lá; assim nenhum build precisa de token próprio.

> Resumo: com o mirante no cluster, **use o HTTP direto** e nada disso é necessário para os agentes.
> As seções 2 e 3 só importam para quem quiser o SDK — ou para o build do **próprio** mirante, que
> precisa do repo de qualquer forma.
