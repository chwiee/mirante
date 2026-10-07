# Integrar este agente ao mirante — instruções para o assistente de IA

> Documento escrito **para um assistente de IA** (Claude Code, Kiro, Cursor…) executar.
> Humano: peça ao assistente *"integre o mirante neste agente seguindo esta instrução"*.
> Versão estática. Prefira a versão viva, servida pelo próprio mirante em `<URL do mirante>/ia/integrar-mirante.md`, que já traz a URL e os upstreams reais.

## Objetivo

Fazer este agente aparecer no **mirante**, o painel em tempo real que mostra, para cada pergunta, a decisão do LLM, cada tool chamada (com argumentos, retorno e tempo) e alertas de alucinação.

**Regras de ouro, em ordem de prioridade:**

1. **Não quebre o agente.** O comportamento visto pelo usuário final tem que ficar idêntico.
2. **Prefira configuração a código.** No modo proxy, a integração é **só trocar URLs**. Não toque na lógica do agente.
3. **Mudança mínima e reversível.** O usuário precisa conseguir desfazer voltando as URLs.
4. **Não invente.** Se não conseguir descobrir algo (URL, nome de upstream, onde fica a config), pergunte.

## Dados deste mirante

- URL base: `http://mirante.observabilidade:8080`, que é o **padrão do manifest**. Confirme com o usuário a URL real do mirante antes de usar e troque em todos os comandos abaixo.
- Upstreams de **LLM**: consulte `GET <mirante>/api/upstreams`
- Upstreams **MCP**: consulte `GET <mirante>/api/upstreams`
- Injeção de raciocínio (`--inject-reasoning`): ver `inject_reasoning` em `/api/upstreams`

Para confirmar os dados ao vivo, use `curl -s http://mirante.observabilidade:8080/api/upstreams`. A resposta traz `llm`, `mcp`, `llm_urls`, `mcp_urls` e `inject_reasoning`.

## Passo 1 — Descobrir como o agente fala com o mundo (faça, não pergunte)

Procure no repositório:

| o quê | onde/como procurar |
|---|---|
| **Cliente de LLM** | `/api/chat`, `ollama`, `chat/completions`, `base_url`, `baseURL`, `OPENAI_BASE_URL`, `api_base`, `ChatOllama`, `ChatOpenAI`, `new OpenAI(`, `OpenAI(`, `createOpenAI`, `litellm`, `bedrock`, `BedrockRuntime`, `anthropic`, `Anthropic(` |
| **Cliente MCP** | `StreamableHTTP`, `streamablehttp_client`, `StreamableClientTransport`, `StreamableHTTPClientTransport`, `SSEClientTransport`, `sse_client`, `stdio_client`, `StdioClientTransport`, arquivos `mcp.json` / `mcpServers` |
| **Onde a URL é configurada** | `.env.example`, `.env.*`, `config.*`, `settings.*`, `values*.yaml` (Helm), manifests Kubernetes (`env:`), `docker-compose*.yml`, `Dockerfile` (`ENV`), README |

Anote para cada destino: **o cliente usado**, **a variável/campo de config** e **o valor atual**.

## Passo 2 — Escolher o modo

| situação encontrada | modo | mudança |
|---|---|---|
| LLM via **Ollama** (`/api/chat`) ou **OpenAI-compatible** (`/chat/completions`: OpenAI, Azure OpenAI, vLLM, LiteLLM, Ollama `/v1`, Gemini OpenAI-compat…) | **A — proxy LLM** | só config |
| MCP via **Streamable HTTP** | **A — proxy MCP** | só config |
| LLM via **Bedrock**, **API da Anthropic** (`/v1/messages`) ou **Vertex** | proxy de LLM **não suportado** (requisição assinada ou protocolo diferente). Use o proxy MCP, se houver MCP HTTP, **e** opcionalmente o modo B/C para ver as decisões | config + (opcional) código |
| MCP via **stdio** ou via transporte **SSE legado** (`/sse` + `/messages`) | não dá para usar proxy | modo B/C, se o usuário quiser ver essas tools |
| sem MCP e sem LLM HTTP (tools locais, framework próprio) | **B (Go)** ou **C (HTTP)** | código mínimo |

