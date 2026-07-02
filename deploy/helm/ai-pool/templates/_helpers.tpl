{{/*
Expand the name of the chart.
*/}}
{{- define "ai-pool.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified app name (release-name + chart-name).
Usado em todos os recursos para nomes únicos no namespace.
*/}}
{{- define "ai-pool.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Chart name + version label (para recursos Kubernetes).
*/}}
{{- define "ai-pool.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Selector labels — usados em matchLabels do Deployment/Service.
DEVE ser imutável entre versões do chart para que RollingUpdate funcione.
*/}}
{{- define "ai-pool.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ai-pool.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Common labels — injetadas em TODOS os recursos (labels + Helm metadata).
Inclui versão do chart e appVersion para auditoria.
*/}}
{{- define "ai-pool.commonLabels" -}}
{{ include "ai-pool.selectorLabels" . }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ include "ai-pool.chart" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end -}}

{{/*
Nome do ServiceAccount.
Se serviceAccount.name está setado, usa. Senão, deriva do fullname.
*/}}
{{- define "ai-pool.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "ai-pool.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Nome do Secret de database (DATABASE_URL).
Se database.existingSecret está setado, usa-o. Senão, deriva do fullname.
*/}}
{{- define "ai-pool.dbSecretName" -}}
{{- if .Values.database.existingSecret -}}
{{- .Values.database.existingSecret -}}
{{- else -}}
{{- printf "%s-db" (include "ai-pool.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
Nome do Secret de secrets de app (KEK + token_pepper) montados como arquivo.
Sempre deriva do fullname (não é overridable — é propriedade do chart).
*/}}
{{- define "ai-pool.appSecretName" -}}
{{- printf "%s-secrets" (include "ai-pool.fullname" .) -}}
{{- end -}}

{{/*
Nome do ConfigMap de configuração não-sensível.
*/}}
{{- define "ai-pool.configMapName" -}}
{{- printf "%s-config" (include "ai-pool.fullname" .) -}}
{{- end -}}

{{/*
Image reference (repository:tag). Helper para consistência.
*/}}
{{- define "ai-pool.image" -}}
{{- printf "%s:%s" .Values.image.repository .Values.image.tag -}}
{{- end -}}

{{/*
Nome do Job de migrations.
Sufixo "-migrate" para distinguir do Deployment.
*/}}
{{- define "ai-pool.migrationJobName" -}}
{{- printf "%s-migrate" (include "ai-pool.fullname" .) -}}
{{- end -}}

{{/*
Nome da NetworkPolicy.
*/}}
{{- define "ai-pool.networkPolicyName" -}}
{{- printf "%s-netpol" (include "ai-pool.fullname" .) -}}
{{- end -}}

{{/*
ExternalSecret name.
*/}}
{{- define "ai-pool.externalSecretName" -}}
{{- printf "%s-eso" (include "ai-pool.fullname" .) -}}
{{- end -}}

{{/*
Nome canônico do PostgreSQL StatefulSet/Service/PVC.
Sempre deriva do fullname. Usado por todos os templates postgresql-*.
*/}}
{{- define "ai-pool.postgresqlFullname" -}}
{{- printf "%s-postgresql" (include "ai-pool.fullname" .) -}}
{{- end -}}

{{/*
Nome do Secret do PostgreSQL (senha + DATABASE_URL).

Quando postgresql.enabled=true, este Secret é criado pelo template
postgresql-secret.yaml com as chaves:
  - password      (consumida pelo StatefulSet do postgres)
  - DATABASE_URL  (consumida pela app + migration-job)

Quando database.existingSecret está setado (DB externo/gerenciado),
retorna o nome do Secret pré-existente (mesma semântica do helper dbSecretName).
*/}}
{{- define "ai-pool.postgresqlSecretName" -}}
{{- if .Values.database.existingSecret -}}
{{- .Values.database.existingSecret -}}
{{- else -}}
{{- printf "%s-db" (include "ai-pool.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
DATABASE_URL: construída quando postgresql.enabled=true.
Usada no postgresql-secret.yaml para gravar a chave DATABASE_URL no Secret.
Aponta para o Service do postgres criado neste mesmo release.

Fallback: se postgresql.enabled=false, retorna vazio (a URL deve vir de
database.existingSecret ou database.url).
*/}}
{{- define "ai-pool.databaseURL" -}}
{{- if .Values.postgresql.enabled -}}
{{- $pgUser := default "aipp" .Values.postgresql.auth.username -}}
{{- $pgDB := default "aipp" .Values.postgresql.auth.database -}}
{{- $pgHost := include "ai-pool.postgresqlFullname" . -}}
{{- printf "postgres://%s:%s@%s:5432/%s?sslmode=disable" $pgUser "__POSTGRES_PASSWORD__" $pgHost $pgDB -}}
{{- else -}}
{{- "" -}}
{{- end -}}
{{- end -}}
