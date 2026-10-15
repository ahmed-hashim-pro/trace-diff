package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func synthDir(t *testing.T, scenario string) string {
	t.Helper()
	dir := t.TempDir()
	if code, _, e := runCLI(t, "synth", "--scenario", scenario, "--seed", "1", "--out", dir); code != 0 {
		t.Fatalf("synth: %d %s", code, e)
	}
	return dir
}

func TestCheckExitCodes(t *testing.T) {
	quiet := synthDir(t, "none")
	code, out, _ := runCLI(t, "check", filepath.Join(quiet, "before.jsonl"), filepath.Join(quiet, "after.jsonl"))
	if code != 0 || !strings.Contains(out, "no structural changes found") {
		t.Fatalf("no change: exit %d\n%s", code, out)
	}

	dir := synthDir(t, "n-plus-one")
	before, after := filepath.Join(dir, "before.jsonl"), filepath.Join(dir, "after.jsonl")
	svg := filepath.Join(dir, "w.svg")
	code, out, _ = runCLI(t, "check", before, after, "--svg", svg)
	if code != 1 {
		t.Fatalf("n-plus-one: exit %d, want 1\n%s", code, out)
	}
	for _, want := range []string{"FAIL  n-plus-one", "WHERE id = ?", "replaces SELECT id, sku, qty, price_cents FROM order_items WHERE order_id = ?", "trace after"} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if b, err := os.ReadFile(svg); err != nil || !bytes.HasPrefix(b, []byte("<svg")) {
		t.Errorf("svg: %v", err)
	}
	if code, _, _ := runCLI(t, "diff", before, after); code != 0 {
		t.Errorf("diff exits %d, want 0", code)
	}

	code, out, _ = runCLI(t, "check", "--json", before, after)
	var r struct {
		Findings []struct{ Kind, Severity string }
	}
	if code != 1 || json.Unmarshal([]byte(out), &r) != nil || len(r.Findings) == 0 || r.Findings[0].Kind != "n-plus-one" {
		t.Errorf("json: exit %d, %+v", code, r)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"nope"},
		{"check", "one-file"},
		{"check", "missing-a", "missing-b"},
		{"check", "a", "b", "--fail-on", "maybe"},
		{"show", "missing", "abc"},
	} {
		if code, _, _ := runCLI(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestSummaryAndShow(t *testing.T) {
	dir := synthDir(t, "none")
	f := filepath.Join(dir, "before.jsonl")
	code, out, _ := runCLI(t, "summary", f)
	if code != 0 || !strings.Contains(out, "gateway GET /orders/{id}") || !strings.Contains(out, "calls per request") {
		t.Fatalf("summary: %d\n%s", code, out)
	}
	// The first trace id in the file.
	b, _ := os.ReadFile(f)
	i := bytes.Index(b, []byte(`"traceId":"`))
	id := string(b[i+11 : i+43])
	code, out, _ = runCLI(t, "show", f, id[:12])
	if code != 0 || !strings.Contains(out, "trace "+id) {
		t.Fatalf("show: %d\n%s", code, out)
	}
}