Faça o modo A sempre que possível. Os modos B/C só valem se o usuário quiser o que o proxy não vê, ou se o proxy não for possível. Diga isso ao usuário.

**Se o destino do agente não estiver entre os upstreams configurados no mirante**, não invente um nome. Pare e diga ao usuário exatamente o que o operador do mirante precisa adicionar, por exemplo `--llm ollama=http://ollama.ce.svc:11434` ou `--mcp k8s-ts-mcp=http://k8s-ts-mcp:8443` (ou, no Kubernetes, as variáveis `MIRANTE_LLM`/`MIRANTE_MCP` do ConfigMap). Depois continue o resto.

## Passo 3A — Modo proxy (só configuração)

### Formato da URL nova

```
LLM:  http://mirante.observabilidade:8080/p/<agente>/llm/<upstream><resto>
MCP:  http://mirante.observabilidade:8080/p/<agente>/mcp/<upstream><resto>
```

- `<agente>`: nome do agente no painel, em kebab-case (sugira o nome do repositório e confirme com o usuário).
- `<upstream>`: o nome configurado no mirante cujo destino corresponde ao destino atual do agente. O mesmo serviço pode aparecer com hosts diferentes (`ollama`, `ollama.ce.svc`, `ollama.ce.svc.cluster.local`). Se não tiver certeza de que é o mesmo destino (mesmo host, porta e ambiente), pergunte ao usuário, porque apontar para o upstream errado faz o agente falar com outro serviço.
- `<resto>`: **o que sobra da URL atual depois de tirar o destino do upstream.** O mirante concatena `destino do upstream + resto`.

Exemplos, supondo o upstream `ollama → http://ollama:11434`:

| URL atual no agente | URL nova |
|---|---|
| `http://ollama:11434` | `http://mirante.observabilidade:8080/p/<agente>/llm/ollama` |
| `http://ollama:11434/v1` (cliente OpenAI) | `http://mirante.observabilidade:8080/p/<agente>/llm/ollama/v1` |
| `http://ollama:11434/api` (provider que espera `/api`) | `http://mirante.observabilidade:8080/p/<agente>/llm/ollama/api` |

Se o upstream fosse `openai → https://api.openai.com/v1` (com `/v1` já incluído), o cliente OpenAI usaria `http://mirante.observabilidade:8080/p/<agente>/llm/openai`, sem repetir o `/v1`.

Para MCP, com o upstream `k8s-ts-mcp → http://hub:8443/mcp`: a URL atual `http://hub:8443/mcp` vira `http://mirante.observabilidade:8080/p/<agente>/mcp/k8s-ts-mcp`.

### Onde mudar

- Mude o **valor padrão/exemplo** onde a config é declarada: `.env.example`, `values.yaml`, manifests, `docker-compose`.
- **Não** commite arquivos `.env` reais nem segredos. Se a URL só existe num `.env` local, mostre a mudança ao usuário em vez de editar.
- Se a URL estiver **fixa no código**, transforme-a em variável de ambiente com o valor antigo como padrão (`os.getenv("LLM_URL", "http://ollama:11434")`) e só então mude a config. É a única mudança de código aceitável no modo A.
- Mantenha o valor antigo visível num comentário ou na sua resposta, para o rollback.

### Armadilhas conhecidas

- **Headers e credenciais** (`Authorization`, `api-key`, `Mcp-Session-Id`) passam intactos pelo mirante. Não mude a autenticação do agente.
- **Streaming** funciona; o mirante só observa.
- **Proxy só de MCP**, sem o do LLM: o painel mostra as tools, mas cada rajada de chamadas vira um run sem pergunta e sem decisão. É o esperado.
- **Proxy só de LLM**, sem o do MCP: as tools aparecem sem o MCP server ao lado no mapa. Aponte os dois quando puder.
- O agente **não precisa de nenhuma dependência nova** no modo A.

## Passo 3B — Modo SDK (só Go)

