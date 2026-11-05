{{- /*
Copyright 2026 The Shadewright Authors.
SPDX-License-Identifier: Apache-2.0
*/ -}}
{{- define "type" -}}
{{- $type := .type -}}
{{- $level := .level -}}
{{- if markdownShouldRenderType $type -}}
{{ $level }} {{ $type.Name }}

{{ $type.Doc | replace "<" "&lt;" }}
{{ if or $type.GVK $type.IsAlias $type.Validation }}
{{- if $type.GVK }}
- API version: `{{ $type.GVK.Group }}/{{ $type.GVK.Version }}`
- Kind: `{{ $type.GVK.Kind }}`
{{- range (index $type.Markers "kubebuilder:resource") }}
- Scope: {{ .Scope | default "Namespaced" }}
{{- end }}
{{- end }}
{{- if $type.IsAlias }}
- Underlying type: {{ template "typeRef" $type.UnderlyingType }}
{{- end }}
{{- range $type.Validation }}
- {{ template "validation" . }}
{{- end }}
{{- end }}
{{- with (index $type.Markers "kubebuilder:validation:XValidation") }}

Validation rules:
{{ range . }}
- {{ .Message | replace "<" "&lt;" }}: `{{ .Rule }}`
{{- end }}
{{- end }}
{{- if $type.References }}

Used by: {{ range $i, $r := $type.SortedReferences }}{{ if $i }}, {{ end }}{{ markdownRenderTypeLink $r }}{{ end }}.
{{- end }}
{{- if $type.Members }}

| Field | Type | Required | Description |
| --- | --- | --- | --- |
{{- range $type.Members }}
{{ template "field" . }}
{{- end }}
{{- end }}
{{- if $type.EnumValues }}

| Value | Description |
| --- | --- |
{{- range $type.EnumValues }}
| `{{ .Name }}` | {{ template "cell" .Doc }} |
{{- end }}
{{- end }}
{{ end -}}
{{- end -}}

{{- /* crd-ref-docs links every Kubernetes type to the API reference, which has no Duration and files
Quantity under another anchor. */ -}}
{{- define "typeRef" -}}
{{- $s := markdownRenderType . | replace "#quantity-resource-api" "#quantity-resource-core" -}}
{{- regexReplaceAll "https://kubernetes.io/[^)]*#duration-v1-meta" $s "https://pkg.go.dev/k8s.io/apimachinery/pkg/apis/meta/v1#Duration" -}}
{{- end -}}

{{- define "cell" -}}
{{- . | trim | replace "\n" " " | replace "|" "\\|" | replace "<" "&lt;" -}}
{{- end -}}

{{- /* A code span for a table cell: a pipe would split the cell, and an escaped one would show its backslash
on the docs site, so text with a pipe becomes a <code> element with the pipe as an entity. A "[" is an entity too, or the Markdown renderer reads a pattern such as
[a-z]([-a-z]*) inside the element as a link. */ -}}
{{- define "tablecode" -}}
{{- if contains "|" . -}}
<code>{{ . | replace "&" "&amp;" | replace "<" "&lt;" | replace "|" "&#124;" | replace "[" "&#91;" }}</code>
{{- else -}}
`{{ . }}`
{{- end -}}
{{- end -}}

{{- define "validation" -}}
{{- $parts := splitn ": " 2 . -}}
{{- $key := $parts._0 -}}
{{- $val := $parts._1 -}}
{{- if eq $key "Enum" -}}
One of {{ range $i, $v := ($val | trimPrefix "[" | trimSuffix "]" | splitList " ") }}{{ if $i }}, {{ end }}`{{ $v }}`{{ end }}
{{- else if eq $key "Pattern" -}}
Pattern: {{ $val }}
{{- else if eq $key "MinLength" -}}
Minimum length: {{ $val }}
{{- else if eq $key "MaxLength" -}}
Maximum length: {{ $val }}
{{- else if eq $key "MinItems" -}}
Minimum items: {{ $val }}
{{- else if eq $key "MaxItems" -}}
Maximum items: {{ $val }}
{{- else if and (or (eq $key "Minimum") (eq $key "Maximum")) (contains "e+" $val) -}}
{{ $key }}: {{ $val | float64 | int64 }}
{{- else -}}
{{ $key }}: {{ $val }}
{{- end -}}
{{- end -}}
