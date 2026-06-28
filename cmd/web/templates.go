package main

import (
	"embed"
	"html/template"
)

//go:embed templates/*.html
var templateFS embed.FS

var (
	homeTemplate                  = mustTemplate("templates/home.html")
	topicsTemplate                = mustTemplate("templates/topics.html")
	submissionsTemplate           = mustTemplate("templates/submissions.html", "templates/submit_lock.html")
	readingTemplate               = mustTemplate("templates/reading.html")
	adminLoginTemplate            = mustTemplate("templates/admin_login.html")
	adminSubmissionsTemplate      = mustTemplate("templates/admin_submissions.html")
	adminSourcesTemplate          = mustTemplate("templates/admin_sources.html", "templates/submit_lock.html")
	adminRunDetailTemplate        = mustTemplate("templates/admin_run_detail.html")
	adminSourceDetailTemplate     = mustTemplate("templates/admin_source_detail.html", "templates/submit_lock.html")
	adminSubmissionDetailTemplate = mustTemplate("templates/admin_submission_detail.html", "templates/submit_lock.html")
)

func mustTemplate(path string, extra ...string) *template.Template {
	files := append([]string{path}, extra...)
	return template.Must(template.ParseFS(templateFS, files...))
}
