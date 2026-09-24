{{/*
The release's name for its objects: the release name, with the chart's name
appended unless the release name already contains it. Kept to 63 characters.
*/}}
{{- define "hearsay.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "hearsay.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "hearsay.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{ include "hearsay.selectorLabels" . }}
app.kubernetes.io/version: {{ .Values.image.tag | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
The four services, in the order their objects render: the value key, the
subcommand (which is also the component label and the object suffix), and the
port the binary listens on by default.
*/}}
{{- define "hearsay.services" -}}
- {key: connectors, name: connectors, port: 8081}
- {key: distiller, name: distiller, port: 8082}
- {key: assertWorker, name: assert-worker, port: 8083}
- {key: api, name: api, port: 8080}
{{- end -}}

{{- define "hearsay.image" -}}
{{- $tag := required "image.tag is required: a published version, such as v0.10.0 (there is no latest)" .Values.image.tag -}}
{{- if .Values.image.digest -}}
{{- printf "%s:%s@%s" .Values.image.repository $tag .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
{{- end -}}

{{/* The database URL, from the existing Secret. */}}
{{- define "hearsay.databaseEnv" -}}
- name: HEARSAY_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ required "database.existingSecret is required: the name of an existing Secret holding the Postgres URL" .Values.database.existingSecret | quote }}
      key: {{ required "database.key is required" .Values.database.key | quote }}
{{- end -}}

{{/*
The config form in use: hearsayYaml, files or volume. Exactly one is set, or
rendering fails.
*/}}
{{- define "hearsay.configForm" -}}
{{- $forms := list -}}
{{- if .Values.config.hearsayYaml -}}{{- $forms = append $forms "hearsayYaml" -}}{{- end -}}
{{- if .Values.config.files -}}{{- $forms = append $forms "files" -}}{{- end -}}
{{- if .Values.config.volume -}}{{- $forms = append $forms "volume" -}}{{- end -}}
{{- if ne (len $forms) 1 -}}
{{- fail (printf "set exactly one of config.hearsayYaml, config.files and config.volume; %s set" (ternary "none is" (printf "%s are" (join " and " $forms)) (eq (len $forms) 0))) -}}
{{- end -}}
{{- first $forms -}}
{{- end -}}

{{/*
A ConfigMap key for a path in the directory form. Keys cannot hold a slash,
and the directory form is one level deep, so dir/file.yaml becomes
dir.file.yaml; the ConfigMap template refuses two paths that meet.
*/}}
{{- define "hearsay.configKey" -}}
{{- replace "/" "." . -}}
{{- end -}}

{{- define "hearsay.configMapName" -}}
{{- printf "%s-config" (include "hearsay.fullname" .) -}}
{{- end -}}

{{/* The config volume's source. */}}
{{- define "hearsay.configVolume" -}}
{{- $form := include "hearsay.configForm" . -}}
{{- if eq $form "volume" -}}
{{- toYaml .Values.config.volume -}}
{{- else -}}
configMap:
  name: {{ include "hearsay.configMapName" . }}
  {{- if eq $form "files" }}
  items:
    {{- range $path, $_ := .Values.config.files }}
    - key: {{ include "hearsay.configKey" $path | quote }}
      path: {{ $path | quote }}
    {{- end }}
  {{- end }}
{{- end -}}
{{- end -}}

{{/*
A service's environment: HEARSAY_CONFIG, the database URL, then the shared and
the service's own env and secretEnv, the service's winning on a name both set.
Takes (dict "root" $ "svc" <the service's values>).
*/}}
{{- define "hearsay.env" -}}
{{- $root := .root -}}
{{- $env := merge (deepCopy (.svc.env | default dict)) ($root.Values.env | default dict) -}}
{{- $secretEnv := merge (deepCopy (.svc.secretEnv | default dict)) ($root.Values.secretEnv | default dict) -}}
- name: HEARSAY_CONFIG
  value: /etc/hearsay
{{ include "hearsay.databaseEnv" $root }}
{{- range $name, $value := $env }}
{{- if hasKey $secretEnv $name }}
{{- fail (printf "%s is in both env and secretEnv; set it in one" $name) }}
{{- end }}
{{- include "hearsay.reserved" $name }}
- name: {{ $name | quote }}
  value: {{ $value | toString | quote }}
{{- end }}
{{- range $name, $ref := $secretEnv }}
{{- include "hearsay.reserved" $name }}
- name: {{ $name | quote }}
  valueFrom:
    secretKeyRef:
      name: {{ required (printf "secretEnv.%s.secret is required" $name) $ref.secret | quote }}
      key: {{ required (printf "secretEnv.%s.key is required" $name) $ref.key | quote }}
{{- end }}
{{- end -}}

{{/*
Variables the chart sets itself: the config path and the database URL, and the
listen addresses the ports and probes assume.
*/}}
{{- define "hearsay.reserved" -}}
{{- if has . (list "HEARSAY_CONFIG" "HEARSAY_DATABASE_URL" "HEARSAY_LISTEN" "HEARSAY_API_LISTEN" "HEARSAY_DISTILLER_LISTEN" "HEARSAY_ASSERT_WORKER_LISTEN") -}}
{{- fail (printf "%s is set by the chart: the configuration comes from config, the database URL from database.existingSecret, and the probes and Services assume the default listen addresses" .) -}}
{{- end -}}
{{- end -}}
