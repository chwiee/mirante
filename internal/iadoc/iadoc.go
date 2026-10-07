// Package iadoc gera a instrução "integrar o mirante neste agente" para
// assistentes de IA (Claude Code, Kiro…), a partir de um template único em
// web/ia. Três saídas com o mesmo corpo:
//
//   - Markdown puro, servido em /ia/integrar-mirante.md (a IA lê via curl);
//   - skill do Claude Code (.claude/skills/integrar-mirante/SKILL.md);
//   - steering do Kiro (.kiro/steering/mirante.md, inclusão manual: #mirante).
//
// Servida pelo próprio mirante, a instrução já sai com a URL real e os
// upstreams configurados — a IA não precisa adivinhar nada. A cópia estática
// em docs/ia é mantida em dia por teste (golden).
package iadoc

import (
	"bytes"
	"sort"
	"strings"
	"text/template"

	"github.com/chwiee/mirante/web"
)

// Upstream é um destino configurado no proxy do mirante.
type Upstream struct{ Name, URL string }

// Data é o que muda entre a versão viva (servida) e a estática (docs/ia).
type Data struct {
	BaseURL         string
	Live            bool // true = gerada pelo mirante em execução, com upstreams reais
	LLM, MCP        []Upstream
	InjectReasoning bool
}

// StaticData é o que vai para a cópia versionada em docs/ia.
var StaticData = Data{BaseURL: "http://mirante.observabilidade:8080"}

const (
	SkillName = "integrar-mirante"

	skillFront = `---
name: integrar-mirante
description: Conecta este agente de IA ao mirante, o painel em tempo real que mostra decisões do LLM, tool calls (payload, retorno, tempo) e alucinações. Use quando pedirem para integrar, conectar, plugar ou instrumentar o agente no mirante, no painel ou telão de agentes, ou para ver as chamadas de tool do agente no mirante.
---

`
	steeringFront = `---
inclusion: manual
---

`
)

var tmpl = template.Must(template.New("integrar-mirante.md.tmpl").Delims("[[", "]]").
	ParseFS(web.IA, "ia/integrar-mirante.md.tmpl"))

// Markdown renderiza a instrução pura.
func Markdown(d Data) (string, error) {
	py, err := web.IA.ReadFile("ia/clients/mirante_client.py")
	if err != nil {
		return "", err
	}
	ts, err := web.IA.ReadFile("ia/clients/mirante.ts")
	if err != nil {
		return "", err
	}
	d.BaseURL = strings.TrimRight(d.BaseURL, "/")
	sortUp(d.LLM)
	sortUp(d.MCP)
	var b bytes.Buffer
	err = tmpl.Execute(&b, struct {
		Data
		PythonClient, TSClient string
	}{d, strings.TrimRight(string(py), "\n"), strings.TrimRight(string(ts), "\n")})
	return b.String(), err
}

// Skill é a instrução no formato de skill do Claude Code.
func Skill(d Data) (string, error) {
	md, err := Markdown(d)
	return skillFront + md, err
}

// Steering é a instrução no formato de steering do Kiro (inclusão manual).
func Steering(d Data) (string, error) {
	md, err := Markdown(d)
	return steeringFront + md, err
}

// FromUpstreams monta Data a partir do mapa de /api/upstreams.
func FromUpstreams(base string, up map[string]any) Data {
	d := Data{BaseURL: base, Live: true}
	conv := func(v any) []Upstream {
		m, _ := v.(map[string]string)
		out := make([]Upstream, 0, len(m))
		for k, u := range m {
			out = append(out, Upstream{k, u})
		}
		return out
	}
	d.LLM, d.MCP = conv(up["llm_urls"]), conv(up["mcp_urls"])
	d.InjectReasoning, _ = up["inject_reasoning"].(bool)
	return d
}

func sortUp(u []Upstream) { sort.Slice(u, func(i, j int) bool { return u[i].Name < u[j].Name }) }
