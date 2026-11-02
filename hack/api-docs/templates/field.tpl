{{- /*
Copyright 2026 The Shadewright Authors.
SPDX-License-Identifier: Apache-2.0
*/ -}}
{{- define "field" -}}
{{- $f := . -}}
{{- $required := or (index $f.Markers "required") (index $f.Markers "kubebuilder:validation:Required") -}}
{{- /* Pipes inside code spans are left as they are: Python-Markdown, which the docs site uses, does not
split table cells inside backticks, and an escaped pipe would show its backslash. */ -}}
{{- /* Rules that come from the field's type are listed with their expression under that type; rules set on
the field itself show their expression here. */ -}}
{{- $t := $f.Type -}}
{{- if and $t.UnderlyingType (or (eq (toString $t.Kind) "SLICE") (eq (toString $t.Kind) "POINTER")) }}{{ $t = $t.UnderlyingType }}{{ end -}}
{{- $typeRules := index $t.Markers "kubebuilder:validation:XValidation" | default list -}}
| `{{ $f.Name }}` | {{ template "typeRef" $f.Type }} | {{ if $required }}Yes{{ else }}No{{ end }} | {{ if eq $f.Name "metadata" }}Standard object metadata.{{ else }}{{ template "cell" $f.Doc }}{{ end }}
{{- if $f.Default }}<br>Default: `{{ $f.Default | replace "{  }" "{}" }}`{{ end }}
{{- range $f.Validation }}
{{- if not (or (hasPrefix "Required" .) (hasPrefix "Optional" .)) }}<br>{{ template "validation" . }}{{ end }}
{{- end }}
{{- range (index $f.Markers "listType") }}<br>List type: {{ . }}{{ end }}
{{- range (index $f.Markers "listMapKey") }}<br>List key: `{{ . }}`{{ end }}
{{- range (index $f.Markers "kubebuilder:validation:XValidation") }}<br>Rule: {{ template "cell" .Message }}{{ if not (has . $typeRules) }} (`{{ .Rule }}`){{ end }}{{ end }} |
{{- end -}}
