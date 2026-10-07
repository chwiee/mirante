// Package detect aplica as heurísticas de alucinação/incerteza do mirante.
//
// Tudo aqui roda no servidor, sobre os eventos que o agente já manda — então
// funciona igual pra Qwen local, Claude no Bedrock ou qualquer outro modelo,
// sem o agente precisar fazer nada além de declarar as tools disponíveis no
// run_start. Confiança auto-reportada pelo modelo é mal calibrada, por isso
// ela é só UM dos sinais, não o único.
//
// Regra de severidade:
//   - hallucination (vermelho): evidência objetiva de que o modelo inventou algo
//     — tool que não existe, argumento fora do schema, identificador na
//     resposta que não aparece em nenhum lugar (entrada, args, resultados).
//   - uncertain (amarelo): indício de escolha duvidosa — confiança baixa,
//     alternativas empatadas, chamada repetida, resposta confiante após erro.
package detect

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/chwiee/mirante/internal/event"
)

// Config ajusta os limiares.
type Config struct {
	// MinConfidence abaixo disso vira "baixa_confianca". 0 desliga.
	MinConfidence float64
	// AmbiguityMargin: se a melhor alternativa descartada tem score a menos
	// disso da escolhida, vira "escolha_ambigua".
	AmbiguityMargin float64
	// ActionTools: tools que executam ações mesmo sem o nome dizer (ex:
	// troubleshoot do k8s-ts-mcp, que deleta pod). Somam-se às anotações MCP
	// (readOnlyHint=false / destructiveHint=true) e às palavras-chave do nome.
	ActionTools []string
}

// Default é o que o servidor usa se nada for configurado.
var Default = Config{MinConfidence: 0.6, AmbiguityMargin: 0.1}

func mk(level, code, format string, args ...any) event.Flag {
	return event.Flag{Level: level, Code: code, Reason: fmt.Sprintf(format, args...), Source: "mirante"}
}

// Decision avalia uma decisão do LLM.
func (c Config) Decision(s *event.Step) []event.Flag {
	var out []event.Flag
	if c.MinConfidence > 0 && s.Confidence != nil && *s.Confidence < c.MinConfidence {
		out = append(out, mk(event.LevelUncertain, "baixa_confianca",
			"modelo reportou confiança %.2f na decisão (mínimo %.2f)", *s.Confidence, c.MinConfidence))
	}
	if s.Confidence != nil && c.AmbiguityMargin > 0 {
		for _, a := range s.Alternatives {
			if a.Score != nil && *s.Confidence-*a.Score < c.AmbiguityMargin {
				out = append(out, mk(event.LevelUncertain, "escolha_ambigua",
					"alternativa %q ficou com score %.2f, quase empatada com a escolhida (%.2f)", a.Tool, *a.Score, *s.Confidence))
				break
			}
		}
	}
	return out
}

// ToolCall avalia uma chamada de tool contra o que foi declarado no run.
func (c Config) ToolCall(run *event.Run, s *event.Step) []event.Flag {
	var out []event.Flag

	if len(run.Tools) > 0 {
		spec := findTool(run.Tools, s.Server, s.Tool)
		if spec == nil {
			out = append(out, mk(event.LevelHallucination, "tool_inexistente",
				"modelo chamou %q, que não está entre as %d tools disponíveis (%s)",
				s.Tool, len(run.Tools), toolNames(run.Tools)))
		} else if len(spec.Schema) > 0 {
			for _, p := range validate(spec.Schema, s.Args) {
				out = append(out, mk(event.LevelHallucination, "args_invalidos", "%s", p))
			}
		}
	}

	if c.MinConfidence > 0 && s.Confidence != nil && *s.Confidence < c.MinConfidence {
		out = append(out, mk(event.LevelUncertain, "baixa_confianca",
			"modelo reportou confiança %.2f ao chamar %s (mínimo %.2f)", *s.Confidence, s.Tool, c.MinConfidence))
	}

	n := 0
	for _, prev := range run.Steps {
		if prev != s && prev.Kind == "tool" && prev.Tool == s.Tool && jsonEqual(prev.Args, s.Args) {
			n++
		}
	}
	if n > 0 {
		out = append(out, mk(event.LevelUncertain, "chamada_repetida",
			"%s chamada com os mesmos argumentos pela %dª vez neste run — modelo pode estar em loop", s.Tool, n+1))
	}
	return out
}

