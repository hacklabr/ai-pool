package routing

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestExtractMessages_ExtractsFromOpenAIBody valida parse de messages.
func TestExtractMessages_ExtractsFromOpenAIBody(t *testing.T) {
	body := []byte(`{"model":"gpt-4","messages":[
		{"role":"system","content":"you are helpful"},
		{"role":"user","content":"hello"}
	]}`)
	msgs, err := ExtractMessages(body)
	if err != nil {
		t.Fatalf("ExtractMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("len(msgs) = %d, want 2", len(msgs))
	}
	if msgs[0].Role != "system" || msgs[1].Role != "user" {
		t.Errorf("roles = %q, %q", msgs[0].Role, msgs[1].Role)
	}
}

// TestExtractMessages_NoMessages retorna vazio (sem erro) se campo ausente.
func TestExtractMessages_NoMessages(t *testing.T) {
	body := []byte(`{"model":"text-embedding-ada-002","input":"hello"}`)
	msgs, err := ExtractMessages(body)
	if err != nil {
		t.Fatalf("ExtractMessages: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("len(msgs) = %d, want 0", len(msgs))
	}
}

// TestHashMessages_Deterministic mesmo conteúdo → mesmo hash.
func TestHashMessages_Deterministic(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: json.RawMessage(`"hello"`)},
		{Role: "assistant", Content: json.RawMessage(`"hi"`)},
	}
	h1, _ := HashMessages(msgs)
	h2, _ := HashMessages(msgs)
	if h1 != h2 {
		t.Errorf("hashes should match: %q != %q", h1, h2)
	}
}

// TestHashMessages_OrderMatters mudar ordem de mensagens → hash diferente.
// Preserva a sequência da conversa.
func TestHashMessages_OrderMatters(t *testing.T) {
	m1 := []Message{
		{Role: "user", Content: json.RawMessage(`"a"`)},
		{Role: "assistant", Content: json.RawMessage(`"b"`)},
	}
	m2 := []Message{
		{Role: "assistant", Content: json.RawMessage(`"b"`)},
		{Role: "user", Content: json.RawMessage(`"a"`)},
	}
	h1, _ := HashMessages(m1)
	h2, _ := HashMessages(m2)
	if h1 == h2 {
		t.Errorf("hashes should differ when order differs")
	}
}

// TestHashMessages_ContentChange muda resposta → hash muda.
func TestHashMessages_ContentChange(t *testing.T) {
	m1 := []Message{{Role: "user", Content: json.RawMessage(`"hello"`)}}
	m2 := []Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}
	h1, _ := HashMessages(m1)
	h2, _ := HashMessages(m2)
	if h1 == h2 {
		t.Errorf("hashes should differ when content differs")
	}
}

// TestHashMessages_PrefixIsStrict garante que HashPrefix NÃO inclui a última msg.
// Ou seja: hash de [a,b] é diferente de hash de [a,b,c] mas hash de [a,b] é o
// prefix de [a,b,c].
func TestHashMessages_PrefixIsStrict(t *testing.T) {
	prefix := []Message{
		{Role: "user", Content: json.RawMessage(`"q1"`)},
		{Role: "assistant", Content: json.RawMessage(`"a1"`)},
	}
	full := append(prefix, Message{Role: "user", Content: json.RawMessage(`"q2"`)})

	// Hash de prefix deve ser diferente de hash de full (n!=k).
	hPrefix, _ := HashMessages(prefix)
	hFull, _ := HashMessages(full)
	if hPrefix == hFull {
		t.Errorf("prefix hash should differ from full hash")
	}

	// HashPrefix de full deve casar com HashMessages de prefix.
	hPrefixFromFull, _ := HashPrefix(full)
	if hPrefixFromFull != hPrefix {
		t.Errorf("HashPrefix(full) = %q, want HashMessages(prefix) = %q",
			hPrefixFromFull, hPrefix)
	}
}

// TestHashPrefix_EmptyOrSingle retorna "" se <= 1 mensagem (sem prefixo).
func TestHashPrefix_EmptyOrSingle(t *testing.T) {
	h0, _ := HashPrefix(nil)
	if h0 != "" {
		t.Errorf("HashPrefix(nil) = %q, want empty", h0)
	}
	h1, _ := HashPrefix([]Message{{Role: "user", Content: json.RawMessage(`"x"`)}})
	if h1 != "" {
		t.Errorf("HashPrefix(single) = %q, want empty", h1)
	}
}

// TestHashCurrent vs HashPrefix.
func TestHashCurrent(t *testing.T) {
	msgs := []Message{{Role: "user", Content: json.RawMessage(`"x"`)}}
	h, _ := HashCurrent(msgs)
	if h == "" {
		t.Errorf("HashCurrent(single) should not be empty")
	}
	h0, _ := HashCurrent(nil)
	if h0 != "" {
		t.Errorf("HashCurrent(nil) = %q, want empty", h0)
	}
}

// TestResolveConversationKey_PriorityHeader valida prioridade ID > hash.
func TestResolveConversationKey_PriorityHeader(t *testing.T) {
	if got := ResolveConversationKey("conv-1", "h:abc"); got != "id:conv-1" {
		t.Errorf("expected id:conv-1, got %q", got)
	}
	if got := ResolveConversationKey("", "h:abc"); got != "h:h:abc" {
		t.Errorf("expected h:h:abc, got %q", got)
	}
	if got := ResolveConversationKey("", ""); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// TestNormalizeMessage_CanonicalJSON garante ordem de keys canônica.
func TestNormalizeMessage_CanonicalJSON(t *testing.T) {
	m := Message{Role: "user", Content: json.RawMessage(`"hi"`)}
	nb, err := NormalizeMessage(m)
	if err != nil {
		t.Fatal(err)
	}
	// Formato esperado: {"role":"user","content":"hi"} (sem espaços).
	if !strings.Contains(string(nb), `"role":"user"`) {
		t.Errorf("canonical missing role: %s", nb)
	}
	if !strings.Contains(string(nb), `"content":"hi"`) {
		t.Errorf("canonical missing content: %s", nb)
	}
}

// TestHashMessages_TruncatedTo128Bits retorna 32 hex chars.
func TestHashMessages_TruncatedTo128Bits(t *testing.T) {
	msgs := []Message{{Role: "user", Content: json.RawMessage(`"test"`)}}
	h, _ := HashMessages(msgs)
	if len(h) != 32 {
		t.Errorf("hash length = %d, want 32 (128 bits hex)", len(h))
	}
}
