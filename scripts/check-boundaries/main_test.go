package main

import (
	"strings"
	"testing"
)

func project(path string, dependencies ...string) pkg {
	p := pkg{ImportPath: module + path}
	for _, dependency := range dependencies {
		p.Imports = append(p.Imports, module+dependency)
	}
	return p
}

func TestDependencyClosures(t *testing.T) {
	for _, test := range []struct {
		name     string
		packages []pkg
		forbid   string
	}{
		{"public_contracts", []pkg{project("domain"), project("storage", "domain"), project("extension", "domain")}, ""},
		{"os_uses_client", []pkg{project("cmd/os", "internal/cli", "client"), project("client", "domain")}, ""},
		{"os_cannot_compose_runtime", []pkg{project("cmd/os", "internal/runtime")}, "internal/runtime"},
		{"direct_adapter", []pkg{project("internal/runtime", "extensions/github")}, "extensions/github"},
		{"transitive_adapter", []pkg{project("internal/runtime", "extension"), project("extension", "extensions/github")}, "extensions/github"},
		{"reference_runtime_leak", []pkg{project("conformance/reference", "internal/runtime")}, "internal/runtime"},
		{"domain_payload_leak", []pkg{project("domain", "extensions/slack")}, "extensions/slack"},
		{"executor_sqlite_leak", []pkg{project("executors/shell", "internal/storage/sqlite")}, "internal/storage/sqlite"},
		{"unknown_package_leak", []pkg{project("newpackage", "domain")}, "domain"},
		{"external_bridge", []pkg{project("internal/runtime"), {ImportPath: "external.test/helper", Imports: []string{module + "extensions/git"}}}, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name == "external_bridge" {
				test.packages[0].Imports = []string{"external.test/helper"}
				test.forbid = "extensions/git"
			}
			failures := violations(test.packages)
			if test.forbid == "" {
				if len(failures) > 0 {
					t.Fatal(failures)
				}
			} else if !strings.Contains(strings.Join(failures, "\n"), test.forbid) {
				t.Fatalf("weakened graph not rejected: %v", failures)
			}
		})
	}
}

func TestRuntimeTestVariantBoundaries(t *testing.T) {
	base := project("internal/runtime", "domain")
	variant := project("internal/runtime_test [" + module + "internal/runtime.test]")
	variant.ForTest = module + "internal/runtime"
	variant.Imports = []string{module + "internal/runtime", module + "conformance"}
	graph := []pkg{base, project("conformance", "domain"), project("domain"), variant}
	if failures := violations(graph); len(failures) > 0 {
		t.Fatal(failures)
	}
	for _, forbidden := range []string{"extensions/github", "internal/storage/sqlite", "conformance/reference"} {
		leaking := variant
		leaking.Imports = append(append([]string{}, variant.Imports...), module+forbidden)
		if failures := violations(append(graph[:len(graph)-1:len(graph)-1], leaking)); len(failures) == 0 {
			t.Fatalf("test-only %s leak passed", forbidden)
		}
	}
	// A test import may also be rewritten by Go's ImportMap to a package variant.
	helper := project("extension ["+module+"internal/runtime.test]", "extensions/slack")
	leaking := variant
	leaking.Imports = append(append([]string{}, variant.Imports...), module+"extension")
	leaking.ImportMap = map[string]string{module + "extension": helper.ImportPath}
	if failures := violations(append(graph[:len(graph)-1:len(graph)-1], leaking, helper)); len(failures) == 0 {
		t.Fatal("transitive test-variant adapter leak passed")
	}
	// Dependency tests are never part of a consumer's dependency closure.
	contractTests := project("conformance_test ["+module+"conformance.test]", "conformance/reference")
	contractTests.ForTest = module + "conformance"
	if failures := violations(append(graph, contractTests)); len(failures) > 0 {
		t.Fatal(failures)
	}
}
