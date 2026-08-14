package routing

import (
	"regexp"
	"strings"
)

// reWhitespaceCollapse colapsa whitespace (newlines, tabs, espaços múltiplos)
// em mensagens de erro do provedor — mensagens em uma linha para log/auditoria.
var reWhitespaceCollapse = regexp.MustCompile(`\s+`)

// Limites de sanitização: o input é truncado ANTES de processar (defesa DoS
// para bodies grandes) e a saída é limitada (auditoria legível, sem dump).
const (
	sanitizeMaxInput  = 400
	sanitizeMaxOutput = 200
)

// SanitizeUpstreamMessage extrai um trecho curto (≤200 chars) e sanitizado do
// body de erro do provedor para fins de log, UI e auditoria (issue #19).
//
// É a ÚNICA função de sanitização de mensagens upstream do sistema (o padrão
// antes duplicado no key_tester foi extraído para cá): colapsa whitespace,
// remove newlines e trunca. Recebe APENAS bodies de erro upstream — nunca
// contém a chave de API (o header Authorization não é repassado ao erro).
func SanitizeUpstreamMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	s := string(body)
	// Limita o input antes de processar (defesa DoS para bodies grandes).
	if len(s) > sanitizeMaxInput {
		s = s[:sanitizeMaxInput]
	}
	s = reWhitespaceCollapse.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	if len(s) > sanitizeMaxOutput {
		s = s[:sanitizeMaxOutput] + "..."
	}
	return s
}
