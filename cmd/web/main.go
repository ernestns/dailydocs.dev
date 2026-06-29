package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/topicsearch"
)

var topicPathPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

const topicProcessingDailyLimit = 20

type app struct {
	db             *sql.DB
	now            func() time.Time
	searchMu       *sync.Mutex
	searchProvider topicsearch.Provider
	searchReviewer topicsearch.Reviewer
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

	var searchProvider topicsearch.Provider
	if os.Getenv("TAVILY_API_KEY") != "" {
		searchProvider = topicsearch.TavilyClient{
			APIKey:   os.Getenv("TAVILY_API_KEY"),
			Endpoint: os.Getenv("TAVILY_ENDPOINT"),
		}
	}
	searchReviewer := openAIReviewerFromEnv()

	app := app{
		db:             conn,
		now:            func() time.Time { return time.Now().UTC() },
		searchMu:       &sync.Mutex{},
		searchProvider: searchProvider,
		searchReviewer: searchReviewer,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.HandleFunc("/topics", app.topicsHandler)
	mux.HandleFunc("/topics/", app.topicEvaluationsHandler)
	mux.HandleFunc("/topics/search", app.searchTopicsHandler)
	mux.HandleFunc("/process-topic", app.processTopicHandler)
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

func (a app) processNextQueuedTopic(ctx context.Context) {
	if a.searchProvider == nil {
		return
	}
	if a.searchMu != nil {
		a.searchMu.Lock()
		defer a.searchMu.Unlock()
	}
	result, err := topicsearch.ProcessNextQueuedTopic(ctx, a.db, topicsearch.Options{
		Provider:    a.searchProvider,
		Reviewer:    a.searchReviewer,
		Now:         a.now,
		MinInterval: time.Nanosecond,
		DailyLimit:  topicProcessingDailyLimit,
	})
	if err != nil {
		if result.Processed {
			log.Printf("topic processor failed topic=%s error=%v", result.Result.TopicSlug, err)
			return
		}
		log.Printf("topic processor failed: %v", err)
		return
	}
	if result.Processed {
		log.Printf("topic processor processed topic=%s status=%s results=%d stored=%d", result.Result.TopicSlug, result.Result.Status, result.Result.ResultCount, result.Result.StoredCount)
	} else if result.DailyLimitReached {
		log.Printf("topic processor daily limit reached limit=%d", topicProcessingDailyLimit)
	}
}

func openAIReviewerFromEnv() topicsearch.Reviewer {
	if os.Getenv("OPENAI_API_KEY") == "" {
		return nil
	}
	return topicsearch.OpenAIReviewer{
		APIKey:   os.Getenv("OPENAI_API_KEY"),
		Endpoint: os.Getenv("OPENAI_ENDPOINT"),
		Model:    os.Getenv("OPENAI_MODEL"),
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = fmt.Fprintln(w, "ok")
}
