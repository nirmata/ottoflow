{{/*
Expand the name of the chart.
*/}}
{{- define "ottoflow.name" -}}
{{- default .Chart.Name .Values.controller.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "ottoflow.fullname" -}}
{{- if .Values.controller.fullnameOverride }}
{{- .Values.controller.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.controller.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "ottoflow.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "ottoflow.labels" -}}
helm.sh/chart: {{ include "ottoflow.chart" . }}
{{ include "ottoflow.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: ottoflow
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Common labels with indent (avoids | in .yaml templates for linter)
*/}}
{{- define "ottoflow.labels.n4" -}}
{{- include "ottoflow.labels" . | nindent 4 -}}
{{- end }}
{{- define "ottoflow.labels.n8" -}}
{{- include "ottoflow.labels" . | nindent 8 -}}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "ottoflow.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ottoflow.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
control-plane: controller-manager
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "ottoflow.serviceAccountName" -}}
{{- if .Values.controller.serviceAccount.create }}
{{- default "controller-manager" .Values.controller.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.controller.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Create the name of the ClusterRole workflow-runner ServiceAccounts are bound to
*/}}
{{- define "ottoflow.workflowRunnerClusterRole" -}}
{{- .Values.workflowRunner.clusterRole | default (printf "%s-runner-role" (include "ottoflow.fullname" .)) -}}
{{- end -}}

{{/*
Create namespace name (uses release namespace; user specifies via helm -n)
*/}}
{{- define "ottoflow.namespace" -}}
{{- default .Release.Namespace .Values.namespace.name }}
{{- end }}

{{/*
Create controller image
*/}}
{{- define "ottoflow.controllerImage" -}}
{{- if .Values.controller.image.fullOverride }}
{{- .Values.controller.image.fullOverride }}
{{- else }}
{{- $registry := .Values.global.imageRegistry }}
{{- if .Values.controller.image.registry }}
{{- $registry = .Values.controller.image.registry }}
{{- end }}
{{- if $registry }}
{{- printf "%s/%s:%s" $registry .Values.controller.image.repository (.Values.controller.image.tag | default (printf "v%s" .Chart.AppVersion)) }}
{{- else }}
{{- printf "%s:%s" .Values.controller.image.repository (.Values.controller.image.tag | default (printf "v%s" .Chart.AppVersion)) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create image pull policy
*/}}
{{- define "ottoflow.imagePullPolicy" -}}
{{- .Values.global.imagePullPolicy }}
{{- end }}

