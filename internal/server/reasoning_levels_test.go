package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestEffortLevelsCacheResolution(t *testing.T) {
	c := LoadEffortLevelsCache("")
	if got := c.LevelsForModel("muse-spark-1.3-contributor-free"); !reflect.DeepEqual(got,
		[]string{"minimal", "low", "medium", "high", "xhigh"}) {
		t.Errorf("muse family: %v", got)
	}
	if got := c.LevelsForModel("muse-spark-1.2"); !reflect.DeepEqual(got,
		[]string{"minimal", "low", "medium", "high", "xhigh"}) {
		t.Errorf("muse prefix: %v", got)
	}
	if got := c.LevelsForModel("some-unknown-model"); !reflect.DeepEqual(got,
		[]string{"minimal", "low", "medium", "high", "xhigh"}) {
		t.Errorf("unknown default: %v", got)
	}
	// Returned slices are copies.
	got := c.LevelsForModel("muse-spark-1.3")
	got[0] = "MUTATED"
	if c.LevelsForModel("muse-spark-1.3")[0] != "minimal" {
		t.Error("LevelsForModel must return a copy")
	}
}

func TestEffortLevelsCacheLearnAndPersist(t *testing.T) {
	dir := t.TempDir()
	c := LoadEffortLevelsCache(dir)
	c.Learn("nemo-1", []string{"low", "medium", "high"})
	if got := c.LevelsForModel("nemo-1"); !reflect.DeepEqual(got, []string{"low", "medium", "high"}) {
		t.Fatalf("learned: %v", got)
	}
	// none/max in taught lists are filtered (listed-but-rejected upstream).
	c.Learn("nemo-2", []string{"none", "minimal", "low", "max"})
	if got := c.LevelsForModel("nemo-2"); !reflect.DeepEqual(got, []string{"minimal", "low"}) {
		t.Fatalf("filtered: %v", got)
	}
	// Garbage is ignored, never persisted.
	c.Learn("nemo-3", []string{"ok", "not a level!!"})
	if got := c.LevelsForModel("nemo-3"); !reflect.DeepEqual(got,
		[]string{"minimal", "low", "medium", "high", "xhigh"}) {
		t.Fatalf("garbage must not stick: %v", got)
	}
	// Reload from disk.
	c2 := LoadEffortLevelsCache(dir)
	if got := c2.LevelsForModel("nemo-1"); !reflect.DeepEqual(got, []string{"low", "medium", "high"}) {
		t.Errorf("persisted: %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "reasoning-levels.json")); err != nil {
		t.Errorf("cache file: %v", err)
	}
}

func TestEffortLevelsCacheConcurrentPersist(t *testing.T) {
	dir := t.TempDir()
	c := LoadEffortLevelsCache(dir)
	const models = 32
	var wg sync.WaitGroup
	for i := 0; i < models; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			model := fmt.Sprintf("model-%02d", i)
			c.Learn(model, []string{"low", "medium", "high"})
			c.DemoteTop(model)
		}(i)
	}
	wg.Wait()

	// Every concurrent writer must survive: an unserialized write could
	// truncate the file or let an older snapshot win.
	raw, err := os.ReadFile(filepath.Join(dir, "reasoning-levels.json"))
	if err != nil {
		t.Fatalf("read cache: %v", err)
	}
	var disk map[string][]string
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatalf("cache is not valid JSON after concurrent writes: %v\n%s", err, raw)
	}
	if len(disk) != models {
		t.Fatalf("cache holds %d models, want %d", len(disk), models)
	}
	for i := 0; i < models; i++ {
		model := fmt.Sprintf("model-%02d", i)
		if got := disk[model]; !reflect.DeepEqual(got, []string{"low", "medium"}) {
			t.Fatalf("%s = %v, want demoted [low medium]", model, got)
		}
	}
	// Atomic writes must not leave temp files behind.
	leftovers, err := filepath.Glob(filepath.Join(dir, ".reasoning-levels-*"))
	if err != nil {
		t.Fatalf("glob temp files: %v", err)
	}
	if len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}

