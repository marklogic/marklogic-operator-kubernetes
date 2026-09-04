package k8sutil

import "testing"

func TestNormalizeYAMLIndentationPreservesNestedOutputProcessors(t *testing.T) {
	t.Parallel()

	input := `- name: gelf
  match: kube.marklogic.logs.error
  processors:
    logs:
      - name: modify
        match: "*"
        add:
          - Tenant why`

	want := `    - name: gelf
      match: kube.marklogic.logs.error
      processors:
        logs:
          - name: modify
            match: "*"
            add:
              - Tenant why`

	got := normalizeYAMLIndentation(input, 4, 6)
	if got != want {
		t.Fatalf("normalized YAML mismatch\nwant:\n%s\n\ngot:\n%s", want, got)
	}
}
