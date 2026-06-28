package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAdminDisabledReturnsNotFound(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "")

	handler := newTestHandler(nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin", nil))

	if response.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", response.Code)
	}
}

func TestAdminLoginSetsSessionCookie(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-token")

	handler := newTestHandler(nil)
	form := url.Values{"token": {"test-admin-token"}}
	request := httptest.NewRequest(http.MethodPost, "/admin/login", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/admin/submissions" {
		t.Fatalf("expected redirect to admin submissions, got %q", location)
	}
	cookie := findCookie(response.Result().Cookies(), adminSessionCookie)
	if cookie == nil {
		t.Fatal("expected admin session cookie")
	}
	if !cookie.HttpOnly {
		t.Fatal("expected admin session cookie to be HttpOnly")
	}
	if !cookie.Secure {
		t.Fatal("expected admin session cookie to be Secure")
	}
}

func TestAdminRequiresAuth(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-token")

	handler := newTestHandler(nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/admin/submissions", nil))

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", response.Code)
	}
	if location := response.Header().Get("Location"); location != "/admin/login" {
		t.Fatalf("expected redirect to login, got %q", location)
	}
}

func TestAdminSubmissionsListsSubmissions(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-token")

	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()
	insertWebSubmission(t, ctx, conn, "https://doc.rust-lang.org/book/", "Rust")

	handler := newTestHandler(conn)
	cookie := adminLoginCookie(t, handler, "test-admin-token")
	request := httptest.NewRequest(http.MethodGet, "/admin/submissions", nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, "Rust") {
		t.Fatalf("expected Rust submission:\n%s", body)
	}
	if !strings.Contains(body, "/admin/submissions/1") {
		t.Fatalf("expected submission detail link:\n%s", body)
	}
	if !strings.Contains(body, `data-href="/admin/submissions/1"`) {
		t.Fatalf("expected clickable submission row:\n%s", body)
	}
	if !strings.Contains(body, `tabindex="0"`) {
		t.Fatalf("expected keyboard-focusable submission row:\n%s", body)
	}
}

func TestAdminCanProcessAndActivateSubmission(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-token")

	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()

	server := adminDocsServer()
	defer server.Close()
	submissionID := insertWebSubmission(t, ctx, conn, server.URL+"/docs", "Rust")

	handler := newTestHandler(conn)
	cookie := adminLoginCookie(t, handler, "test-admin-token")
	csrf := adminCSRFToken(t, handler, cookie, submissionID)

	processForm := url.Values{"csrf": {csrf}}
	processRequest := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/submissions/%d/process", submissionID), strings.NewReader(processForm.Encode()))
	processRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	processRequest.AddCookie(cookie)
	processResponse := httptest.NewRecorder()
	handler.ServeHTTP(processResponse, processRequest)

	if processResponse.Code != http.StatusSeeOther {
		t.Fatalf("expected process redirect, got %d: %s", processResponse.Code, processResponse.Body.String())
	}

	var status string
	if err := conn.QueryRowContext(ctx, "SELECT status FROM documentation_submissions WHERE id = ?", submissionID).Scan(&status); err != nil {
		t.Fatalf("read processed status: %v", err)
	}
	if status != "candidates_ready" {
		t.Fatalf("expected candidates_ready, got %q", status)
	}

	csrf = adminCSRFToken(t, handler, cookie, submissionID)
	activateForm := url.Values{"csrf": {csrf}}
	activateRequest := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/submissions/%d/activate", submissionID), strings.NewReader(activateForm.Encode()))
	activateRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	activateRequest.AddCookie(cookie)
	activateResponse := httptest.NewRecorder()
	handler.ServeHTTP(activateResponse, activateRequest)

	if activateResponse.Code != http.StatusSeeOther {
		t.Fatalf("expected activate redirect, got %d: %s", activateResponse.Code, activateResponse.Body.String())
	}

	var topicCount int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM topics WHERE slug = 'rust' AND status = 'active'").Scan(&topicCount); err != nil {
		t.Fatalf("count rust topic: %v", err)
	}
	if topicCount != 1 {
		t.Fatalf("expected active rust topic, got %d", topicCount)
	}
}

func TestAdminCanCreateTopicSourceFromSubmission(t *testing.T) {
	t.Setenv("ADMIN_TOKEN", "test-admin-token")

	ctx := context.Background()
	conn := openWebTestDB(t, ctx)
	defer conn.Close()

	submissionID := insertWebSubmission(t, ctx, conn, "https://doc.rust-lang.org/stable/book", "Rust")

	handler := newTestHandler(conn)
	cookie := adminLoginCookie(t, handler, "test-admin-token")
	csrf := adminCSRFToken(t, handler, cookie, submissionID)

	form := url.Values{
		"csrf":       {csrf},
		"topic_slug": {"rust"},
		"topic_name": {"Rust"},
	}
	request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/admin/submissions/%d/create-source", submissionID), strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected create source redirect, got %d: %s", response.Code, response.Body.String())
	}

	var sourceCount int
	if err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM topic_sources ts
		JOIN topics t ON t.id = ts.topic_id
		WHERE t.slug = 'rust'
			AND ts.normalized_url = 'https://doc.rust-lang.org/stable/book'
			AND ts.created_from_submission_id = ?
	`, submissionID).Scan(&sourceCount); err != nil {
		t.Fatalf("count topic sources: %v", err)
	}
	if sourceCount != 1 {
		t.Fatalf("expected one rust source, got %d", sourceCount)
	}

	detailRequest := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/admin/submissions/%d", submissionID), nil)
	detailRequest.AddCookie(cookie)
	detailResponse := httptest.NewRecorder()
	handler.ServeHTTP(detailResponse, detailRequest)

	if detailResponse.Code != http.StatusOK {
		t.Fatalf("expected detail 200, got %d: %s", detailResponse.Code, detailResponse.Body.String())
	}
	body := detailResponse.Body.String()
	if !strings.Contains(body, "Process Source") {
		t.Fatalf("expected process source action:\n%s", body)
	}
	if !strings.Contains(body, "https://doc.rust-lang.org/stable/book") {
		t.Fatalf("expected source URL in detail:\n%s", body)
	}
}
