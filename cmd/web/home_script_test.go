package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	importWebTopic(t, context.Background(), conn, "go", "Go")
	var searchResponses []json.RawMessage
	for _, path := range []string{"/topics/search?q=not-known", "/topics/search?q=Go"} {
		response := httptest.NewRecorder()
		newTestHandler(conn).ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatal(response.Code, response.Body.String())
		}
		searchResponses = append(searchResponses, response.Body.Bytes())
	}
	encoded, err := json.Marshal(searchResponses)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(node, "testdata/home-input.cjs", string(encoded))
	command.Stdin = bytes.NewReader(scripts[1])
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("home input behavior: %v\n%s", err, output)
	} else {
		t.Logf("%s", output)
	}
}
