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

{{- define "hearsay.databaseEnv" -}}
- name: HEARSAY_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ required "database.existingSecret is required: the name of an existing Secret holding the Postgres URL" .Values.database.existingSecret | quote }}
      key: {{ required "database.key is required" .Values.database.key | quote }}
{{- end -}}

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

{{- define "hearsay.configKey" -}}
{{- replace "/" "." . -}}
{{- end -}}

{{- define "hearsay.configMapName" -}}
{{- printf "%s-config" (include "hearsay.fullname" .) -}}
{{- end -}}

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

{{- define "hearsay.reserved" -}}
{{- if has . (list "HEARSAY_CONFIG" "HEARSAY_DATABASE_URL" "HEARSAY_LISTEN" "HEARSAY_API_LISTEN" "HEARSAY_DISTILLER_LISTEN" "HEARSAY_ASSERT_WORKER_LISTEN") -}}
{{- fail (printf "%s is set by the chart: the configuration comes from config, the database URL from database.existingSecret, and the probes and Services assume the default listen addresses" .) -}}
{{- end -}}
{{- end -}}