```go
import "github.com/chwiee/mirante/pkg/mirante"

// no startup, UMA vez (URL vazia = no-op):
mc := mirante.New(mirante.Config{URL: os.Getenv("MIRANTE_URL"), Agent: "<agente>"})
defer mc.Close() // no shutdown

// por pergunta:
run := mc.StartRun(pergunta, specs) // []mirante.ToolSpec{Server, Name, Schema, ReadOnly}
run.Decision(mirante.Decision{Model: m, Reasoning: texto, Chosen: nomes, Duration: d})
span := run.ToolCall(mirante.Call{Server: s, Tool: t, Args: args, Rationale: porque})
span.End(resultado, err)
run.End(resposta, errFinal)
```

Exige acesso ao módulo privado `github.com/chwiee/mirante` (`GOPRIVATE=github.com/chwiee/*`). Se não houver, use o modo C com o contrato abaixo.

## Passo 3C — Modo HTTP (Python, TypeScript ou qualquer linguagem)

Copie o cliente de referência da linguagem do agente **para dentro do repositório** (ex: `observability/mirante_client.py`) e instrumente os pontos abaixo. Os dois clientes foram testados contra o mirante real, inclusive com o painel fora do ar.

**Onde instrumentar** (procure o loop de tool-calling):

1. ao receber a pergunta → `start_run(pergunta, tools)`, onde `tools` é a lista oferecida ao modelo, com `server`, `name` e `schema`;
2. depois de cada resposta do LLM → `decision(...)`, com as tools escolhidas (lista vazia = respondeu direto);
3. antes de executar cada tool → `tool_call(...)`; depois → `end(result=...)` ou `end(error=...)`;
4. ao responder ao usuário → `end(resposta)`; em exceção → `end(error=exc)`.

**Regras obrigatórias:**

- Crie o cliente **uma vez no startup** e chame `close()` **só no shutdown**. Nunca crie nem feche por requisição: com o painel inacessível, `close()` pode esperar alguns segundos.
- URL vinda de variável de ambiente (`MIRANTE_URL`). Vazia = no-op. Nunca deixe a URL fixa no código.
- Instrumentação **nunca** pode lançar exceção para o fluxo do agente. Os clientes já garantem isso; não adicione `raise`.
- Não mande segredos em `args`/`result`. Se a tool recebe credenciais, remova-as antes de passar ao `tool_call`.

<details><summary><b>Cliente Python</b> (<code>mirante_client.py</code>, só stdlib)</summary>

