package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hacklabr/ai-pool/internal/config"
	"github.com/hacklabr/ai-pool/internal/usage"
)

// =============================================================================
// Streaming SSE — repasse chunked com http.Flusher + 3 timeouts.
//
// Princípio (spec §3.2.1, Backend Architect T1 §3.4):
//   - Flush IMEDIATO a cada chunk lido do upstream (sem buffering).
//   - 3 timeouts via context + timers (não via http.Client.Timeout, que
//     cortaria streams longos):
//       a) Overall:     cap absoluto (10min default). Cancela tudo.
//       b) First-byte:  tempo até o PRIMEIRO chunk ÚTIL (status 2xx + corpo
//                       começando a chegar). Permite failover (F5) se o
//                       provedor não responde.
//       c) Idle:        intervalo MÁXIMO entre chunks. Se o provedor "trava"
//                       em meio ao stream, aborta (não segura conexão/cliente).
//   - Propagação RIGOROSA de r.Context().Done(): se o cliente desconecta,
//     cancelamos o upstream (poupando tokens/custo no provedor).
//
// FAILOVER PÓS-PRIMEIRO-CHUNK (spec §3.2.1):
//   - Uma vez que começamos a escrever a resposta ao cliente (status 2xx ou
//     qualquer chunk enviado), NÃO há failover transparente — o cliente já
//     recebeu parte da resposta. Erros neste ponto são TERMINAIS:
//     enviamos um chunk de erro SSE e encerramos.
//   - Ponto de extensão para F5: a função streamResponse retorna um sinal
//     `committed` indicando se já houve commit (escrita ao cliente). O caller
//     decide se pode tentar outra chave.
// =============================================================================

// streamConfig agrega os 3 timeouts (seconds-level).
type streamConfig struct {
	FirstByte time.Duration
	Idle      time.Duration
	Overall   time.Duration
}

// streamConfigFromCfg monta a configuração a partir do config global.
func streamConfigFromCfg(cfg *config.Config) streamConfig {
	return streamConfig{
		FirstByte: cfg.UpstreamFirstByteTimeout,
		Idle:      cfg.UpstreamIdleChunkTimeout,
		Overall:   cfg.UpstreamOverallTimeout,
	}
}

// streamResult é retornado por streamResponse para o orquestrador (Server).
//
// O campo Committed indica se algum byte já foi enviado ao cliente. Se true,
// qualquer erro upstream posterior é terminal (não há failover transparente).
type streamResult struct {
	Committed bool
	// StatusCode é o status HTTP que escrevemos ao cliente (0 se nada foi
	// escrito antes de um erro — caller pode tentar outra chave).
	StatusCode int
	// Err é o erro que abortou o stream (nil em sucesso).
	Err error
	// Tokens consumidos (parseados do body do provedor — campo usage do JSON
	// OpenAI). Zeros se o provedor não reportar (ex.: stream sem include_usage).
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
}

// handleUpstreamResponse é a função central de "pós-envio".
//
// Recebe a resposta do provedor (resp.Body ainda aberto) e decide:
//   - Se status != 2xx E ainda não escrevemos nada ao cliente → repassa a
//     resposta do provedor AO CLIENTE (mantendo status/body) para que o SDK
//     OpenAI receba o erro do provedor (ex.: 400 bad request, 429 rate limit).
//     Isto é importante porque o cliente OpenAI precisa ver os erros reais do
//     provedor (ex.: para retry automático). Retorna Committed=true.
//   - Se status == 2xx E Content-Type indica SSE → faz o streaming chunked
//     com flush imediato + 3 timeouts. Retorna Committed=true (após primeiro
//     flush) ou false (antes do primeiro flush, se first-byte timeout dispara).
//   - Se status == 2xx E NÃO é SSE → copia o body de uma vez (JSON único).
//     Retorna Committed=true após começar a escrever.
//
// Erros de rede/io durante o streaming devolvem Err=true; se já committed,
// o stream foi abortado mid-way (cliente recebeu resposta parcial).
//
// Caller é responsável por chamar resp.Body.Close().
//
// HandleUpstreamResponse é a forma EXPORTADA para uso pelo wrapper failover.
func (s *Server) handleUpstreamResponse(
	w http.ResponseWriter,
	clientCtx context.Context,
	resp *http.Response,
	isStream bool,
	cfg streamConfig,
) streamResult {
	// === Erro do provedor (status não-2xx) ===
	// Repassamos a resposta do provedor ao cliente para que o SDK OpenAI
	// interprete o erro corretamente. NÃO há failover aqui em F3 (caller
	// decide se refaz — em F3, qualquer erro é terminal).
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return s.copyUpstreamResponse(w, resp) // committed=true após escrever
	}

	// === Sucesso (2xx) ===
	contentType := resp.Header.Get("Content-Type")
	isSSE := isStream && (strings.Contains(contentType, "text/event-stream") || resp.TransferEncoding != nil)

	// Streaming SSE: copia chunked com flush + timeouts.
	if isSSE {
		return s.streamSSE(w, clientCtx, resp, cfg)
	}

	// Não-streaming (JSON único): copia body normalmente. Sem timeouts de
	// first-byte/idle (overall já é protegido pelo context do request).
	return s.copyUpstreamResponse(w, resp)
}

