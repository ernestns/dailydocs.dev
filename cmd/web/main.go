package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/reading"
	"github.com/ernestns/daily-docs/internal/seed"
)

var topicPathPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type app struct {
	db  *sql.DB
	now func() time.Time
}

func main() {
	ctx := context.Background()

	if len(os.Args) > 1 {
		if err := runCommand(ctx, os.Args[1:]); err != nil {
			log.Printf("command failed: %v", err)
			os.Exit(1)
		}
		return
	}

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}

	dbPath := os.Getenv("DB_PATH")
	conn, err := db.Open(ctx, dbPath)
	if err != nil {
		log.Printf("database startup failed: %v", err)
		os.Exit(1)
	}
	defer conn.Close()

	app := app{
		db:  conn,
		now: func() time.Time { return time.Now().UTC() },
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/topics/search", app.searchTopicsHandler)
	mux.HandleFunc("/read", app.generateReadingHandler)
	mux.HandleFunc("/", app.routeHandler)

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errs := make(chan error, 1)
	go func() {
		log.Printf("starting DailyDocs web server addr=%s", addr)
		errs <- server.ListenAndServe()
	}()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errs:
		if err != nil && err != http.ErrServerClosed {
			log.Printf("server failed: %v", err)
			os.Exit(1)
		}
	case sig := <-shutdown:
		log.Printf("shutdown signal received signal=%s", sig.String())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("server shutdown failed: %v", err)
			os.Exit(1)
		}
	}
}

func (a app) routeHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if r.URL.Path == "/" {
		a.homeHandler(w, r)
		return
	}

	topic, date, ok := parseReadingPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	if date == "" {
		date = a.now().UTC().Format("2006-01-02")
	}

	dailyReading, err := reading.GetDailyReading(r.Context(), a.db, topic, date)
	if err != nil {
		switch {
		case errors.Is(err, reading.ErrTopicNotFound):
			http.NotFound(w, r)
		case errors.Is(err, reading.ErrNoActivePages):
			http.Error(w, "topic has no active pages", http.StatusNotFound)
		case errors.Is(err, reading.ErrInvalidDate):
			http.Error(w, "invalid date", http.StatusBadRequest)
		default:
			log.Printf("reading page failed: %v", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
		}
		return
	}

	renderTemplate(w, readingTemplate, dailyReading)
}

func (a app) homeHandler(w http.ResponseWriter, r *http.Request) {
	topics, err := listTopics(r.Context(), a.db, "")
	if err != nil {
		log.Printf("list topics failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	renderTemplate(w, homeTemplate, struct {
		Topics []topicOption
	}{Topics: topics})
}

func (a app) generateReadingHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	topic := strings.TrimSpace(r.URL.Query().Get("topic"))
	if !topicPathPattern.MatchString(topic) {
		http.Error(w, "invalid topic", http.StatusBadRequest)
		return
	}

	http.Redirect(w, r, "/"+topic, http.StatusSeeOther)
}

func (a app) searchTopicsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	topics, err := listTopics(r.Context(), a.db, r.URL.Query().Get("q"))
	if err != nil {
		log.Printf("search topics failed: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(topics); err != nil {
		log.Printf("encode topic search failed: %v", err)
	}
}

func parseReadingPath(path string) (topic string, date string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 1 && len(parts) != 2 {
		return "", "", false
	}
	if !topicPathPattern.MatchString(parts[0]) {
		return "", "", false
	}
	if len(parts) == 2 {
		if _, err := time.Parse("2006-01-02", parts[1]); err != nil {
			return parts[0], parts[1], true
		}
		return parts[0], parts[1], true
	}
	return parts[0], "", true
}

type topicOption struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
}

func listTopics(ctx context.Context, conn *sql.DB, query string) ([]topicOption, error) {
	query = strings.TrimSpace(strings.ToLower(query))

	sqlQuery := `
		SELECT slug, name
		FROM topics
		WHERE status = 'active'
	`
	args := []any{}
	if query != "" {
		sqlQuery += " AND (slug LIKE ? OR lower(name) LIKE ?)"
		like := "%" + query + "%"
		args = append(args, like, like)
	}
	sqlQuery += " ORDER BY name ASC LIMIT 10"

	rows, err := conn.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		return nil, fmt.Errorf("query topics: %w", err)
	}
	defer rows.Close()

	var topics []topicOption
	for rows.Next() {
		var topic topicOption
		if err := rows.Scan(&topic.Slug, &topic.Name); err != nil {
			return nil, fmt.Errorf("scan topic: %w", err)
		}
		topics = append(topics, topic)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate topics: %w", err)
	}
	return topics, nil
}

func renderTemplate(w http.ResponseWriter, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		log.Printf("render template failed: %v", err)
	}
}

