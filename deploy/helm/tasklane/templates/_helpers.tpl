{{- define "tasklane.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "tasklane.fullname" -}}
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

{{- define "tasklane.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "tasklane.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "tasklane.selectorLabels" -}}
app.kubernetes.io/name: {{ include "tasklane.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "tasklane.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "tasklane.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "tasklane.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{/* the address people use: base_url, or the first ingress host */}}
{{- define "tasklane.baseURL" -}}
{{- if .Values.config.base_url -}}
{{- .Values.config.base_url -}}
{{- else if and .Values.ingress.enabled .Values.ingress.hosts -}}
{{- $h := index .Values.ingress.hosts 0 -}}
{{- if .Values.ingress.tls -}}https://{{ $h.host }}{{- else -}}http://{{ $h.host }}{{- end -}}
{{- else -}}
{{- fail "set config.base_url (the address people open Tasklane at), or enable the ingress with a host" -}}
{{- end -}}
{{- end -}}

{{/* guards: say what is wrong at install time, not at the first failed start */}}
{{- define "tasklane.validate" -}}
{{- if and (not .Values.database.existingSecret) (not .Values.database.host) -}}
{{- fail "set database.existingSecret (a Secret with the connection URL) or database.host" -}}
{{- end -}}
{{- if and (not .Values.database.existingSecret) .Values.database.host (not .Values.database.passwordSecret.name) -}}
{{- fail "database.host needs database.passwordSecret.name: the password is read from a Secret" -}}
{{- end -}}
{{- if and (eq .Values.blobs.backend "s3") (not .Values.blobs.s3.bucket) -}}
{{- fail "blobs.backend is s3: set blobs.s3.bucket" -}}
{{- end -}}
{{- if and (gt (int .Values.replicaCount) 1) (eq .Values.blobs.backend "fs") -}}
{{- if or (not .Values.blobs.persistence.enabled) (and (eq .Values.blobs.persistence.accessMode "ReadWriteOnce") (not .Values.blobs.persistence.existingClaim)) -}}
{{- fail "more than one replica needs attachments in S3 (blobs.backend=s3) or on a ReadWriteMany volume (blobs.persistence.accessMode=ReadWriteMany, or an existingClaim)" -}}
{{- end -}}
{{- end -}}
{{- if and (le (int .Values.terminationGracePeriodSeconds) 10) .Values.lifecycle.drainDelay -}}
{{- fail "terminationGracePeriodSeconds must be longer than the drain delay plus the longest request (at least 30)" -}}
{{- end -}}
{{- end -}}

{{/* the environment every Tasklane container shares */}}
{{- define "tasklane.env" -}}
{{- if .Values.database.existingSecret }}
- name: TASKLANE_DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.existingSecret }}
      key: {{ .Values.database.existingSecretKey }}
{{- else }}
- name: TASKLANE_DATABASE_URL
  value: {{ printf "postgres://%s@%s:%v/%s?sslmode=%s" .Values.database.user .Values.database.host .Values.database.port .Values.database.name .Values.database.sslmode | quote }}
- name: PGPASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ .Values.database.passwordSecret.name }}
      key: {{ .Values.database.passwordSecret.key }}
{{- end }}
{{- if .Values.secretKey.existingSecret }}
- name: TASKLANE_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.secretKey.existingSecret }}
      key: {{ .Values.secretKey.key }}
{{- end }}
{{- if .Values.smtp.passwordSecret.name }}
- name: TASKLANE_SMTP_PASS
  valueFrom:
    secretKeyRef:
      name: {{ .Values.smtp.passwordSecret.name }}
      key: {{ .Values.smtp.passwordSecret.key }}
{{- end }}
{{- if and (eq .Values.blobs.backend "s3") .Values.blobs.s3.existingSecret }}
- name: TASKLANE_S3_ACCESS_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.blobs.s3.existingSecret }}
      key: {{ .Values.blobs.s3.accessKeyKey }}
- name: TASKLANE_S3_SECRET_KEY
  valueFrom:
    secretKeyRef:
      name: {{ .Values.blobs.s3.existingSecret }}
      key: {{ .Values.blobs.s3.secretKeyKey }}
{{- end }}
{{- with .Values.extraEnv }}
{{ toYaml . }}
{{- end }}
{{- end -}}