func TestEffortLevelsCacheDemote(t *testing.T) {
	c := LoadEffortLevelsCache("")
	c.Learn("nemo-9", []string{"low", "medium", "high", "xhigh"})
	if got := c.DemoteTop("nemo-9"); !reflect.DeepEqual(got, []string{"low", "medium", "high"}) {
		t.Fatalf("demoted: %v", got)
	}
	if got := c.LevelsForModel("nemo-9"); !reflect.DeepEqual(got, []string{"low", "medium", "high"}) {
		t.Errorf("demote must stick: %v", got)
	}
	c.Learn("nemo-1", []string{"low"})
	if got := c.DemoteTop("nemo-1"); !reflect.DeepEqual(got, []string{"low"}) {
		t.Errorf("must never drop the last level: %v", got)
	}
	// Demoting a family-table model snapshots the demoted list as learned.
	if got := c.DemoteTop("muse-spark-1.3"); !reflect.DeepEqual(got,
		[]string{"minimal", "low", "medium", "high"}) {
		t.Errorf("family demote: %v", got)
	}
}

func TestEffortForRequest(t *testing.T) {
	srv := New(nil, nil)
	const target = "muse-spark-1.3-contributor-free"

	// Bridge (/v1/messages): the live Claude Code wire shape — bare adaptive
	// thinking block plus output_config.effort — must surface the clamped
	// level, exactly like the bridge builder sends upstream.
	bridge := []byte(`{"model":"m","thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"messages":[]}`)
	if got := srv.effortForRequest("/v1/messages", target, bridge); got != "xhigh" {
		t.Errorf("bridge output_config max must clamp to xhigh, got %q", got)
	}
	// Bridge thinking.effort wins over output_config when both are present.
	both := []byte(`{"model":"m","thinking":{"type":"enabled","effort":"high"},"output_config":{"effort":"max"},"messages":[]}`)
	if got := srv.effortForRequest("/v1/messages", target, both); got != "high" {
		t.Errorf("thinking.effort must win, got %q", got)
	}
	// Passthrough (/v1/responses): client level clamped into target scale.
	pass := []byte(`{"model":"m","reasoning":{"effort":"ultra"},"input":[]}`)
	if got := srv.effortForRequest("/v1/responses", target, pass); got != "xhigh" {
		t.Errorf("passthrough ultra must clamp to xhigh, got %q", got)
	}
	// Passthrough without an effort has no level to show.
	if got := srv.effortForRequest("/v1/responses", target, []byte(`{"model":"m","input":[]}`)); got != "" {
		t.Errorf("passthrough without effort must be empty, got %q", got)
	}
	// Non-effort paths and empty bodies show "—".
	if got := srv.effortForRequest("/v1/chat/completions", target, bridge); got != "" {
		t.Errorf("chat path must be empty, got %q", got)
	}
	if got := srv.effortForRequest("/v1/messages", target, nil); got != "" {
		t.Errorf("empty body must be empty, got %q", got)
	}
}

func TestClampPassthroughEffort(t *testing.T) {
	srv := New(nil, nil)
	in := []byte(`{"model":"m","reasoning":{"effort":"ultra"},"input":[]}`)
	out := srv.clampPassthroughEffort(in, "muse-spark-1.3-contributor-free", "m")
	if !strings.Contains(string(out), `"effort":"xhigh"`) {
		t.Errorf("ultra must clamp to xhigh: %s", out)
	}
	exact := []byte(`{"model":"m","reasoning":{"effort":"high"},"input":[]}`)
	if out := srv.clampPassthroughEffort(exact, "muse-spark-1.3-contributor-free", "m"); string(out) != string(exact) {
		t.Errorf("exact effort must pass through byte-identical: %s", out)
	}
	cased := []byte(`{"model":"m","reasoning":{"effort":"XHIGH"},"input":[]}`)
	if out := srv.clampPassthroughEffort(cased, "muse-spark-1.3-contributor-free", "m"); !strings.Contains(string(out), `"effort":"xhigh"`) {
		t.Errorf("case must normalize: %s", out)
	}
	absent := []byte(`{"model":"m","input":[]}`)
	if out := srv.clampPassthroughEffort(absent, "muse-spark-1.3-contributor-free", "m"); string(out) != string(absent) {
		t.Errorf("missing effort must pass through: %s", out)
	}
}