```python
"""Cliente mínimo do mirante para Python (só biblioteca padrão, Python 3.8+).

Manda eventos para POST /v1/events. Regras:
  - nunca bloqueia nem derruba o agente: fila em memória, envio em lote numa
    thread daemon; fila cheia ou painel fora do ar = evento descartado;
  - URL vazia = no-op (a instrumentação pode ficar no código sempre).

Uso:
    m = Mirante(os.getenv("MIRANTE_URL"), agent="meu-agente")
    run = m.start_run(pergunta, tools=[{"server": "k8s-ts-mcp", "name": "scan_cluster", "schema": {...}}])
    run.decision(model="gpt-4o", reasoning=texto, chosen=["scan_cluster"], duration_ms=812)
    span = run.tool_call("k8s-ts-mcp", "scan_cluster", {"cluster_id": "x"}, rationale="...")
    span.end(result=resultado)            # ou span.end(error=exc)
    run.end(resposta)                     # ou run.end(error=exc)
    m.close()                             # no shutdown: envia o que falta
"""
from __future__ import annotations

import json
import queue
import threading
import time
import urllib.request
import uuid
from datetime import datetime, timezone
from typing import Any, Dict, Iterable, List, Optional


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _new_id() -> str:
    return uuid.uuid4().hex[:16]


class Mirante:
    def __init__(self, url: Optional[str], agent: str, token: Optional[str] = None,
                 flush_interval: float = 0.2, max_queue: int = 4096) -> None:
        self.url = (url or "").rstrip("/")
        self.agent = agent
        self.token = token
        self.flush_interval = flush_interval
        self._q: "queue.Queue[Dict[str, Any]]" = queue.Queue(max_queue)
        self._closed = threading.Event()
        self._thread: Optional[threading.Thread] = None
        if self.url:
            self._thread = threading.Thread(target=self._loop, name="mirante", daemon=True)
            self._thread.start()

    def emit(self, ev: Dict[str, Any]) -> None:
        if not self.url:
            return
        ev.setdefault("agent", self.agent)
        ev.setdefault("time", _now())
        try:
            self._q.put_nowait(ev)
        except queue.Full:
            pass  # o agente é mais importante que o painel

    def start_run(self, input: str, tools: Optional[List[Dict[str, Any]]] = None, user: str = "") -> "Run":
        run = Run(self, _new_id())
        self.emit({"type": "run_start", "run_id": run.id, "input": input, "user": user, "tools": tools or []})
        return run

    def close(self, timeout: float = 5.0) -> None:
        if self._thread is None:
            return
        self._closed.set()
        self._thread.join(timeout)

    def _loop(self) -> None:
        while True:
            batch: List[Dict[str, Any]] = []
            try:
                batch.append(self._q.get(timeout=self.flush_interval))
            except queue.Empty:
                if self._closed.is_set():
                    return
                continue
            while len(batch) < 200:
                try:
                    batch.append(self._q.get_nowait())
                except queue.Empty:
                    break
            self._post(batch)

    def _post(self, batch: List[Dict[str, Any]]) -> None:
        headers = {"Content-Type": "application/json"}
        if self.token:
            headers["Authorization"] = "Bearer " + self.token
        req = urllib.request.Request(self.url + "/v1/events", data=json.dumps(batch, default=str).encode(),
                                     headers=headers, method="POST")
        try:
            urllib.request.urlopen(req, timeout=3).close()
        except Exception:
            pass  # painel fora do ar não pode afetar o agente


class Run:
    def __init__(self, m: Mirante, run_id: str) -> None:
        self.m, self.id, self._t0 = m, run_id, time.monotonic()

    def decision(self, model: str = "", reasoning: str = "", chosen: Iterable[str] = (),
                 confidence: Optional[float] = None, duration_ms: Optional[float] = None,
                 tokens_in: int = 0, tokens_out: int = 0,
                 alternatives: Optional[List[Dict[str, Any]]] = None) -> None:
        """chosen vazio = o modelo respondeu sem pedir tool."""
        self.m.emit({"type": "decision", "run_id": self.id, "span_id": _new_id(), "model": model,
                     "reasoning": reasoning, "chosen": list(chosen), "confidence": confidence,
                     "duration_ms": duration_ms or 0, "tokens_in": tokens_in, "tokens_out": tokens_out,
                     "alternatives": alternatives or []})

    def tool_call(self, server: str, tool: str, args: Any, rationale: str = "",
                  confidence: Optional[float] = None) -> "ToolSpan":
        span = ToolSpan(self, _new_id(), server, tool)
        self.m.emit({"type": "tool_call", "run_id": self.id, "span_id": span.id, "server": server,
                     "tool": tool, "args": args, "rationale": rationale, "confidence": confidence})
        return span

    def flag(self, level: str, code: str, reason: str) -> None:
        """level: "hallucination" (vermelho) ou "uncertain" (amarelo)."""
        self.m.emit({"type": "flag", "run_id": self.id,
                     "flag": {"level": level, "code": code, "reason": reason, "source": "agent"}})

    def end(self, output: str = "", error: Optional[BaseException | str] = None) -> None:
        ev: Dict[str, Any] = {"type": "run_end", "run_id": self.id, "output": output,
                              "duration_ms": (time.monotonic() - self._t0) * 1000}
        if error:
            ev["error"] = str(error)
        self.m.emit(ev)


class ToolSpan:
    def __init__(self, run: Run, span_id: str, server: str, tool: str) -> None:
        self.run, self.id, self.server, self.tool, self._t0 = run, span_id, server, tool, time.monotonic()

    def end(self, result: Any = None, error: Optional[BaseException | str] = None) -> None:
        ev: Dict[str, Any] = {"type": "tool_result", "run_id": self.run.id, "span_id": self.id,
                              "server": self.server, "tool": self.tool, "result": result,
                              "duration_ms": (time.monotonic() - self._t0) * 1000}
        if error:
            ev["error"] = str(error)
        self.run.m.emit(ev)
```
</details>

<details><summary><b>Cliente TypeScript/JavaScript</b> (<code>mirante.ts</code>, sem dependências)</summary>

