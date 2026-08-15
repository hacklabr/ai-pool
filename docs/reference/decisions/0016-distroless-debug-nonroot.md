# ADR-0016 — Imagem base distroless/base-debian12:debug-nonroot

**Status:** aceito

**Contexto:** incidente de operação: falha 500 no sync do catálogo de modelos
em produção sem possibilidade de diagnóstico direto — a imagem de runtime
(`base-debian12:nonroot`, ADR-0012) não tem shell, então não há como entrar no
container para inspecionar (`kubectl exec sh` falha), verificar arquivos,
rede ou CA roots no local onde o processo roda. O diagnóstico precisou ser
inferido apenas por logs sanitizados. Operação de produção sem shell eleva o
custo de cada incidente e o tempo de recuperação (MTTR).

Alternativas consideradas:

1. **Imagem completa `debian:bookworm-slim`** — rejeitada: traria package
   manager (apt) e shell completos, ampliando a superfície de ataque
   (instalação pós-exploração, CVEs de mais pacotes); abriria caminho para
   "consertos" ad-hoc por apt em produção, que divergem da imagem buildada.
2. **Imagem de debug separada (variante `-debug` publicada ao lado da
   de produção)** — rejeitada: complexidade de duas imagens para build,
   versionar, escanear e promover; risco de a imagem de debug divergir da
   de produção (diagnóstico em ambiente que não é o mesmo); fricção
   operacional no momento do incidente (trocar imagem para diagnosticar
   muda o estado do sistema).
3. **`gcr.io/distroless/base-debian12:debug-nonroot`** — escolhida: é a
   mesma base `nonroot` (UID/GID 65532 preservado, CA roots em
   `/etc/ssl/certs`) com busybox `sh` + coreutils embutidos; **sem** package
   manager (sem apt/apk) — impossível instalar pacotes.

**Decisão:** vamos usar `gcr.io/distroless/base-debian12:debug-nonroot` como
base do stage de runtime no `Dockerfile` (substitui parcialmente ADR-0012 no
ponto da base; o restante do ADR-0012 — build multi-stage, `CGO_ENABLED=0`,
`-trimpath`, embed.FS, `TZ=UTC` — permanece válido). `USER nonroot` e o
`ENTRYPOINT` em exec form são mantidos: exec form garante que o binário é o
PID 1 e recebe os sinais diretamente (graceful shutdown), e é boa prática
independente da presença de shell.

**Consequências:**

- Positivas: shell (busybox) disponível para diagnóstico em produção
  (`kubectl exec` com `sh` funciona), reduzindo MTTR em incidentes como o do
  sync; zero mudança de pipeline — mesma família de imagem, mesmo
  `USER nonroot`, mesmos CA roots; sem nova imagem para versionar.
- Negativas: superfície de ataque levemente maior — um shell embutido dá
  ferramenta de pós-exploração a quem conseguir comprometer o processo
  (mitigações existentes: container roda como nonroot, sem package manager
  para escalar privilégio, `readOnlyRootFilesystem` no Helm); imagem
  marginalmente maior (busybox + coreutils); perde-se o argumento de
  auditoria "impossível executar shell" do ADR-0012.
- Fica mais difícil: justificar `distroless` sem shell como controle de
  segurança primário (agora é defesa-em-profundidade: nonroot +
  package manager ausente + scan trivy no CI).

**Evidências:** `Dockerfile`.

**Origem:** issue #27 (round R06); incidente de operação — 500 no sync sem
diagnóstico em produção.
