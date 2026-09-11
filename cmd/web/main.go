package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/topicsearch"
)

const topicProcessingDailyLimit = 20

type app struct {
	db              *sql.DB
	now             func() time.Time
	searchMu        *sync.Mutex
	pendingSearches *sync.Map
	searchProvider  topicsearch.Provider
	searchPlanner   topicsearch.Planner
	searchReviewer  topicsearch.Reviewer
	asyncProcessing bool
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

	trafficCollector := newTrafficCollector(conn)
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := trafficCollector.Close(flushCtx); err != nil {
			log.Print("traffic: final aggregate flush did not complete")
		}
	}()

	var searchProvider topicsearch.Provider
	if os.Getenv("TAVILY_API_KEY") != "" {
		searchProvider = topicsearch.TavilyClient{
			APIKey:   os.Getenv("TAVILY_API_KEY"),
			Endpoint: os.Getenv("TAVILY_ENDPOINT"),
		}
	}
	searchPlanner := openAIPlannerFromEnv()
	searchReviewer := openAIReviewerFromEnv()

	app := app{
		db:              conn,
		now:             func() time.Time { return time.Now().UTC() },
		searchMu:        &sync.Mutex{},
		pendingSearches: &sync.Map{},
		searchProvider:  searchProvider,
		searchPlanner:   searchPlanner,
		searchReviewer:  searchReviewer,
		asyncProcessing: true,
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
		Handler:           trafficCollector.Middleware(mux),
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

func (a app) processQueuedTopic(ctx context.Context, slug string) {
	if a.searchProvider == nil {
		return
	}
	if a.searchMu != nil {
		a.searchMu.Lock()
		defer a.searchMu.Unlock()
	}
	opts := topicsearch.Options{
		Provider:    a.searchProvider,
		Planner:     a.searchPlanner,
		Reviewer:    a.searchReviewer,
		Now:         a.now,
		MinInterval: time.Nanosecond,
		DailyLimit:  topicProcessingDailyLimit,
	}
	result, err := topicsearch.ProcessQueuedTopic(ctx, a.db, slug, opts)
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

func (a app) processQueuedTopicAsync(slug string) bool {
	if a.searchProvider == nil {
		return false
	}
	if !a.asyncProcessing {
		a.processQueuedTopic(context.Background(), slug)
		return true
	}
	if a.pendingSearches != nil {
		if _, pending := a.pendingSearches.LoadOrStore(slug, true); pending {
			return false
		}
	}
	go func() {
		if a.pendingSearches != nil {
			defer a.pendingSearches.Delete(slug)
		}
		// SearchTopic starts its bounded deadline after this job acquires the worker.
		a.processQueuedTopic(context.Background(), slug)
	}()
	return true
}

func openAIPlannerFromEnv() topicsearch.Planner {
	if os.Getenv("OPENAI_API_KEY") == "" {
		return nil
	}
	return topicsearch.OpenAITopicPlanner{
		APIKey:          os.Getenv("OPENAI_API_KEY"),
		Endpoint:        os.Getenv("OPENAI_ENDPOINT"),
		Model:           os.Getenv("OPENAI_PLANNER_MODEL"),
		ReasoningEffort: os.Getenv("OPENAI_PLANNER_REASONING_EFFORT"),
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