```ts
// Cliente mínimo do mirante para TypeScript/JavaScript (Node 18+, Bun, Deno;
// sem dependências). Só sintaxe "apagável" de TS: roda direto com
// `node arquivo.ts` (Node 23.6+) ou como .js tirando as anotações de tipo.
//
// Manda eventos para POST /v1/events. Regras:
//   - nunca bloqueia nem derruba o agente: fila em memória, envio em lote
//     periódico; fila cheia ou painel fora do ar = evento descartado;
//   - URL vazia = no-op (a instrumentação pode ficar no código sempre).
//
// Uso:
//   const m = new Mirante(process.env.MIRANTE_URL, "meu-agente");
//   const run = m.startRun(pergunta, [{ server: "k8s-ts-mcp", name: "scan_cluster", schema: {...} }]);
//   run.decision({ model: "gpt-4o", reasoning: texto, chosen: ["scan_cluster"], durationMs: 812 });
//   const span = run.toolCall("k8s-ts-mcp", "scan_cluster", { cluster_id: "x" }, "porque...");
//   span.end(resultado);                 // ou span.end(undefined, erro)
//   run.end(resposta);                   // ou run.end("", erro)
//   await m.close();                     // no shutdown: envia o que falta

type Ev = Record<string, unknown>;

const newId = (): string => crypto.randomUUID().replace(/-/g, "").slice(0, 16);
const errText = (e: unknown): string | undefined =>
  e === undefined || e === null || e === "" ? undefined : e instanceof Error ? e.message : String(e);

export class Mirante {
  readonly url: string;
  readonly agent: string;
  readonly token: string | undefined;
  readonly maxQueue: number;
  private q: Ev[] = [];
  private timer: ReturnType<typeof setInterval> | undefined;

  constructor(url: string | undefined, agent: string, opts: { token?: string; flushMs?: number; maxQueue?: number } = {}) {
    this.url = (url ?? "").replace(/\/+$/, "");
    this.agent = agent;
    this.token = opts.token;
    this.maxQueue = opts.maxQueue ?? 4096;
    if (this.url) {
      this.timer = setInterval(() => void this.flush(), opts.flushMs ?? 200);
      (this.timer as { unref?: () => void }).unref?.(); // não segura o processo vivo
    }
  }

  emit(ev: Ev): void {
    if (!this.url || this.q.length >= this.maxQueue) return; // agente > painel
    this.q.push({ agent: this.agent, time: new Date().toISOString(), ...ev });
  }

  startRun(input: string, tools: Ev[] = [], user = ""): Run {
    const run = new Run(this, newId());
    this.emit({ type: "run_start", run_id: run.id, input, user, tools });
    return run;
  }

  async flush(): Promise<void> {
    while (this.q.length) {
      const batch = this.q.splice(0, 200);
      const headers: Record<string, string> = { "Content-Type": "application/json" };
      if (this.token) headers.Authorization = `Bearer ${this.token}`;
      try {
        await fetch(`${this.url}/v1/events`, { method: "POST", headers, body: JSON.stringify(batch), signal: AbortSignal.timeout(3000) });
      } catch {
        return; // painel fora do ar não pode afetar o agente
      }
    }
  }

  async close(): Promise<void> {
    if (this.timer) clearInterval(this.timer);
    if (this.url) await this.flush();
  }
}

export class Run {
  readonly m: Mirante;
  readonly id: string;
  private t0 = performance.now();

  constructor(m: Mirante, id: string) {
    this.m = m;
    this.id = id;
  }

  /** chosen vazio = o modelo respondeu sem pedir tool. */
  decision(d: { model?: string; reasoning?: string; chosen?: string[]; confidence?: number; durationMs?: number; tokensIn?: number; tokensOut?: number; alternatives?: Ev[] }): void {
    this.m.emit({
      type: "decision", run_id: this.id, span_id: newId(), model: d.model ?? "", reasoning: d.reasoning ?? "",
      chosen: d.chosen ?? [], confidence: d.confidence, duration_ms: d.durationMs ?? 0,
      tokens_in: d.tokensIn ?? 0, tokens_out: d.tokensOut ?? 0, alternatives: d.alternatives ?? [],
    });
  }

  toolCall(server: string, tool: string, args: unknown, rationale = "", confidence?: number): ToolSpan {
    const span = new ToolSpan(this, newId(), server, tool);
    this.m.emit({ type: "tool_call", run_id: this.id, span_id: span.id, server, tool, args, rationale, confidence });
    return span;
  }

  /** level: "hallucination" (vermelho) ou "uncertain" (amarelo). */
  flag(level: "hallucination" | "uncertain", code: string, reason: string): void {
    this.m.emit({ type: "flag", run_id: this.id, flag: { level, code, reason, source: "agent" } });
  }

  end(output = "", error?: unknown): void {
    this.m.emit({ type: "run_end", run_id: this.id, output, error: errText(error), duration_ms: performance.now() - this.t0 });
  }
}

export class ToolSpan {
  readonly run: Run;
  readonly id: string;
  readonly server: string;
  readonly tool: string;
  private t0 = performance.now();

  constructor(run: Run, id: string, server: string, tool: string) {
    this.run = run;
    this.id = id;
    this.server = server;
    this.tool = tool;
  }

  end(result?: unknown, error?: unknown): void {
    this.run.m.emit({
      type: "tool_result", run_id: this.run.id, span_id: this.id, server: this.server, tool: this.tool,
      result, error: errText(error), duration_ms: performance.now() - this.t0,
    });
  }
}
```
</details>

