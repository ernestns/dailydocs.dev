package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/ernestns/daily-docs/internal/db"
	"github.com/ernestns/daily-docs/internal/seed"
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
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
