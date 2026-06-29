package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/seed"
	"github.com/ernestns/daily-docs/internal/topicsearch"
	"github.com/ernestns/daily-docs/internal/validator"
)

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
	case "validate-links":
		if len(args) != 1 {
			return fmt.Errorf("usage: dailydocs validate-links")
		}

		conn, err := db.Open(ctx, os.Getenv("DB_PATH"))
		if err != nil {
			return err
		}
		defer conn.Close()

		result, err := validator.ValidateLinks(ctx, conn, nil, validator.DefaultFailureThreshold)
		if err != nil {
			return err
		}

		log.Printf("validated links checked=%d healthy=%d failed=%d disabled=%d", result.Checked, result.Healthy, result.Failed, result.Disabled)
		return nil
	case "search-topic":
		if len(args) != 2 {
			return fmt.Errorf("usage: dailydocs search-topic topic")
		}

		conn, err := db.Open(ctx, os.Getenv("DB_PATH"))
		if err != nil {
			return err
		}
		defer conn.Close()

		result, err := topicsearch.SearchTopic(ctx, conn, args[1], topicsearch.Options{
			Provider: topicsearch.TavilyClient{
				APIKey:   os.Getenv("TAVILY_API_KEY"),
				Endpoint: os.Getenv("TAVILY_ENDPOINT"),
			},
			Reviewer: openAIReviewerFromEnv(),
		})
		if err != nil {
			return err
		}

		log.Printf("searched topic=%s run_id=%d status=%s results=%d stored=%d", result.TopicSlug, result.RunID, result.Status, result.ResultCount, result.StoredCount)
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
