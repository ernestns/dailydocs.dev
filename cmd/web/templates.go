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
	submissionsTemplate           = mustTemplate("templates/submissions.html")
	readingTemplate               = mustTemplate("templates/reading.html")
	adminLoginTemplate            = mustTemplate("templates/admin_login.html")
	adminSubmissionsTemplate      = mustTemplate("templates/admin_submissions.html")
	adminSubmissionDetailTemplate = mustTemplate("templates/admin_submission_detail.html")
)

func mustTemplate(path string) *template.Template {
	return template.Must(template.ParseFS(templateFS, path))
}
