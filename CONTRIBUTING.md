# Contribuir com o mirante

Obrigado pelo interesse! Issues, ideias e pull requests são bem-vindos.

## Antes de abrir uma issue

- **Bug:** diga a versão (`mirante --version`), como o mirante foi iniciado (flags/env, **sem tokens**), o tipo de agente (Ollama, OpenAI-compatible, MCP, SDK, HTTP direto) e o que você esperava ver no painel.
- **Falso alerta de alucinação:** mande o run em JSON (`GET /api/runs/{id}`), com dados sensíveis removidos. É o jeito mais rápido de virar teste.
- **Vulnerabilidade:** não abra issue pública; veja [SECURITY.md](SECURITY.md).

## Rodando localmente

Requisito: Go (a versão está no `go.mod`).

```bash
make test   # go vet + go test, inclui proxy de ponta a ponta com MCP server/cliente reais
make demo   # sobe em http://localhost:8080 com tráfego sintético
```

Não há dependência externa para os testes: eles sobem MCP servers e LLMs falsos em memória.

## O que esperamos de um PR

- **Um assunto por PR**, com a motivação na descrição (o que estava errado ou faltando, e como você viu).
- **Teste junto com a mudança.** Correção de bug vem com um teste que falha sem ela. Os testes de `internal/proxy` mostram como montar um cenário de ponta a ponta.
- `gofmt` e `go vet` limpos; `go test ./...` passando.
- Mudou o template em `web/ia`? Rode `make docs-ia` (o teste falha se a cópia em `docs/ia` divergir).
- Mudou comportamento visível (flag, detecção, protocolo)? Atualize o [README](README.md) e o [guia de implantação](docs/IMPLANTACAO.md).

## Estilo

- Código simples, só biblioteca padrão quando der. Nova dependência precisa de justificativa no PR.
- Comentários explicam o **porquê** (de preferência o caso real que motivou), não o quê.
- O SDK em `pkg/mirante` é API pública: mudança incompatível só com motivo forte e anotada na release.
- Documentação e mensagens em português do Brasil. Issues e PRs em inglês também são bem-vindos.

## Licença

Ao contribuir, você concorda que sua contribuição é licenciada sob a [Apache-2.0](LICENSE), a mesma do projeto.
