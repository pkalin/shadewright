{{/*
Expand the name of the chart.
*/}}
{{- define "shadewright.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "shadewright.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Namespace for generated references.
Always uses the Helm release namespace.
*/}}
{{- define "shadewright.namespaceName" -}}
{{- .Release.Namespace }}
{{- end }}

{{/*
Resource name with proper truncation for Kubernetes 63-character limit.
Takes a dict with:
  - .suffix: Resource name suffix (e.g., "metrics", "webhook")
  - .context: Template context (root context with .Values, .Release, etc.)
Dynamically calculates safe truncation to ensure total name length <= 63 chars.
*/}}
{{- define "shadewright.resourceName" -}}
{{- $fullname := include "shadewright.fullname" .context }}
{{- $suffix := .suffix }}
{{- $maxLen := sub 62 (len $suffix) | int }}
{{- if gt (len $fullname) $maxLen }}
{{- printf "%s-%s" (trunc $maxLen $fullname | trimSuffix "-") $suffix | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" $fullname $suffix | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{/*
Selector labels of one component (the Deployment selector, the Service and NetworkPolicy selectors).
Takes a dict with .component ("executor" or "analyzer") and .context.
*/}}
{{- define "shadewright.selectorLabels" -}}
app.kubernetes.io/name: {{ include "shadewright.name" .context }}
app.kubernetes.io/component: {{ .component }}
control-plane: {{ .component }}
{{- end }}

{{/*
Common labels. Takes a dict with .component (may be empty) and .context.
*/}}
{{- define "shadewright.labels" -}}
app.kubernetes.io/name: {{ include "shadewright.name" .context }}
app.kubernetes.io/instance: {{ .context.Release.Name }}
app.kubernetes.io/managed-by: {{ .context.Release.Service }}
helm.sh/chart: {{ .context.Chart.Name }}-{{ .context.Chart.Version | replace "+" "_" }}
{{- if .component }}
app.kubernetes.io/component: {{ .component }}
{{- end }}
{{- end }}

{{/*
Image reference of a component by digest. The digest is required: a tag can move, a digest cannot.
Takes a dict with .component and .context.
*/}}
{{- define "shadewright.image" -}}
{{- $c := index .context.Values .component -}}
{{- $digest := required (printf "%s.image.digest is required (sha256:<64 hex digits>); images are pinned by digest" .component) $c.image.digest -}}
{{- printf "%s@%s" $c.image.repository $digest -}}
{{- end }}
