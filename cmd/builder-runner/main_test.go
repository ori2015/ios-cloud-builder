package main

import (
	"os"
	"testing"
)

func TestResolveProjectRejectsRegistryArgumentAndClearsEnvironment(t *testing.T) {
	t.Setenv("PROJECT_REGISTRY", `{"sensitive":"registry"}`)

	if err := resolveProject([]string{"--registry", "plaintext"}); err == nil {
		t.Fatal("resolve-project accepted the protected registry through argv")
	}
	if value := os.Getenv("PROJECT_REGISTRY"); value != "" {
		t.Fatal("resolve-project left the protected registry in the environment")
	}
}
