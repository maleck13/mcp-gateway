#!/usr/bin/env bash
# sync-helm-rbac.sh - sync RBAC rules from generated config/rbac/role.yaml
# into the kustomize and helm chart ClusterRole definitions.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
YQ="${REPO_ROOT}/bin/yq"
GENERATED="${REPO_ROOT}/config/rbac/role.yaml"
KUSTOMIZE_RBAC="${REPO_ROOT}/config/mcp-gateway/components/controller/rbac-controller.yaml"
HELM_RBAC="${REPO_ROOT}/charts/mcp-gateway/templates/rbac.yaml"

if [ ! -f "$YQ" ]; then
  echo "yq not found at $YQ, run 'make yq' first"
  exit 1
fi

if [ ! -f "$GENERATED" ]; then
  echo "generated RBAC not found at $GENERATED, run 'make generate' first"
  exit 1
fi

rules_json=$("$YQ" -o=json '.rules' "$GENERATED")

# --- update kustomize rbac-controller.yaml ---
echo "updating $KUSTOMIZE_RBAC"
cat > "$KUSTOMIZE_RBAC" << 'HEADER'
# DO NOT EDIT - rules are synced from config/rbac/role.yaml by 'make sync-rbac'
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: mcp-controller
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: mcp-controller
HEADER

echo "rules:" >> "$KUSTOMIZE_RBAC"
echo "$rules_json" | "$YQ" -P '... style="single"' | sed 's/^/  /' >> "$KUSTOMIZE_RBAC"

cat >> "$KUSTOMIZE_RBAC" << 'FOOTER'
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: mcp-controller
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: mcp-controller
subjects:
  - kind: ServiceAccount
    name: mcp-controller
    namespace: placeholder
FOOTER

# --- update helm chart rbac.yaml ---
echo "updating $HELM_RBAC"
rules_file=$(mktemp)
echo "$rules_json" | "$YQ" -P '... style="double"' | sed 's/^/  /' > "$rules_file"

tmpfile=$(mktemp)
awk '
  /^rules:/ && !done {
    print "rules:"
    while ((getline line < "'"$rules_file"'") > 0) {
      print line
    }
    skip = 1
    done = 1
    next
  }
  skip && /^---/ {
    skip = 0
  }
  !skip { print }
' "$HELM_RBAC" > "$tmpfile"
mv "$tmpfile" "$HELM_RBAC"
rm -f "$rules_file"

# add sync comment to helm file if not present
if ! grep -q 'DO NOT EDIT.*sync-rbac' "$HELM_RBAC"; then
  tmpfile=$(mktemp)
  echo "{{- /* DO NOT EDIT rules - synced from config/rbac/role.yaml by 'make sync-rbac' */ -}}" > "$tmpfile"
  cat "$HELM_RBAC" >> "$tmpfile"
  mv "$tmpfile" "$HELM_RBAC"
fi

echo "RBAC rules synchronized"
