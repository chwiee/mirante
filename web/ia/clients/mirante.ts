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
