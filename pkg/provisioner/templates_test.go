package provisioner_test

import (
	"encoding/json"
	"testing"

	"github.com/supaclank/clank/internal/host"
	"github.com/supaclank/clank/pkg/provisioner"
)

func TestTemplateBuildTargetSurvivesProvisioning(t *testing.T) {
	t.Parallel()
	var configured []provisioner.Template
	if err := json.Unmarshal([]byte(`[{"display_name":"Website","clone_url":"https://example.com/web.git","build_target":"web"}]`), &configured); err != nil {
		t.Fatal(err)
	}
	var forwarded []map[string]any
	if err := json.Unmarshal([]byte(provisioner.TemplatesEnvValue(configured)), &forwarded); err != nil {
		t.Fatal(err)
	}
	if forwarded[0]["build_target"] != "web" {
		t.Fatalf("provisioning lost build_target: %v", forwarded)
	}
	var hosted []host.Template
	if err := json.Unmarshal([]byte(provisioner.TemplatesEnvValue(configured)), &hosted); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(hosted)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &forwarded); err != nil {
		t.Fatal(err)
	}
	if forwarded[0]["build_target"] != "web" {
		t.Fatalf("host lost build_target: %s", encoded)
	}
}
