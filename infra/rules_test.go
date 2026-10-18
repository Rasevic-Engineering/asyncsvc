package main

import (
	"strings"
	"testing"
)

func TestFirestoreRules(t *testing.T) {
	r := firestoreRules(" Andrej@RasevicEngineering.com , ops@example.com,")
	if !strings.Contains(r, `in ["andrej@rasevicengineering.com","ops@example.com"]`) {
		t.Fatalf("admin list not filled in:\n%s", r)
	}
	if strings.Contains(r, "__ADMIN_EMAILS__") {
		t.Fatal("placeholder left in rules")
	}
	if r := firestoreRules(""); !strings.Contains(r, "in []") {
		t.Fatal("empty admin list should give an empty rules list")
	}
}
