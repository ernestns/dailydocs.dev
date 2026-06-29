package main

import (
	"embed"
	"html/template"
)

//go:embed templates/*.html
var templateFS embed.FS

var (
	homeTemplate             = mustTemplate("templates/home.html")
	topicsTemplate           = mustTemplate("templates/topics.html")
	topicEvaluationsTemplate = mustTemplate("templates/topic_evaluations.html")
	readingTemplate          = mustTemplate("templates/reading.html")
	queuedTopicTemplate      = mustTemplate("templates/queued_topic.html")
)

func mustTemplate(path string, extra ...string) *template.Template {
	files := append([]string{path}, extra...)
	return template.Must(template.ParseFS(templateFS, files...))
}
