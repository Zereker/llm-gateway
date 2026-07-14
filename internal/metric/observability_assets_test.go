package metric

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var (
	metricConstantPattern = regexp.MustCompile(`(?m)^\s*([A-Za-z0-9_]+)\s*=\s*"(llm_gateway_[a-z0-9_]+)"`)
	metricEmissionPattern = regexp.MustCompile(`metric\.(?:Inc|Add|Observe|Set)\(metric\.([A-Za-z0-9_]+)`)
	metricLiteralPattern  = regexp.MustCompile(`llm_gateway_[a-z0-9_]+`)
)

func TestObservabilityAssetsReferenceEmittedMetrics(t *testing.T) {
	root := repositoryRoot(t)
	namesSource := readFile(t, filepath.Join(root, "internal", "metric", "names.go"))
	constantValues := make(map[string]string)
	for _, match := range metricConstantPattern.FindAllStringSubmatch(namesSource, -1) {
		constantValues[match[1]] = match[2]
	}

	emitted := make(map[string]bool)
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		for _, match := range metricEmissionPattern.FindAllStringSubmatch(readFile(t, path), -1) {
			if value := constantValues[match[1]]; value != "" {
				emitted[value] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal metrics: %v", err)
	}

	assets := []string{
		filepath.Join(root, "deploy", "observability", "alerts.yaml"),
		filepath.Join(root, "deploy", "observability", "dashboard.json"),
	}
	for _, asset := range assets {
		for _, name := range metricLiteralPattern.FindAllString(readFile(t, asset), -1) {
			base := histogramBase(name)
			if !emitted[base] {
				t.Errorf("%s references %q, but the gateway never emits %q", filepath.Base(asset), name, base)
			}
		}
	}
}

func TestGrafanaDashboardIsProvisionableJSON(t *testing.T) {
	root := repositoryRoot(t)
	var dashboard struct {
		UID    string            `json:"uid"`
		Title  string            `json:"title"`
		Panels []json.RawMessage `json:"panels"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(root, "deploy", "observability", "dashboard.json"))), &dashboard); err != nil {
		t.Fatalf("decode dashboard: %v", err)
	}
	if dashboard.UID == "" || dashboard.Title == "" || len(dashboard.Panels) < 8 {
		t.Fatalf("dashboard is incomplete: uid=%q title=%q panels=%d", dashboard.UID, dashboard.Title, len(dashboard.Panels))
	}
}

func histogramBase(name string) string {
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
