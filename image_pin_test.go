package main

import (
	"strings"
	"testing"
)

func TestRuntimeImageIsImmutable(t *testing.T) {
	if image.Digest == "" || strings.Contains(image.Tag, "latest") {
		t.Fatalf("SQL Server image is not pinned: %+v", image)
	}
}
