{{/* vim: set filetype=mustache: */}}
{{/*
Expand the name of the chart.
*/}}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
*/}}
{{- define "fullname" -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "jxcontroller.name" -}}
{{- $name := default "-controller" .Values.jxcontroller.nameOverride -}}
{{- printf "%s-%s" .Chart.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
