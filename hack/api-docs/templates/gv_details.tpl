{{- /*
Copyright 2026 The Shadewright Authors.
SPDX-License-Identifier: Apache-2.0
*/ -}}
{{- define "gvDetails" -}}
{{- $gv := . -}}
API group and version: `{{ $gv.GroupVersionString }}`

## Resource types

| Kind | Scope | Short names | Summary |
| --- | --- | --- | --- |
{{- range $gv.SortedKinds }}
{{- $t := $gv.TypeForKind . }}
{{- $res := index $t.Markers "kubebuilder:resource" }}
{{- $scope := "Namespaced" }}{{ $short := list }}
{{- range $res }}{{ if .Scope }}{{ $scope = .Scope }}{{ end }}{{ $short = .ShortName }}{{ end }}
| {{ markdownRenderTypeLink $t }} | {{ $scope }} | {{ range $i, $s := $short }}{{ if $i }}, {{ end }}`{{ $s }}`{{ end }} | {{ template "summary" $t.Doc }} |
{{- end }}
{{ range $gv.SortedKinds }}
{{ template "type" (dict "type" ($gv.TypeForKind .) "level" "##") }}
{{- end }}

## Types

Types used by the resources above, in alphabetical order.
{{ range $gv.SortedTypes }}
{{- if not .GVK }}
{{ template "type" (dict "type" . "level" "###") }}
{{- end }}
{{- end }}
{{- end -}}

{{- define "summary" -}}
{{- $first := splitList ". " (. | replace "\n" " ") | first -}}
{{- $first | trimSuffix "." | replace "|" "\\|" | replace "<" "&lt;" }}.
{{- end -}}
