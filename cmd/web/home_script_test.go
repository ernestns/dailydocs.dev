package main

import (
	"bytes"
	"os/exec"
	"regexp"
	"testing"
)

func TestHomeInputButtonTracksValue(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is needed to execute the browser-script behavior test")
	}
	page, err := templateFS.ReadFile("templates/home.html")
	if err != nil {
		t.Fatal(err)
	}
	scripts := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindSubmatch(page)
	if len(scripts) != 2 {
		t.Fatal("home page script not found")
	}
	command := exec.Command(node, "testdata/home-input.cjs")
	command.Stdin = bytes.NewReader(scripts[1])
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("home input behavior: %v\n%s", err, output)
	} else {
		t.Logf("%s", output)
	}
}