var homeTemplate = template.Must(template.New("home").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>DailyDocs</title>
  <style>
    body {
      margin: 0;
      font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      color: #1f2933;
      background: #f7f8fa;
    }
    main {
      min-height: 100vh;
      display: grid;
      place-items: center;
      padding: 2rem;
      box-sizing: border-box;
    }
    section {
      width: min(42rem, 100%);
    }
    h1 {
      margin: 0 0 0.75rem;
      font-size: clamp(2.5rem, 8vw, 5rem);
      line-height: 1;
    }
    p {
      margin: 0 0 1.5rem;
      max-width: 34rem;
      color: #52606d;
      font-size: 1.125rem;
      line-height: 1.6;
    }
    form {
      display: flex;
      gap: 0.75rem;
      align-items: center;
      max-width: 32rem;
    }
    input {
      flex: 1;
      min-width: 0;
      padding: 0.75rem 0.875rem;
      border: 1px solid #cbd2d9;
      border-radius: 6px;
      font: inherit;
      background: #ffffff;
    }
    button {
      padding: 0.75rem 1rem;
      border: 0;
      border-radius: 6px;
      font: inherit;
      color: #ffffff;
      background: #1f2933;
      cursor: pointer;
    }
    ul {
      margin: 1.25rem 0 0;
      padding: 0;
      list-style: none;
      display: flex;
      flex-wrap: wrap;
      gap: 0.5rem;
    }
    a {
      color: #1f2933;
    }
  </style>
</head>
<body>
  <main>
    <section>
      <h1>DailyDocs</h1>
      <p>One documentation link per topic per day.</p>
      <form method="get" action="/read">
        <input name="topic" list="topics" autocomplete="off" placeholder="sqlite" aria-label="Topic">
        <datalist id="topics">
          {{range .Topics}}<option value="{{.Slug}}">{{.Name}}</option>{{end}}
        </datalist>
        <button type="submit">View Reading</button>
      </form>
      {{if .Topics}}
      <ul>
        {{range .Topics}}<li><a href="/{{.Slug}}">{{.Name}}</a></li>{{end}}
      </ul>
      {{end}}
    </section>
  </main>
</body>
</html>
`))

var readingTemplate = template.Must(template.New("reading").Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>{{.TopicName}} - DailyDocs</title>
  <style>
    body {
      margin: 0;
      font-family: system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      color: #1f2933;
      background: #f7f8fa;
    }
    main {
      min-height: 100vh;
      display: grid;
      place-items: center;
      padding: 2rem;
      box-sizing: border-box;
    }
    article {
      width: min(42rem, 100%);
    }
    .date {
      margin: 0 0 0.5rem;
      color: #52606d;
      font-size: 0.95rem;
    }
    h1 {
      margin: 0 0 0.5rem;
      font-size: clamp(2.25rem, 7vw, 4.5rem);
      line-height: 1;
    }
    h2 {
      margin: 0 0 1rem;
      font-size: clamp(1.5rem, 4vw, 2.25rem);
      line-height: 1.15;
    }
    p {
      margin: 0 0 1.5rem;
      color: #52606d;
      font-size: 1.05rem;
      line-height: 1.6;
    }
    .badge {
      display: inline-block;
      margin-left: 0.5rem;
      font-size: 0.85rem;
      color: #1f2933;
    }
    a.button {
      display: inline-block;
      padding: 0.75rem 1rem;
      border-radius: 6px;
      color: #ffffff;
      background: #1f2933;
      text-decoration: none;
    }
    nav {
      margin-top: 1.5rem;
    }
    nav a {
      color: #52606d;
    }
  </style>
</head>
<body>
  <main>
    <article>
      <p class="date">{{.Date}}</p>
      <h1>{{.TopicName}}</h1>
      <h2>{{.Title}}</h2>
      <p>
        {{if .Source}}{{.Source}}{{else}}Documentation{{end}}
        {{if .Official}}<span class="badge">Official</span>{{end}}
        {{if .EstimatedMinutes}}<br>{{.EstimatedMinutes}} min{{end}}
      </p>
      <a class="button" href="{{.URL}}">Read</a>
      <nav><a href="/">All topics</a></nav>
    </article>
  </main>
</body>
</html>
`))

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintln(w, "ok")
}

func runCommand(ctx context.Context, args []string) error {
	switch args[0] {
	case "import-file":
		if len(args) != 2 {
			return fmt.Errorf("usage: dailydocs import-file path/to/topic.yaml")
		}

		conn, err := db.Open(ctx, os.Getenv("DB_PATH"))
		if err != nil {
			return err
		}
		defer conn.Close()

		result, err := seed.ImportFile(ctx, conn, args[1])
		if err != nil {
			return err
		}

		log.Printf("imported topic=%s pages_found=%d pages_imported=%d", result.TopicSlug, result.PagesFound, result.PagesImported)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