// copyUpstreamResponse copia status + headers + body do upstream para o cliente.
// Usado para respostas de erro do provedor e respostas JSON não-streaming.
//
// Retorna streamResult com Committed=true após começar a escrever (status).
func (s *Server) copyUpstreamResponse(w http.ResponseWriter, resp *http.Response) streamResult {
	// Repassa headers relevantes (Content-Type, etc.). Remove hop-by-hop.
	dst := w.Header()
	for h, vals := range resp.Header {
		hc := http.CanonicalHeaderKey(h)
		if isHopByHop(hc) {
			continue
		}
		// Remover Content-Length se vamos copiar via io.Copy (pode diferir).
		if hc == "Content-Length" {
			continue
		}
		for _, v := range vals {
			dst.Add(hc, v)
		}
	}
	dst.Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)

	// Flush inicial (caso o cliente esteja esperando início da resposta).
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Captura o body via TeeReader para extrair tokens do campo usage (apenas
	// respostas 2xx têm usage; erros do provedor retornam zeros).
	var buf bytes.Buffer
	_, err := io.Copy(w, io.TeeReader(resp.Body, &buf))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	if err != nil {
		// Erro durante cópia: já committed (status foi enviado). Cliente
		// recebeu resposta parcial.
		return streamResult{Committed: true, StatusCode: resp.StatusCode,
			Err: fmt.Errorf("copy body: %w", err)}
	}
	res := streamResult{Committed: true, StatusCode: resp.StatusCode}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if stats, ok := usage.ParseUsageFromBody(buf.Bytes()); ok {
			res.PromptTokens = ptrIntOr(stats.PromptTokens)
			res.CompletionTokens = ptrIntOr(stats.CompletionTokens)
			res.CachedTokens = ptrIntOr(stats.CachedTokens)
		}
	}
	return res
}

