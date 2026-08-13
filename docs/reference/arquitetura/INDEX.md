# Índice de arquitetura — AI Provider Pool

> Criado em: 2026-08-13 · Última revisão: 2026-08-13
> Regra: doc desatualizado é corrigido ou marcado como obsoleto — nunca deixado apodrecendo em silêncio.

| Documento | Conteúdo | Quando ler |
|---|---|---|
| `../architecture.md` | Arquitetura interna do sistema (hexagonal, módulos, fluxo do proxy, persistência, deploy, dívidas) | Antes de alterar regras de negócio, a estrutura de módulos ou o hot path do proxy |
| `runbooks/deploy.md` | Como publicar uma versão (local, Helm, CI/CD) | Antes de qualquer deploy |
| `runbooks/rollback.md` | Como reverter uma versão | Quando um deploy falha ou introduz regressão |
| `runbooks/incidentes.md` | Resposta a incidentes em produção | Durante um incidente |
| `../decisions/` | ADRs — registros de decisão técnica (0001–0014) | Antes de mudar uma decisão técnica |

## Regra de cabeçalho dos documentos

Todo documento de arquitetura carrega no topo: data de criação, campo
"última revisão" e a regra de obsolescência acima.
