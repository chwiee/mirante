package mirante

import (
	"strconv"
	"strings"
)

// Modelos menores (Qwen 7B no Ollama, por exemplo) costumam chamar tool sem
// escrever nada antes — não tem "porquê" pra mostrar no painel. O truque é
// tornar o porquê um ARGUMENTO obrigatório: WithReasoningParams acrescenta
// "reason" e "confidence" ao schema de cada tool oferecida ao modelo, e
// SplitReasoning tira esses campos de volta antes de executar a tool de
// verdade (o MCP server nunca vê).
//
// Nomes em inglês de propósito: em teste real com qwen2.5:7b, um parâmetro
// chamado "confianca" voltou como "confiança" (com cedilha) — o modelo
// "corrigiu" a grafia, o campo não foi removido e a tool rejeitou a chamada.
// Mesmo assim, SplitReasoning aceita as variações (motivo, confiança, caixa,
// acento) porque modelo não é compilador.
//
// Confiança auto-reportada é mal calibrada — o painel trata como UM sinal,
// junto das heurísticas objetivas.

const (
	ArgReason     = "reason"
	ArgConfidence = "confidence"
)

var (
	reasonKeys     = map[string]bool{"reason": true, "motivo": true, "rationale": true, "razao": true, "_reason": true, "_motivo": true}
	confidenceKeys = map[string]bool{"confidence": true, "confianca": true, "_confidence": true, "_confianca": true}
)

// WithReasoningParams devolve uma cópia do schema com reason/confidence
// obrigatórios. O schema original não é alterado.
func WithReasoningParams(schema map[string]any) map[string]any {
	out := make(map[string]any, len(schema)+2)
	for k, v := range schema {
		out[k] = v
	}
	if out["type"] == nil {
		out["type"] = "object"
	}

	props := map[string]any{}
	if p, ok := schema["properties"].(map[string]any); ok {
		for k, v := range p {
			props[k] = v
		}
	}
	props[ArgReason] = map[string]any{
		"type":        "string",
		"description": "Em uma frase: por que esta ferramenta, com estes argumentos, é a certa para o pedido do usuário.",
	}
	props[ArgConfidence] = map[string]any{
		"type":        "number",
		"description": "Número de 0 a 1: o quanto você tem certeza de que esta é a chamada correta. Seja honesto: use valores baixos quando estiver chutando.",
	}
	out["properties"] = props

	var req []any
	switch r := schema["required"].(type) {
	case []any:
		req = append(req, r...)
	case []string:
		for _, s := range r {
			req = append(req, s)
		}
	}
	out["required"] = append(req, ArgReason, ArgConfidence)
	return out
}

// SplitReasoning separa reason/confidence (e variações) dos argumentos reais
// da tool. Devolve uma cópia de args sem esses campos.
func SplitReasoning(args map[string]any) (clean map[string]any, reason string, confidence *float64) {
	clean = make(map[string]any, len(args))
	for k, v := range args {
		switch nk := normKey(k); {
		case reasonKeys[nk]:
			if s, ok := v.(string); ok {
				reason = s
			}
		case confidenceKeys[nk]:
			confidence = toConfidence(v)
		default:
			clean[k] = v
		}
	}
	return clean, reason, confidence
}

// IsReasoningKey diz se a chave é uma das que WithReasoningParams injeta
// (em qualquer variação aceita por SplitReasoning).
func IsReasoningKey(k string) bool {
	nk := normKey(k)
	return reasonKeys[nk] || confidenceKeys[nk]
}

var unaccent = strings.NewReplacer(
	"á", "a", "à", "a", "â", "a", "ã", "a", "é", "e", "ê", "e", "í", "i",
	"ó", "o", "ô", "o", "õ", "o", "ú", "u", "ü", "u", "ç", "c",
)

// normKey: minúsculas e sem acento ("Confiança" → "confianca"). Sem
// golang.org/x/text de propósito: o SDK não deve arrastar dependência.
func normKey(k string) string {
	return unaccent.Replace(strings.ToLower(strings.TrimSpace(k)))
}

// toConfidence aceita 0.8, 80, "0.8", "80%" e devolve em [0,1].
func toConfidence(v any) *float64 {
	var f float64
	switch x := v.(type) {
	case float64:
		f = x
	case int:
		f = float64(x)
	case string:
		s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(x), "%"))
		p, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", "."), 64)
		if err != nil {
			return nil
		}
		f = p
	default:
		return nil
	}
	if f > 1 && f <= 100 {
		f /= 100
	}
	if f < 0 || f > 1 {
		return nil
	}
	return &f
}