// streamSSE faz o pipe chunked do upstream para o cliente com flush imediato
// e os 3 timeouts.
//
// Mecanismo dos timeouts:
//   - overallCtx (filho de clientCtx): cancela TUDO após cfg.Overall.
//   - firstByteTimer: dispara após cfg.FirstByte se nenhum byte foi escrito.
//     Se disparar ANTES do primeiro flush → não committed, caller pode failover.
//   - idleTimer: resetado a cada chunk lido. Dispara após cfg.Idle sem chunks.
//     Já committed — aborta stream mid-way.
//
// Mecanismo de cancelamento do cliente:
//   - clientCtx (r.Context()) já é o pai. Ao Done(), overallCtx cancela,
//     resp.Body.Read() retorna erro, goroutine termina.
func (s *Server) streamSSE(
	w http.ResponseWriter,
	clientCtx context.Context,
	resp *http.Response,
	cfg streamConfig,
) streamResult {
	// Verifica suporte a Flusher — sem ele não há streaming real.
	flusher, ok := w.(http.Flusher)
	if !ok {
		// Sem Flusher, cai para cópia simples (pode quebrar UX mas funciona).
		return s.copyUpstreamResponse(w, resp)
	}

	// Setup headers SSE no cliente.
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	// Crítico para nginx/reverse proxies: desabilita buffering.
	hdr.Set("X-Accel-Buffering", "no")

	// NÃO chamamos WriteHeader ainda — esperamos o primeiro chunk útil do
	// upstream para confirmar que o provedor está realmente respondendo.
	// Isto preserva a possibilidade de failover (F5) até o commit.

	// Context com overall timeout (filho de clientCtx — herda cancelamento do cliente).
	overallCtx, overallCancel := context.WithTimeout(clientCtx, cfg.Overall)
	defer overallCancel()

	// Timers de first-byte e idle.
	firstByteCh := time.NewTimer(cfg.FirstByte)
	defer firstByteCh.Stop()

	idleReset := make(chan struct{}, 1)
	// Pré-alimenta um "reset" para iniciar o idle timer no primeiro chunk.
	idleReset <- struct{}{}

	// Canal de erro interno (rede/io).
	errCh := make(chan error, 1)
	// Canal de bytes lidos do upstream.
	type chunk struct {
		data []byte
	}
	chunkCh := make(chan chunk, 8) // buffer pequeno para suavizar bursts

	// Goroutine LEITORA do upstream. Cancelável via overallCtx.
	go func() {
		defer close(chunkCh)
		buf := make([]byte, 8192) // 8KB por read — bom trade-off latência/throughput
		for {
			// Verifica cancelamento antes de ler (evita leak em shutdown).
			select {
			case <-overallCtx.Done():
				errCh <- overallCtx.Err()
				return
			default:
			}
			n, err := resp.Body.Read(buf)
			if n > 0 {
				// CÓPIA do slice — buf será reusado no próximo Read.
				out := make([]byte, n)
				copy(out, buf[:n])
				select {
				case chunkCh <- chunk{data: out}:
				case <-overallCtx.Done():
					errCh <- overallCtx.Err()
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					errCh <- err
				}
				return
			}
		}
	}()

	// Loop principal de seleção entre chunks/timers/cancel.
	committed := false
	// usageBuf acumula os chunks para extrair tokens do chunk final (best-effort:
	// OpenAI só envia usage em stream se stream_options.include_usage=true).
	// Cap de 4MB evita OOM em respostas gigantes (usage vem num chunk pequeno
	// no final — capturado enquanto houver espaço no buffer).
	var usageBuf bytes.Buffer
	const usageBufCap = 4 * 1024 * 1024
	var idleTimer *time.Timer
	var idleC <-chan time.Time
	// Inicializa idleTimer lazily — só disparamos após primeiro chunk útil,
	// pois antes disso o firstByteTimer é o relevante.

	for {
		select {
		case <-overallCtx.Done():
			// Cancelado (cliente desconectou OU overall expirou).
			err := overallCtx.Err()
			if errors.Is(err, context.DeadlineExceeded) {
				err = errors.New("stream overall timeout")
			}
			return streamResult{Committed: committed, Err: err}

		case <-firstByteCh.C:
			// First-byte timeout: SEM commit ainda → caller pode failover (F5).
			if !committed {
				return streamResult{
					Committed: false,
					Err:       errors.New("stream first-byte timeout"),
				}
			}
			// Se já committed, este timer é ignorado (já descarregado).

		case <-idleReset:
			// (Re)inicia idle timer. Recebe sinal do loop após cada chunk.
			if idleTimer != nil {
				idleTimer.Stop()
			}
			idleTimer = time.NewTimer(cfg.Idle)
			idleC = idleTimer.C

		case <-idleC:
			// Idle timeout: provedor travou. Se committed, aborta stream.
			return streamResult{
				Committed: committed,
				Err:       errors.New("stream idle timeout (no chunks within window)"),
			}

		case c, ok := <-chunkCh:
			if !ok {
				// Upstream fechou o body normalmente (EOF).
				// Commit final flush.
				if committed {
					flusher.Flush()
				}
				res := streamResult{Committed: committed, StatusCode: http.StatusOK}
				if committed {
					if stats, ok := usage.ParseUsageFromStreamBody(&usageBuf); ok {
						res.PromptTokens = ptrIntOr(stats.PromptTokens)
						res.CompletionTokens = ptrIntOr(stats.CompletionTokens)
						res.CachedTokens = ptrIntOr(stats.CachedTokens)
					}
				}
				return res
			}
			// Primeiro chunk útil — COMMIT (escreve status 200 + headers).
			if !committed {
				w.WriteHeader(http.StatusOK)
				committed = true
			}
			if _, err := w.Write(c.data); err != nil {
				// Cliente desconectou durante write.
				return streamResult{Committed: true, StatusCode: http.StatusOK,
					Err: fmt.Errorf("client write: %w", err)}
			}
			// Acumula o chunk para extrair usage do chunk final (cap anti-OOM).
			if usageBuf.Len() < usageBufCap {
				usageBuf.Write(c.data)
			}
			flusher.Flush()
			// Reset idle timer para o próximo chunk.
			select {
			case idleReset <- struct{}{}:
			default: // já sinalizado
			}

		case err := <-errCh:
			// Erro de io/rede do upstream. Se committed, stream abortado mid-way.
			return streamResult{Committed: committed, Err: err}
		}
	}
}

// isHopByHop verifica se o header (canonical) está na lista hop-by-hop.
func isHopByHop(canonical string) bool {
	for _, h := range hopByHopHeaders {
		if h == canonical {
			return true
		}
	}
	return false
}

// HandleResponseResult é o resultado de HandleUpstreamResponseExported.
// Forma pública para o wrapper failover.
type HandleResponseResult struct {
	Committed  bool
	StatusCode int
	Err        error
	// Tokens consumidos (campo usage do JSON OpenAI). Zeros se não reportados.
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
}

// HandleUpstreamResponseExported é a versão pública de handleUpstreamResponse.
// Usada pelo wrapper failover (que tem acesso ao *Server).
//
// O caller é responsável por fechar resp.Body após esta chamada.
func (s *Server) HandleUpstreamResponseExported(
	w http.ResponseWriter,
	clientCtx context.Context,
	resp *http.Response,
	isStream bool,
) HandleResponseResult {
	r := s.handleUpstreamResponse(w, clientCtx, resp, isStream, s.streamCfg)
	return HandleResponseResult{
		Committed:        r.Committed,
		StatusCode:       r.StatusCode,
		Err:              r.Err,
		PromptTokens:     r.PromptTokens,
		CompletionTokens: r.CompletionTokens,
		CachedTokens:     r.CachedTokens,
	}
}

// ptrIntOr deref um *int com fallback 0 (nil → 0).
func ptrIntOr(p *int) int {
	if p != nil {
		return *p
	}
	return 0
}
