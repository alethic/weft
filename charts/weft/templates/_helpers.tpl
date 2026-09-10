{{/*
Chart name, overridable.
*/}}
{{- define "weft.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Fully qualified name. Release names that already contain the chart name are not
doubled up, which is what keeps "helm install weft ./charts/weft" from producing
"weft-weft".
*/}}
{{- define "weft.fullname" -}}
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

{{- define "weft.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "weft.labels" -}}
helm.sh/chart: {{ include "weft.chart" . }}
{{ include "weft.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: weft
{{- end -}}

{{/*
Selector labels are immutable on a Deployment, so they deliberately exclude
version and chart.
*/}}
{{- define "weft.selectorLabels" -}}
app.kubernetes.io/name: {{ include "weft.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "weft.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "weft.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{/*
Container image. A digest, when given, wins over the tag: a tag can be moved,
and a controller that holds impersonation rights is a bad thing to let drift.
*/}}
{{- define "weft.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}

{{/*
The groups Weft impersonates, as the comma-separated form the flag expects.
*/}}
{{- define "weft.impersonateGroups" -}}
{{- join "," .Values.controller.impersonateGroups -}}
{{- end -}}

{{/*
Whether any configured group carries a placeholder, and so cannot be enumerated
in a resourceNames restriction.
*/}}
{{- define "weft.hasTemplatedGroup" -}}
{{- $templated := false -}}
{{- range .Values.controller.impersonateGroups -}}
{{- if contains "{" . -}}
{{- $templated = true -}}
{{- end -}}
{{- end -}}
{{- if $templated }}true{{ end -}}
{{- end -}}

{{/*
Guard the one genuinely dangerous configuration.
*/}}
{{- define "weft.validate" -}}
{{- if and (include "weft.hasTemplatedGroup" .) (not .Values.rbac.allowUnrestrictedGroupImpersonation) -}}
{{- fail (printf "controller.impersonateGroups contains a templated entry (%s), whose full set cannot be listed in a resourceNames restriction. Granting impersonate on groups without one lets anyone who compromises this controller impersonate system:masters. Set rbac.allowUnrestrictedGroupImpersonation=true to accept that, or remove the templated group." (include "weft.impersonateGroups" .)) -}}
{{- end -}}
{{- if not (has .Values.rbac.impersonation.scope (list "cluster" "namespaced")) -}}
{{- fail (printf "rbac.impersonation.scope must be \"cluster\" or \"namespaced\", got %q" .Values.rbac.impersonation.scope) -}}
{{- end -}}
{{- if and (eq .Values.rbac.impersonation.scope "namespaced") (not .Values.rbac.impersonation.namespaces) -}}
{{- fail "rbac.impersonation.scope is \"namespaced\" but rbac.impersonation.namespaces is empty, so Weft could not impersonate anywhere and every Weave would fail with a permission error." -}}
{{- end -}}
{{- if and (gt (int .Values.replicaCount) 1) (not .Values.controller.leaderElection.enabled) -}}
{{- fail "replicaCount is greater than 1 with controller.leaderElection.enabled=false, which would have several replicas reconciling the same Weave and applying over each other." -}}
{{- end -}}
{{- if and .Values.podDisruptionBudget.enabled .Values.podDisruptionBudget.minAvailable .Values.podDisruptionBudget.maxUnavailable -}}
{{- fail "set only one of podDisruptionBudget.minAvailable and podDisruptionBudget.maxUnavailable" -}}
{{- end -}}
{{- end -}}

{{/*
Controller arguments, assembled from values in one place so the Deployment stays
readable.

Every numeric value is passed through int or int64 on the way out. YAML decodes
a large integer as a float64, and Helm renders that as 2e+07, which the flag
parser rejects and the container crash-loops on.
*/}}
{{- define "weft.args" -}}
{{- $c := .Values.controller -}}
- --health-probe-bind-address=:{{ int .Values.health.port }}
{{- if .Values.metrics.enabled }}
- --metrics-bind-address=:{{ int .Values.metrics.port }}
{{- else }}
- --metrics-bind-address=0
{{- end }}
{{- if $c.leaderElection.enabled }}
- --leader-elect
- --leader-election-id={{ $c.leaderElection.id }}
{{- end }}
{{- if $c.watchNamespace }}
- --namespace={{ $c.watchNamespace }}
{{- end }}
- --impersonate-groups={{ include "weft.impersonateGroups" . }}
- --prune-delay={{ $c.pruneDelay }}
- --prune-threshold={{ int $c.pruneThreshold }}
- --source-finalizer-timeout={{ $c.sourceFinalizerTimeout }}
- --teardown-timeout={{ $c.teardownTimeout }}
- --poll-interval={{ $c.pollInterval }}
- --backstop-interval={{ $c.backstopInterval }}
- --degraded-retry={{ $c.degradedRetry }}
- --max-steps={{ int64 $c.evaluator.maxSteps }}
- --max-resources={{ int $c.evaluator.maxResources }}
- --max-values={{ int $c.evaluator.maxValues }}
- --program-cache-size={{ int $c.evaluator.programCacheSize }}
- --watch-resync={{ $c.watches.resync }}
- --watch-lifetime={{ $c.watches.lifetime }}
- --zap-log-level={{ $c.log.level }}
- --zap-encoder={{ $c.log.encoder }}
- --zap-stacktrace-level={{ $c.log.stacktraceLevel }}
{{- with $c.extraArgs }}
{{ toYaml . }}
{{- end }}
{{- end -}}
