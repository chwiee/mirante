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
