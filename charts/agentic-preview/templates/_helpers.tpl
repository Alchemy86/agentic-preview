{{/*
Name helpers, in the usual Helm shape.
*/}}
{{- define "agentic-preview.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "agentic-preview.fullname" -}}
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

{{- define "agentic-preview.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
`app: agentic-preview` is carried alongside the standard set because the Service
in deploy/ selected on it and an existing installation's pods do.
*/}}
{{- define "agentic-preview.labels" -}}
helm.sh/chart: {{ include "agentic-preview.chart" . }}
{{ include "agentic-preview.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: agentic-preview
{{- end -}}

{{- define "agentic-preview.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agentic-preview.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app: {{ include "agentic-preview.name" . }}
{{- end -}}

{{- define "agentic-preview.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "agentic-preview.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "agentic-preview: serviceAccount.name is required when serviceAccount.create is false." .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
allowedNamespaces, validated.

This is the one value with no default, and the render stops here rather than
letting an install proceed without it. It is the safety boundary in BOTH
directions - the namespaces a preview may be intercepted and built in, and the
namespaces a preview may be forwarded to - and forwarding is a plain dial from
the pod that no Kubernetes permission is consulted for, so nothing but this
list bounds it. An empty list read as "every namespace" is the wrong failure.

The service itself refuses to start without ALLOWED_NAMESPACES. Failing here
instead turns that into an install-time error with a message, rather than a
CrashLoopBackOff somebody has to read logs to explain.

Returns the list, so callers can `range` it.
*/}}
{{- define "agentic-preview.allowedNamespaces" -}}
{{- $v := .Values.allowedNamespaces -}}
{{- if kindIs "string" $v -}}
{{- fail (printf "\n\nagentic-preview: allowedNamespaces must be a LIST of namespaces, but was given the string %q.\n\nOn the command line the braces are what make it a list:\n\n    --set 'allowedNamespaces={%s}'\n\nor in a values file:\n\n    allowedNamespaces:\n      - %s\n" $v $v $v) -}}
{{- end -}}
{{- $ns := compact (default (list) $v) -}}
{{- if not $ns -}}
{{- fail "\n\nagentic-preview: allowedNamespaces is required and has no default.\n\nIt is the safety boundary in both directions: the namespaces a preview may be\nintercepted in and built in, and the namespaces a preview may be forwarded to.\nForwarding is a plain dial from this pod that no Kubernetes permission is\nconsulted for, so this list is the only thing bounding it - which is why an\nempty list cannot be read as \"every namespace\" and the install stops here.\n\nEach entry gets its own attach Role and build Role, with a RoleBinding each, in\nthat namespace. Never a ClusterRole.\n\nSet it to the namespaces holding the workloads you want to preview:\n\n    --set 'allowedNamespaces={shop,warehouse}'\n\nor in a values file:\n\n    allowedNamespaces:\n      - shop\n      - warehouse\n" -}}
{{- end -}}
{{- toYaml $ns -}}
{{- end -}}

{{/*
The traffic-manager's gRPC address: trafficManager.address when set, otherwise
derived from trafficManager.namespace so that the address and the ConnectReview
Role's namespace cannot be set to disagree by accident.
*/}}
{{- define "agentic-preview.managerAddr" -}}
{{- if .Values.trafficManager.address -}}
{{- .Values.trafficManager.address -}}
{{- else -}}
{{- $ns := required "agentic-preview: set either trafficManager.namespace or trafficManager.address - the manager's gRPC address cannot be guessed, and a wrong one fails as an unhelpful dial timeout." .Values.trafficManager.namespace -}}
{{- printf "traffic-manager.%s.svc.cluster.local:8081" $ns -}}
{{- end -}}
{{- end -}}

{{/*
The image reference. A digest wins over a tag; an empty tag means the chart's
appVersion, which the release workflow stamps from the same git tag that built
and pushed the image.
*/}}
{{- define "agentic-preview.image" -}}
{{- $repo := required "agentic-preview: image.repository is required." .Values.image.repository -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" $repo .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{/*
The preview lifetime, as the service spells it.

YAML reads a bare `off` (and `no`, and `false`) as a boolean, so a values file
that says `lifetime: off` - which is exactly what the documentation tells an
adopter to write to disable expiry - would otherwise reach the container as
"false", which the service does not recognise and would silently fall back to
the 24h default from. Turning expiry off has to be the thing that happens when
somebody asks for it.
*/}}
{{- define "agentic-preview.previewLifetime" -}}
{{- $v := .Values.preview.lifetime -}}
{{- if kindIs "bool" $v -}}
{{- if $v -}}
{{- fail "\n\nagentic-preview: preview.lifetime was read as the boolean true, which is not a lifetime.\n\nYAML turns a bare on/yes/true into a boolean. Quote a duration instead:\n\n    preview:\n      lifetime: \"24h\"\n\nor, to disable expiry entirely:\n\n    preview:\n      lifetime: \"off\"\n" -}}
{{- else -}}
off
{{- end -}}
{{- else -}}
{{- $v -}}
{{- end -}}
{{- end -}}