// RunEnd avalia a resposta final contra tudo que o agente realmente viu.
func (c Config) RunEnd(run *event.Run) []event.Flag {
	var out []event.Flag
	if strings.TrimSpace(run.Output) == "" {
		return nil
	}

	tools, failed := 0, 0
	var corpus strings.Builder
	corpus.WriteString(run.Input)
	for _, t := range run.Tools {
		corpus.WriteString("\n" + t.Name) // citar uma tool disponível não é inventar
	}
	for _, s := range run.Steps {
		if s.Kind != "tool" {
			continue
		}
		tools++
		if s.Status == "error" {
			failed++
		}
		corpus.WriteString("\n")
		corpus.WriteString(s.Tool)
		corpus.Write(s.Args)
		corpus.Write(unquote(s.Result))
		corpus.WriteString(s.Error)
	}

	if missing := ungrounded(run.Output, corpus.String()); len(missing) > 0 {
		level := event.LevelHallucination
		why := "não aparecem na entrada nem em nenhum argumento/resultado de tool"
		if tools == 0 {
			// Sem tool nenhuma o modelo pode estar usando conhecimento geral
			// legítimo — é suspeito, mas não prova.
			level = event.LevelUncertain
			why = "foram citados sem nenhuma tool ter sido chamada"
		}
		out = append(out, mk(level, "resposta_sem_base",
			"identificadores na resposta %s: %s", why, strings.Join(missing, ", ")))
	}

	var claims strings.Builder
	claims.WriteString(run.Output)
	for _, s := range run.Steps {
		if s.Kind == "decision" {
			claims.WriteString("\n" + s.Reasoning)
		}
	}
	if verb, want := c.claimedAction(claims.String(), run); verb != "" {
		out = append(out, mk(event.LevelHallucination, "acao_nao_executada",
			"o modelo diz %q, mas nenhuma tool de %s rodou com sucesso neste run (chamadas: %s)", verb, want, calledTools(run.Steps)))
	}

	if tools > 0 && failed == tools && !admitsFailure(run.Output) {
		out = append(out, mk(event.LevelUncertain, "resposta_apos_erro",
			"todas as %d chamadas de tool falharam, mas a resposta não menciona erro", tools))
	}
	return out
}

func findTool(specs []event.ToolSpec, server, name string) *event.ToolSpec {
	for i := range specs {
		if specs[i].Name == name && (server == "" || specs[i].Server == "" || specs[i].Server == server) {
			return &specs[i]
		}
	}
	return nil
}

func toolNames(specs []event.ToolSpec) string {
	names := make([]string, 0, len(specs))
	for _, s := range specs {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	if len(names) > 8 {
		names = append(names[:8], "…")
	}
	return strings.Join(names, ", ")
}

// validate é um validador JSON-schema mínimo: cobre o que modelos erram na
// prática (campo obrigatório faltando, campo inventado, tipo errado, enum).
func validate(schema map[string]any, raw json.RawMessage) []string {
	var args map[string]any
	if len(raw) == 0 {
		args = map[string]any{}
	} else if err := json.Unmarshal(raw, &args); err != nil {
		return []string{"argumentos não são um objeto JSON válido: " + err.Error()}
	}

	var probs []string
	props, _ := schema["properties"].(map[string]any)

	if req, ok := schema["required"].([]any); ok {
		for _, r := range req {
			name, _ := r.(string)
			if _, present := args[name]; !present {
				probs = append(probs, fmt.Sprintf("argumento obrigatório %q ausente", name))
			}
		}
	}

	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		p, known := props[k].(map[string]any)
		if !known {
			if ap, ok := schema["additionalProperties"].(bool); ok && !ap {
				probs = append(probs, fmt.Sprintf("argumento %q não existe no schema da tool", k))
			}
			continue
		}
		if t, ok := p["type"].(string); ok && !typeMatches(t, args[k]) {
			probs = append(probs, fmt.Sprintf("argumento %q deveria ser %s, veio %s", k, t, jsonType(args[k])))
		}
		if enum, ok := p["enum"].([]any); ok && !inEnum(enum, args[k]) {
			probs = append(probs, fmt.Sprintf("argumento %q=%v fora dos valores permitidos %v", k, args[k], enum))
		}
	}
	return probs
}

