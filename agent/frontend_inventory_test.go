package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFrontendInventoryReadsSavedMaterialsWithoutNetworkOrExecution(t *testing.T) {
	var requests atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", 500)
	}))
	defer trap.Close()
	dir := t.TempDir()
	html := `<script src="assets/main.js"></script><script src="/app/assets/main.js"></script><script src="https://cdn.fixture.invalid/chunk.js"></script>`
	js := "const baseURL='/api/v1';\nfetch('/api/v1/users');\n" + `const encoded="\x2fapi\x2fencoded"; import('./chunk.js'); const tpl='/api/users/${id}';` + "\n//# sourceMappingURL=main.js.map\n" +
		`const apiKey="YOUR_API_KEY_HERE"; const clientSecret="opaque-7b39b28fdaf2"; require('fs').writeFileSync('executed','bad');`
	for name, content := range map[string]string{"index.html": html, "main.js": js} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	materials := []frontendMaterial{{"index.html", trap.URL + "/app/index.html"}, {"index.html", trap.URL + "/app/index.html"}, {"main.js", trap.URL + "/app/main.js"}}
	result, err := inspectFrontendMaterials(t.Context(), dir, materials)
	if err != nil || result.Files != 2 || result.Truncated {
		t.Fatalf("inventory=%+v err=%v", result, err)
	}
	if requests.Load() != 0 {
		t.Fatal("offline inventory made a request")
	}
	if _, err := os.Stat(filepath.Join(dir, "executed")); !os.IsNotExist(err) {
		t.Fatal("material was executed")
	}
	want := map[string]bool{"/api/v1/users": false, "/api/encoded": false}
	var external, sourceMap, template, secret, dedup bool
	hash := sha256.Sum256([]byte(js))
	for _, item := range result.Candidates {
		if _, ok := want[item.Reference]; ok {
			want[item.Reference] = true
			if item.Evidence[0].SHA256 != hex.EncodeToString(hash[:]) {
				t.Fatalf("wrong source hash: %+v", item)
			}
			if item.Reference == "/api/v1/users" && item.Evidence[0].Line != 2 {
				t.Fatalf("wrong source line: %+v", item)
			}
		}
		if item.URL == "https://cdn.fixture.invalid/chunk.js" {
			external = !item.SameOrigin
		}
		if item.URL == trap.URL+"/app/assets/main.js" {
			dedup = item.Occurrences == 2 && len(item.Evidence) == 2
		}
		if item.Kind == "sourcemap" && item.URL == trap.URL+"/app/main.js.map" {
			sourceMap = true
		}
		if item.Template {
			template = true
		}
		if item.Kind == "sensitive-key" {
			if item.Reference == "apiKey" {
				t.Fatal("placeholder treated as sensitive candidate")
			}
			secret = item.Reference == "clientSecret" && item.ValueSHA256 != ""
		}
	}
	for ref, found := range want {
		if !found {
			t.Errorf("missing literal %q: %+v", ref, result.Candidates)
		}
	}
	if !external || !sourceMap || !template || !secret || !dedup {
		t.Fatalf("missing classification: external=%t map=%t template=%t sensitive=%t dedup=%t", external, sourceMap, template, secret, dedup)
	}
	encoded, err := json.Marshal(result)
	if err != nil || strings.Contains(string(encoded), "opaque-7b39b28fdaf2") {
		t.Fatalf("sensitive value disclosed: %s err=%v", encoded, err)
	}
	current, err := os.ReadFile(filepath.Join(dir, "main.js"))
	if err != nil || string(current) != js {
		t.Fatal("original material changed")
	}
}

