# Sync Helm Chart Templates

Verify and update Helm chart templates to match the source manifests in config/mcp-system.

## Steps

### 1. Sync CRDs (automated)

Run the existing make targets:
```bash
make generate-all
make check
```

### 2. Compare source manifests to chart templates

Read and compare these file pairs:

| Source (config/mcp-system/) | Chart Template (charts/mcp-gateway/templates/) |
|----------------------------|------------------------------------------------|
| deployment-broker.yaml     | deployment-broker.yaml                         |
| deployment-controller.yaml | deployment-controller.yaml                     |
| rbac.yaml                  | rbac.yaml                                      |
| broker-service.yaml        | service.yaml                                   |

Files intentionally NOT synced (kustomize-only or optional):
- namespace.yaml (Helm uses .Release.Namespace)
- kustomization.yaml (kustomize metadata)
- *-patch.yaml files (kustomize overlays)
- redis-*.yaml (optional component, not in base chart)
- httproute.yaml (cluster-specific)
- trusted-header-public-key.yaml (cluster-specific)

### 3. For each file pair

1. Read the source manifest from config/mcp-system/
2. Read the existing chart template from charts/mcp-gateway/templates/
3. Compare the structural elements (containers, ports, volumes, RBAC rules, etc.)
4. Identify differences that need updating in the chart template

### 4. Update chart templates preserving Helm patterns

When updating templates, preserve these Helm conventions:

**Metadata:**
```yaml
metadata:
  name: {{ include "mcp-gateway.fullname" . }}-suffix
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "mcp-gateway.labels" . | nindent 4 }}
```

**Images:**
```yaml
image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
imagePullPolicy: {{ .Values.image.pullPolicy }}
```

**Controller images:**
```yaml
image: "{{ .Values.imageController.repository }}:{{ .Values.imageController.tag | default .Chart.AppVersion }}"
imagePullPolicy: {{ .Values.imageController.pullPolicy }}
```

**Conditionals:**
```yaml
{{- if .Values.envoyFilter.create }}
...
{{- end }}
```

**Values references:**
- Hardcoded namespaces → `{{ .Release.Namespace }}`
- Image repos → `{{ .Values.image.repository }}`
- ConfigMap names → `{{ include "mcp-gateway.fullname" . }}-config`

### 5. Report findings

Summarize:
- Files that are in sync
- Files that need updates (with specific differences)
- Any new resources in config/mcp-system/ that might need chart templates

### 6. Apply updates

If updates are needed, edit the chart templates to incorporate changes while preserving Helm templating patterns.

### 7. Validate

Run helm lint to verify the chart is still valid:
```bash
helm lint charts/mcp-gateway
helm template test-release charts/mcp-gateway --debug
```
