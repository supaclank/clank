package acptools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"
)

// Read the bundled model catalog without credentials or a model prompt.
func TestIntegration_CodexCatalogOffersAstra(t *testing.T) {
	if os.Getenv(clankTestCodexACPEnv) == "" {
		t.Skip("set CLANK_TEST_CODEX_ACP=1 to verify the bundled Codex model catalog")
	}
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), installTimeout)
	defer cancel()
	paths, err := Ensure(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, paths.BunBin, paths.CodexBin, "app-server")
	cmd.Dir = t.TempDir()
	cmd.Env = append(os.Environ(), "CODEX_HOME="+t.TempDir(), "CODEX_API_KEY=", "OPENAI_API_KEY=")
	cmd.WaitDelay = 5 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = stdin.Close()
		if err := cmd.Wait(); err != nil {
			t.Errorf("Codex app-server exit: %v: %s", err, &stderr)
		}
	}()
	enc, dec := json.NewEncoder(stdin), json.NewDecoder(stdout)
	request := func(id int, method string, params any, result any) {
		t.Helper()
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
		for {
			var response struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if err := dec.Decode(&response); err != nil {
				t.Fatalf("read %s: %v", method, err)
			}
			if response.ID != id {
				continue
			}
			if len(response.Error) != 0 {
				t.Fatalf("%s: %s", method, response.Error)
			}
			if err := json.Unmarshal(response.Result, result); err != nil {
				t.Fatalf("decode %s: %v", method, err)
			}
			return
		}
	}
	var initialized json.RawMessage
	request(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "clank-test", "version": "1.0.0"}}, &initialized)
	if err := enc.Encode(map[string]string{"jsonrpc": "2.0", "method": "initialized"}); err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	request(2, "model/list", map[string]any{}, &catalog)
	const astraModel = "gpt-6-astra"
	for _, model := range catalog.Data {
		if model.ID == astraModel {
			return
		}
	}
	t.Fatalf("bundled Codex catalog does not advertise %s: %+v", astraModel, catalog.Data)
}