func TestFrontendInventoryBoundsEncodingAndSourcemaps(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "large.js"), []byte(strings.Repeat("x", maxFrontendFileBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bad.js"), append([]byte{0xff}, []byte(`fetch('/api/still-visible')`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceMap := `{"version":3,"sources":["original.js"],"sourcesContent":["fetch('/api/from-map')"]}`
	if err := os.WriteFile(filepath.Join(dir, "saved.map"), []byte(sourceMap), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.map"), []byte(`{"sourcesContent":`), 0o600); err != nil {
		t.Fatal(err)
	}
	materials := []frontendMaterial{{"large.js", "https://fixture.invalid/large.js"}, {"bad.js", "https://fixture.invalid/bad.js"}, {"saved.map", "https://fixture.invalid/saved.map"}, {"broken.map", "https://fixture.invalid/broken.map"}}
	result, err := inspectFrontendMaterials(t.Context(), dir, materials)
	if err != nil || !result.Truncated || result.Files != 3 || len(result.Issues) != 3 {
		t.Fatalf("inventory=%+v err=%v", result, err)
	}
	var encoded, embedded bool
	for _, item := range result.Candidates {
		if item.Reference == "/api/still-visible" {
			encoded = true
		}
		if item.Reference == "/api/from-map" && strings.Contains(item.Evidence[0].Path, "#sourcesContent[0]") {
			embedded = true
		}
	}
	if !encoded || !embedded {
		t.Fatalf("encoding/sourcemap candidates missing: %+v", result)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := inspectFrontendMaterials(ctx, dir, materials); err == nil {
		t.Fatal("cancelled parse succeeded")
	}
	if _, err := inspectFrontendMaterials(t.Context(), dir, make([]frontendMaterial, maxFrontendFiles+1)); err == nil {
		t.Fatal("accepted unbounded resources")
	}
	var batch []frontendMaterial
	for i := 0; i < 9; i++ {
		name := fmt.Sprintf("part-%d.js", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(strings.Repeat(" ", 1<<20)), 0o600); err != nil {
			t.Fatal(err)
		}
		batch = append(batch, frontendMaterial{name, "https://fixture.invalid/" + name})
	}
	limited, err := inspectFrontendMaterials(t.Context(), dir, batch)
	if err != nil || !limited.Truncated || limited.BytesRead != maxFrontendTotalBytes || limited.Files != 8 {
		t.Fatalf("total content budget not applied: %+v err=%v", limited, err)
	}
}

func TestFrontendInventoryRejectsOutsidePathsAndLinkedMaterials(t *testing.T) {
	parent := t.TempDir()
	dir, outside := filepath.Join(parent, "run"), filepath.Join(parent, "outside")
	for _, name := range []string{dir, outside} {
		if err := os.Mkdir(name, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "private.js"), []byte(`fetch('/api/private')`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alias")
	if err := os.Symlink(outside, link); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", "$ErrorActionPreference='Stop'; New-Item -ItemType Junction -Path $env:GONDAR_FRONTEND_TEST_LINK -Target $env:GONDAR_FRONTEND_TEST_TARGET | Out-Null")
		cmd.Env = append(os.Environ(), "GONDAR_FRONTEND_TEST_LINK="+link, "GONDAR_FRONTEND_TEST_TARGET="+outside)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("create real test junction: %v %s", err, output)
		}
	}
	result, err := inspectFrontendMaterials(t.Context(), dir, []frontendMaterial{
		{"../outside/private.js", "https://fixture.invalid/private.js"},
		{filepath.Join(outside, "private.js"), "https://fixture.invalid/private.js"},
		{filepath.Join("alias", "private.js"), "https://fixture.invalid/private.js"},
		{"missing.js", "javascript:fetch('/api/private')"},
	})
	if err != nil || result.Files != 0 || len(result.Candidates) != 0 || len(result.Issues) != 4 {
		t.Fatalf("outside material became visible: %+v err=%v", result, err)
	}
}

func TestFrontendInventoryToolOutputBudgetAndExistingToolBindings(t *testing.T) {
	dir := t.TempDir()
	var input strings.Builder
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&input, "fetch('/api/%03d/%s');\n", i, strings.Repeat("a", 200))
	}
	if err := os.WriteFile(filepath.Join(dir, "bounded.js"), []byte(input.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	ts := &ToolSet{runDir: dir}
	tool := ts.frontendInventoryTool()
	args, _ := json.Marshal(map[string]any{"materials": []frontendMaterial{{"bounded.js", "https://fixture.invalid/" + strings.Repeat("s", 1900)}}})
	result, err := tool.Call(context.Background(), args, nil)
	if err != nil || result.IsError {
		t.Fatalf("tool failed: %+v err=%v", result, err)
	}
	out := toolResultJSON(t, result)
	encoded, err := json.Marshal(out)
	if err != nil || len(encoded) > maxFrontendOutput || out["truncated"] != true {
		t.Fatalf("output budget failed: len=%d err=%v", len(encoded), err)
	}
	if !tool.IsReadOnly(args) || !tool.IsConcurrencySafe(args) {
		t.Fatal("offline tool lost read-only contract")
	}
	roles := map[string]bool{}
	for _, seed := range BuiltinToolSeeds() {
		if seed.Key == "frontend_inventory" {
			for _, role := range seed.Agents {
				roles[role] = true
			}
		}
	}
	if !roles["worker"] || !roles["planner"] || !roles["mainagent"] {
		t.Fatalf("tool is not on existing role bindings: %v", roles)
	}
}
