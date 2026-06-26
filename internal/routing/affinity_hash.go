// Package routing implementa o Routing Engine do AI Provider Pool.
//
// Componentes (F5):
//   - RoutingEngine: integra LRUStore + AffinityStore + KeyRepo. Implementa
//     domain.RoutingEngine (estende domain.KeyResolver).
//   - FailoverChain: orquestrador STATEFUL que tenta múltiplas chaves em caso
//     de erro classificado como failover.
//   - AutoDisableManager: contador de erros por chave em janela deslizante +
//     threshold + ErrorParser registry → TransitionToAutoDisabled.
//   - ParserRegistry + DefaultErrorParser: regex genérico para datas em
//     mensagens de erro (ISO 8601, epoch, relativo "in N seconds").
//   - ReactivationScheduler: goroutine que chama KeyRepo.ListToReactivate
//     periodicamente + OnKeyReenabled no LRUStore.
//
// Decisões:
//   - AffinityStore é BEST-EFFORT e em memória (LGPD: hash nunca persistido).
//   - FailoverChain é orquestrador externo ao pipeline (spec §3.2.3):
//     mantém estado de chaves tentadas entre chamadas ao engine.
//   - Auto-disable usa write-through para transições (CAS atômico no DB);
//     contadores em janela são in-memory (write amplification evitado).
package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// =============================================================================
// Hash de afinidade — deriva um conversationKey estável do conteúdo das
// mensagens quando o cliente NÃO envia X-Conversation-ID.
//
// Esquema:
//   1. Parse messages do body OpenAI (array de {role, content, ...}).
//   2. Normaliza cada mensagem: ordena keys, mantém apenas role+content+
//      tool_calls (campos semanticamente significantes).
//   3. Hash SHA-256 dos bytes normalizados, truncado para 128 bits (32 hex).
//
// Continuidade de conversa (spec §3.2.2):
//   - "Continuidade" = array de mensagens anterior é PREFIXO ESTRITO do array
//     atual (n > k, onde n=len(atual), k=len(anterior)).
//   - Para detectar: o routing engine computa h_curr (hash de todas as msgs
//     atuais) e h_prev (hash das mensagens SEM a última). h_prev casaria com
//     o hash armazenado na requisição anterior (que era h_curr naquela época).
//   - Arrays idênticos (n == k) NÃO são continuidade (repetição, não seguimento).
//
// Segurança (spec §3.3.7):
//   - Hash é FINGERPRINT de conteúdo. JAMAIS aparece em logs/métricas/traces.
//   - Não persistir — só em memória (AffinityStore), TTL curto (30min).
//   - X-Conversation-ID é preferido pois não deriva de conteúdo.
// =============================================================================

// Message é a forma canônica usada para hashing.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string OU array (multimodal); mantemos RawMessage
	// ToolCalls etc. podem ser adicionados aqui se tornarem-se relevantes.
	// Por ora, role+content é suficiente para detectar continuidade de chat.
}

// ExtractMessages faz parse PARCIAL do body OpenAI extraindo apenas o array
// "messages" como []Message. Campos desconhecidos são ignorados.
//
// Retorna erro se o body não é JSON válido. Retorna slice vazio (sem erro) se
// o campo "messages" está ausente (ex.: embeddings endpoint).
func ExtractMessages(body []byte) ([]Message, error) {
	var partial struct {
		Messages []Message `json:"messages"`
	}
	if err := json.Unmarshal(body, &partial); err != nil {
		return nil, err
	}
	return partial.Messages, nil
}

// NormalizeMessage serializa uma Message em forma canônica JSON.
// Ordenação de keys é garantida pelo encoding/json (Go usa ordem alfabética
// automática para struct fields, mas para preservar Content como RawMessage
// fazemos marshal explícito).
func NormalizeMessage(m Message) ([]byte, error) {
	return json.Marshal(struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}{Role: m.Role, Content: m.Content})
}

// HashMessages computa o hash SHA-256 (truncado 128 bits) do array de mensagens
// normalizadas. Usado como conversationKey no AffinityStore.
//
// O hash é DETERMINÍSTICO: mesmo array de mensagens produz mesmo hash. Ordem
// das mensagens importa (preserva sequência da conversa).
//
// Aplicações:
//   - HashMessages(msgs):               hash de TODAS as mensagens (h_curr).
//   - HashMessages(msgs[:len(msgs)-1]): hash de todas exceto última (h_prev).
//     h_prev é o conversationKey de Lookup; h_curr é o Remember key.
//
// Retorna string hex de 32 chars (128 bits).
func HashMessages(msgs []Message) (string, error) {
	h := sha256.New()
	for _, m := range msgs {
		nb, err := NormalizeMessage(m)
		if err != nil {
			return "", err
		}
		h.Write(nb)
		// Separador entre mensagens (evita ambiguidade boundary).
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	// Trunca para 16 bytes (128 bits) — sem risco de colisão relevante.
	return hex.EncodeToString(sum[:16]), nil
}

// HashPrefix computa o hash das mensagens SEM a última (prefixo estrito).
// Retorna "" se há 0 ou 1 mensagens (não há prefixo).
func HashPrefix(msgs []Message) (string, error) {
	if len(msgs) <= 1 {
		return "", nil
	}
	return HashMessages(msgs[:len(msgs)-1])
}

// HashCurrent computa o hash de TODAS as mensagens (h_curr).
// Retorna "" se há 0 mensagens.
func HashCurrent(msgs []Message) (string, error) {
	if len(msgs) == 0 {
		return "", nil
	}
	return HashMessages(msgs)
}

// ResolveConversationKey combina X-Conversation-ID explícito com hash fallback.
//
// Regra (spec §3.2.2):
//   - Se conversationID != "" → usa "id:"+conversationID (preferido; não deriva
//     de conteúdo, seguro para logs).
//   - Senão se prefixHash != "" → usa "h:"+prefixHash (fallback heurístico).
//   - Senão → "" (primeira mensagem, sem continuidade estabelecida).
//
// Importante: o conversationKey RESULTANTE é o que passa para AffinityStore.
// O prefix "id:" vs "h:" permite diferenciar a origem (útil para debugging,
// mas o conteúdo do hash NUNCA é logado).
func ResolveConversationKey(conversationID, prefixHash string) string {
	if conversationID != "" {
		return "id:" + conversationID
	}
	if prefixHash != "" {
		return "h:" + prefixHash
	}
	return ""
}