**Contrato** (outras linguagens): `POST http://mirante.observabilidade:8080/v1/events` com um objeto ou um array JSON. Tipos: `run_start` (`run_id`, `agent`, `input`, `tools[]`), `decision` (`run_id`, `span_id`, `model`, `reasoning`, `chosen[]`, `confidence`, `duration_ms`), `tool_call` (`run_id`, `span_id`, `server`, `tool`, `args`, `rationale`, `confidence`), `tool_result` (`run_id`, `span_id`, `result`, `error`, `duration_ms`), `flag` (`run_id`, `flag{level,code,reason}`), `run_end` (`run_id`, `output`, `error`). Todos levam `agent` e `time` (RFC 3339). Envie de forma assíncrona e em lote, e descarte os eventos se falhar.

## Se este repositório é um MCP server (e não um agente)

Ajude o mirante a não acusar falso `acao_nao_executada`: anote as tools que **alteram** algo com `readOnlyHint: false` (ou `destructiveHint: true`) e as de leitura com `readOnlyHint: true`, nas `annotations` do MCP. Não precisa mudar mais nada. Os agentes que usam este server passam pelo proxy normalmente.

## Passo 4 — Verificar (obrigatório; não declare pronto sem isto)

1. `curl -s http://mirante.observabilidade:8080/healthz` → `ok`. Se falhar, pare: o mirante está inacessível a partir daqui.
2. Modo A: `curl -s http://mirante.observabilidade:8080/api/upstreams` → os nomes usados nas URLs novas aparecem na lista.
3. Rode o agente (ou os testes dele) com a config nova e faça uma pergunta que use tool.
4. Confirme que o run chegou:
   `curl -s http://mirante.observabilidade:8080/api/runs | grep -o '"agent":"<agente>"' | head -1`
5. Confirme que a resposta do agente ao usuário é **a mesma** de antes da mudança.
6. Rode a suíte de testes do agente. Ela não pode ter quebrado.

Se não houver como rodar o agente aqui (sem acesso ao LLM ou ao cluster), diga isso **explicitamente** ao usuário e liste os passos 3–5 para ele executar. Não afirme que funcionou sem ter visto.

## Passo 5 — O que entregar ao usuário

- O modo escolhido e por quê (uma linha).
- O diff das configs, com **valor antigo → valor novo** de cada URL.
- O que foi verificado e o resultado de cada passo do Passo 4.
- O que o operador do mirante precisa configurar, se faltar upstream.
- Como desfazer: voltar as URLs aos valores antigos.
- Link do painel: http://mirante.observabilidade:8080

## Não faça

- Não altere prompts, modelo, lógica de tools, retries ou timeouts do agente.
- Não adicione dependências no modo A.
- Não remova a forma antiga de configurar. Mude só o valor.
- Não coloque a URL do mirante fixa no código; use config ou variável de ambiente.
- Não comite `.env` com segredos nem mande segredos para o painel.
- Não use o proxy para MCP via stdio ou SSE legado (não funciona). Use o modo C ou deixe de fora.
- Não declare sucesso sem a verificação do Passo 4.
