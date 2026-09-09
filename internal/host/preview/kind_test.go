package preview

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestExpoPreviewKindDoesNotDependOnWebSupport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, deps string
	}{
		{"native only", `"expo":"56"`},
		{"universal", `"expo":"56","react-dom":"19","react-native-web":"0.21"`},
		{"missing DOM", `"expo":"56","react-native-web":"0.21"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"dependencies":{`+tc.deps+`}}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "app.json"), []byte(`{"expo":{}}`), 0600); err != nil {
				t.Fatal(err)
			}
			manager := New(Options{})
			defer manager.Shutdown()
			status, err := manager.Status(context.Background(), "kind-test", dir)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			var response map[string]any
			if err := json.Unmarshal(encoded, &response); err != nil {
				t.Fatal(err)
			}
			if response["kind"] != string(KindExpo) {
				t.Fatalf("kind = %v, want %v: %s", response["kind"], KindExpo, encoded)
			}
		})
	}
}
