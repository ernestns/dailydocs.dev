package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"testing"
)

func TestHomeInputAndKeyboardBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is needed to execute the browser-script behavior test")
	}
	conn := openWebTestDB(t, context.Background())
	defer conn.Close()
	response := httptest.NewRecorder()
	newTestHandler(conn).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	scripts := regexp.MustCompile(`(?s)<script>(.*?)</script>`).FindSubmatch(response.Body.Bytes())
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
