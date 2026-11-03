{{- /*
Copyright 2026 The Shadewright Authors.
SPDX-License-Identifier: Apache-2.0
*/ -}}
{{- define "field" -}}
{{- $f := . -}}
{{- $required := or (index $f.Markers "required") (index $f.Markers "kubebuilder:validation:Required") -}}
{{- /* A pipe inside a code span splits the cell on GitHub, while an escaped pipe in a code span shows its
backslash on the docs site (Python-Markdown). Code spans that contain a pipe are therefore written as
<code> elements with the pipe as an entity (see "tablecode"), which both render intact. */ -}}
{{- /* Rules that come from the field's type are listed with their expression under that type; rules set on
the field itself show their expression here. */ -}}
{{- $t := $f.Type -}}
{{- if and $t.UnderlyingType (or (eq (toString $t.Kind) "SLICE") (eq (toString $t.Kind) "POINTER")) }}{{ $t = $t.UnderlyingType }}{{ end -}}
{{- $typeRules := index $t.Markers "kubebuilder:validation:XValidation" | default list -}}
| `{{ $f.Name }}` | {{ template "typeRef" $f.Type }} | {{ if $required }}Yes{{ else }}No{{ end }} | {{ if eq $f.Name "metadata" }}Standard object metadata.{{ else }}{{ template "cell" $f.Doc }}{{ end }}
{{- if $f.Default }}<br>Default: {{ template "tablecode" ($f.Default | replace "{  }" "{}") }}{{ end }}
{{- range $f.Validation }}
{{- if not (or (hasPrefix "Required" .) (hasPrefix "Optional" .)) }}<br>{{ if hasPrefix "Pattern: " . }}Pattern: {{ template "tablecode" (trimPrefix "Pattern: " . | trimPrefix "`" | trimSuffix "`") }}{{ else }}{{ template "validation" . }}{{ end }}{{ end }}
{{- end }}
{{- range (index $f.Markers "listType") }}<br>List type: {{ . }}{{ end }}
{{- range (index $f.Markers "listMapKey") }}<br>List key: `{{ . }}`{{ end }}
{{- range (index $f.Markers "kubebuilder:validation:XValidation") }}<br>Rule: {{ template "cell" .Message }}{{ if not (has . $typeRules) }} ({{ template "tablecode" .Rule }}){{ end }}{{ end }} |
{{- end -}}
