#!/bin/bash
set -e

# Values file path
VALUES_FILE="charts/marklogic-operator-kubernetes/values.yaml"
# CRD files to check
CRD_FILES=(
    "charts/marklogic-operator-kubernetes/templates/marklogicgroup-crd.yaml"
    "charts/marklogic-operator-kubernetes/templates/marklogiccluster-crd.yaml"
)

# Function to add crds.annotations to values.yaml if missing
add_values_entry() {
    if ! grep -q "crds:" "$VALUES_FILE"; then
        echo "Adding crds.annotations to $VALUES_FILE..."
        cat <<EOF >> "$VALUES_FILE"

crds:
  # -- Annotations to be added to all CRDs
  annotations: {}
EOF
    else
        echo "crds.annotations already present in $VALUES_FILE"
    fi
}

# Function to inject annotations into CRD template
inject_annotations() {
    local file=$1
    if [ -f "$file" ]; then
        echo "Processing $file..."
        
        # Check if already patched to avoid duplicates when this script runs multiple times
        if grep -q "helm.sh/resource-policy" "$file"; then
        echo "  Already patched."
             return
        fi

        # Use perl to replace only the first occurrence of "  annotations:"
        # This matches the metadata.annotations block indentation.
        # We replace "  annotations:" with itself + hardcoded annotation + template block
        perl -i -0777 -pe 's/(  annotations:)/$1\n    "helm.sh\/resource-policy": keep\n    {{- toYaml .Values.crds.annotations | nindent 4 }}/' "$file"
        
        echo "  Updated."
    else
        echo "Warning: $file not found"
    fi
}

echo "Starting post-processing..."
add_values_entry

for crd in "${CRD_FILES[@]}"; do
    inject_annotations "$crd"
done

echo "Done."
