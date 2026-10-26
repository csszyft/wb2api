package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestAppJSModelsCacheAndDefaultRealmReload(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	src, err := os.ReadFile("app.js")
	if err != nil {
		t.Fatal(err)
	}
	fn := jsFuncFull(string(src), "async function loadModels")
	if fn == "" {
		t.Fatal("loadModels is missing")
	}
	script := `
const assert = require('assert');
const nodes = {mdBody: {innerHTML: '', querySelectorAll: () => []}};
const paths = [];
` + fn + `
let mdRealm = 'global', mdLoadSeq = 0, mdUserSelectedRealm = true;
let mdCache = null, mdCacheRealm = null;
let mdFilter = {q: '', cap: '', effort: '', promo: '', sort: 'default'};
const $ = id => nodes[id];
const esc = String;
const api = async path => {
  if (path.startsWith('models')) paths.push(path);
  return {realm:'cn', models:[], realm_servable:{cn:true,global:false}};
};
const renderModelRealms = () => {};
(async () => {
  await loadModels();
  assert.equal(paths[0], 'models');
  assert.equal(mdCacheRealm, null);
  await loadModels(true);
  assert.equal(paths.length, 1, 'filters must not resend the request');
  await loadModels(false, true);
  assert.equal(paths[1], 'models?refresh=1');
})().catch(e => { console.error(e); process.exit(1); });
`
	fp := filepath.Join(t.TempDir(), "model-cache.cjs")
	if err := os.WriteFile(fp, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, fp).CombinedOutput(); err != nil {
		t.Fatalf("model cache/default realm regression: %v\n%s", err, out)
	}
}