{{/*
Create image pull secrets
*/}}
{{- define "ottoflow.imagePullSecrets" -}}
{{- if .Values.global.imagePullSecrets }}
imagePullSecrets:
{{- range .Values.global.imagePullSecrets }}
  - name: {{ . }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Agent Executor fullname
*/}}
{{- define "ottoflow.agentExecutor.fullname" -}}
{{- if .Values.agentExecutor.fullnameOverride }}
{{- .Values.agentExecutor.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default "agent-executor" .Values.agentExecutor.nameOverride }}
{{- if contains $name .Release.Name }}
{{- printf "%s-%s" .Release.Name "agent-executor" | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Agent Executor labels
*/}}
{{- define "ottoflow.agentExecutor.labels" -}}
helm.sh/chart: {{ include "ottoflow.chart" . }}
{{ include "ottoflow.agentExecutor.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: ottoflow
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Agent Executor selector labels
*/}}
{{- define "ottoflow.agentExecutor.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ottoflow.agentExecutor.fullname" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: agent-executor
{{- end }}

{{/*
Agent Executor labels with 4-space indent (avoids | in .yaml templates for linter)
*/}}
{{- define "ottoflow.agentExecutor.labels.n4" -}}
{{- include "ottoflow.agentExecutor.labels" . | nindent 4 -}}
{{- end }}

{{/*
Agent Executor selector labels with indent (avoids | in .yaml templates for linter)
*/}}
{{- define "ottoflow.agentExecutor.selectorLabels.n4" -}}
{{- include "ottoflow.agentExecutor.selectorLabels" . | nindent 4 -}}
{{- end }}
{{- define "ottoflow.agentExecutor.selectorLabels.n6" -}}
{{- include "ottoflow.agentExecutor.selectorLabels" . | nindent 6 -}}
{{- end }}
{{- define "ottoflow.agentExecutor.selectorLabels.n8" -}}
{{- include "ottoflow.agentExecutor.selectorLabels" . | nindent 8 -}}
{{- end }}

{{/*
Selector labels with indent (avoids | in .yaml templates for linter)
*/}}
{{- define "ottoflow.selectorLabels.n6" -}}
{{- include "ottoflow.selectorLabels" . | nindent 6 -}}
{{- end }}
{{- define "ottoflow.selectorLabels.n8" -}}
{{- include "ottoflow.selectorLabels" . | nindent 8 -}}
{{- end }}

{{/*
Agent Executor ClusterRoleBinding manifest (full resource for linter-friendly include from yaml)
*/}}
{{- define "ottoflow.agentExecutor.clusterrolebinding.manifest" -}}
{{- if and .Values.agentExecutor.enabled .Values.rbac.create }}
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: {{ include "ottoflow.agentExecutor.fullname" . }}-rolebinding
  labels:
    {{- include "ottoflow.agentExecutor.labels.n4" . }}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: {{ include "ottoflow.agentExecutor.fullname" . }}-role
subjects:
  - kind: ServiceAccount
    name: {{ include "ottoflow.agentExecutor.serviceAccountName" . }}
    namespace: {{ include "ottoflow.namespace" . }}
{{- end }}
{{- end }}

{{/*
Agent Executor TLS secret name (kyverno/pkg format: serviceName.namespace.svc.tls-pair)
Used when internal cert controller is enabled (no cert-manager).
*/}}
{{- define "ottoflow.agentExecutor.tlsSecretName" -}}
{{- printf "%s.%s.svc.tls-pair" (include "ottoflow.agentExecutor.fullname" .) (include "ottoflow.namespace" .) }}
{{- end }}

{{/*
Agent Executor CA secret name (kyverno/pkg format: serviceName.namespace.svc.tls-ca)
Mounted in the controller to verify the agent-executor's self-signed TLS cert.
*/}}
{{- define "ottoflow.agentExecutor.caSecretName" -}}
{{- printf "%s.%s.svc.tls-ca" (include "ottoflow.agentExecutor.fullname" .) (include "ottoflow.namespace" .) }}
{{- end }}

{{/*
Agent Executor service account name
*/}}
{{- define "ottoflow.agentExecutor.serviceAccountName" -}}
{{- if .Values.agentExecutor.serviceAccount.create }}
{{- default (include "ottoflow.agentExecutor.fullname" .) .Values.agentExecutor.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.agentExecutor.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Agent Executor image
*/}}
{{- define "ottoflow.agentExecutorImage" -}}
{{- if .Values.agentExecutor.image.fullOverride }}
{{- .Values.agentExecutor.image.fullOverride }}
{{- else }}
{{- $registry := .Values.global.imageRegistry }}
{{- if .Values.agentExecutor.image.registry }}
{{- $registry = .Values.agentExecutor.image.registry }}
{{- end }}
{{- if $registry }}
{{- printf "%s/%s:%s" $registry .Values.agentExecutor.image.repository (.Values.agentExecutor.image.tag | default (printf "v%s" .Chart.AppVersion)) }}
{{- else }}
{{- printf "%s:%s" .Values.agentExecutor.image.repository (.Values.agentExecutor.image.tag | default (printf "v%s" .Chart.AppVersion)) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create the name of the shared ClusterRole the serve-a2a ServiceAccount is bound to
*/}}
{{- define "ottoflow.serveA2AClusterRole" -}}
{{- .Values.serveA2A.clusterRole | default (printf "%s-serve-a2a" (include "ottoflow.fullname" .)) -}}
{{- end -}}

{{/*
serve-a2a image
*/}}
{{- define "ottoflow.serveA2AImage" -}}
{{- if .Values.serveA2A.image.fullOverride }}
{{- .Values.serveA2A.image.fullOverride }}
{{- else }}
{{- $registry := .Values.global.imageRegistry }}
{{- if .Values.serveA2A.image.registry }}
{{- $registry = .Values.serveA2A.image.registry }}
{{- end }}
{{- if $registry }}
{{- printf "%s/%s:%s" $registry .Values.serveA2A.image.repository (.Values.serveA2A.image.tag | default (printf "v%s" .Chart.AppVersion)) }}
{{- else }}
{{- printf "%s:%s" .Values.serveA2A.image.repository (.Values.serveA2A.image.tag | default (printf "v%s" .Chart.AppVersion)) }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Workflow Runner image
*/}}
{{- define "ottoflow.workflowRunnerImage" -}}
{{- if .Values.workflowRunner.image.fullOverride }}
{{- .Values.workflowRunner.image.fullOverride }}
{{- else }}
{{- $registry := .Values.global.imageRegistry }}
{{- if .Values.workflowRunner.image.registry }}
{{- $registry = .Values.workflowRunner.image.registry }}
{{- end }}
{{- if $registry }}
{{- printf "%s/%s:%s" $registry .Values.workflowRunner.image.repository (.Values.workflowRunner.image.tag | default (printf "v%s" .Chart.AppVersion)) }}
{{- else }}
{{- printf "%s:%s" .Values.workflowRunner.image.repository (.Values.workflowRunner.image.tag | default (printf "v%s" .Chart.AppVersion)) }}
{{- end }}
{{- end }}
{{- end }}