func typeMatches(t string, v any) bool {
	switch t {
	case "string":
		_, ok := v.(string)
		return ok
	case "number":
		_, ok := v.(float64)
		return ok
	case "integer":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	}
	return true
}

func jsonType(v any) string {
	switch v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	case nil:
		return "null"
	}
	return "?"
}

func inEnum(enum []any, v any) bool {
	for _, e := range enum {
		if fmt.Sprint(e) == fmt.Sprint(v) {
			return true
		}
	}
	return false
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return bytes.Equal(a, b)
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}

// unquote: se o resultado é uma string JSON, devolve o conteúdo cru, pra que
// "\n" e aspas escapadas não atrapalhem o match de identificadores.
func unquote(raw json.RawMessage) []byte {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []byte(s)
	}
	return raw
}

var tokenRe = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9._:/-]*[A-Za-z0-9]`)

// ungrounded extrai da resposta os tokens com cara de identificador
// (nome de pod, namespace, IP, versão, "75Mi"…) — letra junto com dígito ou
// separador — e devolve os que não aparecem no corpus. Palavras comuns não
// entram, porque não têm essa forma.
func ungrounded(output, corpus string) []string {
	lc := strings.ToLower(corpus)
	quoted := map[string]bool{}
	for _, m := range backtickRe.FindAllStringSubmatch(output, -1) {
		for _, tok := range tokenRe.FindAllString(m[1], -1) {
			quoted[tok] = true
		}
	}
	seen := map[string]bool{}
	var missing []string
	for _, tok := range tokenRe.FindAllString(output, -1) {
		if len(tok) < 4 || seen[tok] || !looksLikeID(tok, quoted[tok]) {
			continue
		}
		seen[tok] = true
		if !grounded(lc, tok, quoted[tok]) {
			missing = append(missing, tok)
		}
	}
	return missing
}

// grounded aceita o token se ele aparece no corpus, ou se é composto por "/"
// e cada pedaço ou aparece ou não tem cara de identificador ("120/s",
// "msg/s", "APM/banco", "kube-system/keda-operator").
func grounded(lcCorpus, tok string, quoted bool) bool {
	if strings.Contains(lcCorpus, strings.ToLower(tok)) {
		return true
	}
	if !strings.Contains(tok, "/") {
		return false
	}
	for _, part := range strings.Split(tok, "/") {
		if part != "" && looksLikeID(part, quoted) && !strings.Contains(lcCorpus, strings.ToLower(part)) {
			return false
		}
	}
	return true
}

var backtickRe = regexp.MustCompile("`([^`\n]+)`")

// looksLikeID decide se o token tem cara de identificador. Com dígito (hash
// de pod, IP, "512Mi", "v2.4.1") sempre conta. Só com separador, conta se
// tiver 2+ separadores ("eks-prd-checkout") ou vier entre crases — senão
// palavra composta do português ("bem-sucedida", visto ao vivo) vira falso
// positivo, e bolinha vermelha falsa no telão destrói a confiança no painel.
func looksLikeID(tok string, quoted bool) bool {
	hasLetter, hasDigit, hasSep := false, false, false
	seps := 0
	for _, r := range tok {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9':
			hasDigit = true
		case r == '-' || r == '_' || r == '.' || r == ':' || r == '/':
			hasSep = true
			seps++
		}
	}
	if !hasLetter && hasDigit && strings.Count(tok, ".") == 3 {
		return true // IPv4
	}
	if !hasLetter {
		return false
	}
	if hasDigit {
		return true
	}
	if isPortugueseClitic(tok) {
		return false // "verifique-se", "reiniciá-lo"…
	}
	return hasSep && (quoted || seps >= 2)
}

var clitics = []string{"-se", "-lo", "-la", "-los", "-las", "-me", "-te", "-nos", "-lhe", "-lhes", "-o", "-a"}

func isPortugueseClitic(tok string) bool {
	l := strings.ToLower(tok)
	for _, c := range clitics {
		if strings.HasSuffix(l, c) && strings.Count(l, "-") == 1 {
			return true
		}
	}
	return false
}

var failureWords = []string{"erro", "falh", "não consegui", "nao consegui", "indisponível", "error", "fail", "unable", "timeout"}

func admitsFailure(s string) bool {
	l := strings.ToLower(s)
	for _, w := range failureWords {
		if strings.Contains(l, w) {
			return true
		}
	}
	return false
}

// actionClaims liga verbos de ação em 1ª pessoa do passado ("registrei",
// "reiniciei", "I restarted") a pedaços de nome de tool que poderiam ter feito
// aquilo. Visto ao vivo com qwen2.5:7b: "Registrei essa informação" sem
// chamar record_lesson, e "Aumentei o limite de memória" sem tool nenhuma que
// altere recurso.
var actionClaims = []struct {
	re    *regexp.Regexp
	tools []string
	what  string
}{
	{regexp.MustCompile(`(?i)\b(registrei|gravei|salvei|anotei|documentei|i (?:have |'ve )?(?:recorded|saved|logged))\b`),
		[]string{"record", "save", "registr", "create", "add", "write", "insert", "lesson"}, "registro"},
	{regexp.MustCompile(`(?i)\b(reiniciei|restartei|reinicializei|i (?:have |'ve )?restarted)\b`),
		[]string{"restart", "delete", "rollout", "reboot", "approve", "exec", "run"}, "reinício"},
	{regexp.MustCompile(`(?i)\b(aumentei|diminui|reduzi|alterei|ajustei|mudei|apliquei|corrigi|escalei|atualizei|configurei|i (?:have |'ve )?(?:increased|changed|updated|applied|fixed|scaled))\b`),
		[]string{"patch", "update", "apply", "scale", "set", "edit", "approve", "exec", "run", "fix", "change"}, "alteração"},
	{regexp.MustCompile(`(?i)\b(deletei|removi|apaguei|exclui|i (?:have |'ve )?(?:deleted|removed))\b`),
		[]string{"delete", "remove", "drop", "purge", "approve", "exec", "run"}, "remoção"},
	{regexp.MustCompile(`(?i)\b(abri (?:um|uma|o|a) (?:chamado|issue|ticket|incidente)|criei|i (?:have |'ve )?(?:created|opened))\b`),
		[]string{"create", "open", "issue", "ticket", "incident", "new"}, "criação"},
	{regexp.MustCompile(`(?i)\b(enviei|mandei|notifiquei|avisei|i (?:have |'ve )?(?:sent|notified))\b`),
		[]string{"send", "notify", "post", "message", "alert", "mail"}, "envio"},
}

func calledTools(steps []*event.Step) string {
	var names []string
	for _, s := range steps {
		if s.Kind == "tool" {
			names = append(names, s.Tool+"("+s.Status+")")
		}
	}
	if len(names) == 0 {
		return "nenhuma"
	}
	return strings.Join(names, ", ")
}

func (c Config) claimedAction(text string, run *event.Run) (verb, what string) {
	for _, claim := range actionClaims {
		m := claim.re.FindString(text)
		if m == "" {
			continue
		}
		done := false
		for _, s := range run.Steps {
			if s.Kind == "tool" && s.Status == "ok" && c.canDo(run, s.Tool, claim.tools) {
				done = true
				break
			}
		}
		if !done {
			return m, claim.what
		}
	}
	return "", ""
}

// canDo: a tool pode ter feito a ação afirmada? Sim se for declarada como
// tool de ação (flag), se o MCP server anotou que ela altera algo, ou se um
// TOKEN do nome casa com a ação ("scale_deployment" → scale). Token, não
// substring: "get_settings" não é "set".
func (c Config) canDo(run *event.Run, tool string, keywords []string) bool {
	for _, t := range c.ActionTools {
		if strings.EqualFold(t, tool) {
			return true
		}
	}
	for _, spec := range run.Tools {
		if spec.Name == tool && spec.ReadOnly != nil {
			return !*spec.ReadOnly
		}
	}
	for _, tok := range strings.FieldsFunc(strings.ToLower(tool), func(r rune) bool { return r == '_' || r == '-' || r == '.' || r == '/' }) {
		for _, k := range keywords {
			// token igual (ou plural); prefixo só para radicais longos ("registr")
			if tok == k || tok == k+"s" || (len(k) >= 6 && strings.HasPrefix(tok, k)) {
				return true
			}
		}
	}
	return false
}
