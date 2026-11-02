package seed

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
)

func TestImportRejectsTrailingDocumentsWithoutChangingStoredData(t *testing.T) {
	for name, trailing := range map[string]string{
		"second topic":              "---\ntopic: rust\nname: Rust\npages:\n  - title: Ownership\n    url: https://docs.example.org/ownership\n",
		"malformed second document": "---\npages: [\n",
		"empty second document":     "---\n",
		"content after end marker":  "...\nunexpected trailing content\n",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			conn := openTestDB(t, ctx)
			defer conn.Close()
			if _, err := ImportTopic(ctx, conn, TopicFile{Topic: "go", Name: "Go", Pages: []PageFile{
				{Title: "Context", URL: "https://docs.example.org/context", Official: true},
				{Title: "Modules", URL: "https://docs.example.org/modules"},
			}}); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(`
				INSERT INTO daily_readings(topic_id,reading_date,page_id)
				SELECT topic_id,date('2026-01-01','+' || (reading_order-1) || ' days'),id FROM pages;
				INSERT INTO topic_search_runs(id,topic_id,provider,query,status,error)
				VALUES(1,1,'fixture','old query','failed','old failure');
				INSERT INTO topic_search_results(topic_id,search_run_id,title,url,rank,accepted,stored_as_page_id)
				SELECT topic_id,1,title,url,reading_order,1,id FROM pages;
			`); err != nil {
				t.Fatal(err)
			}
			before := snapshotSeedData(t, conn)
			path := writeSeed(t, "topic: go\nname: Changed by first document\npages:\n  - title: Modules refresh\n    url: https://docs.example.org/modules\n"+trailing)
			if result, err := ImportFile(ctx, conn, path); err == nil || result != (Result{}) {
				t.Fatalf("trailing document was accepted: result=%+v err=%v", result, err)
			}
			if after := snapshotSeedData(t, conn); !reflect.DeepEqual(before, after) {
				t.Fatalf("rejected file changed catalog, history or import records: before=%v after=%v", before, after)
			}
		})
	}
}

func TestSingleDocumentImportAndReimportKeepSupportedYAMLConventions(t *testing.T) {
	for _, syntax := range []struct{ name, prefix, suffix string }{
		{"plain", "", ""},
		{"markers", "---\n", "\n...\n"},
		{"comments and whitespace", "\n# seed\n\n", "\n# end\n\n"},
		{"marked with trailing comments", "---\n", "\n...\n# end\n\n"},
	} {
		t.Run(syntax.name, func(t *testing.T) {
			ctx := context.Background()
			conn := openTestDB(t, ctx)
			defer conn.Close()
			first := "topic: go\nname: Go\npages:\n  - title: Context\n    url: https://docs.example.org/context\n  - title: Modules\n    url: https://docs.example.org/modules\n"
			second := "topic: go\nname: Go Reference\npages:\n  - title: Modules Reference\n    url: https://docs.example.org/modules\n  - title: Testing\n    url: https://docs.example.org/testing\n"
			for i, contents := range []string{first, second} {
				result, err := ImportFile(ctx, conn, writeSeed(t, syntax.prefix+contents+syntax.suffix))
				if err != nil || result != (Result{TopicSlug: "go", PagesFound: 2, PagesImported: 2}) {
					t.Fatalf("valid import %d failed: %+v %v", i+1, result, err)
				}
				if i == 0 {
					if _, err := conn.Exec(`INSERT INTO daily_readings(topic_id,reading_date,page_id) SELECT topic_id,date('2026-01-01','+' || (reading_order-1) || ' days'),id FROM pages`); err != nil {
						t.Fatal(err)
					}
				}
			}
			var name, title, savedURL string
			var pages, active, imports, assignedID, oldActive, order int
			if err := conn.QueryRow(`SELECT name,
				(SELECT count(*) FROM pages),(SELECT count(*) FROM pages WHERE active=1),
				(SELECT count(*) FROM imports WHERE status='completed'),
				(SELECT page_id FROM daily_readings WHERE reading_date='2026-01-01'),
				(SELECT url FROM pages WHERE id=1),(SELECT active FROM pages WHERE id=1),
				(SELECT title FROM pages WHERE id=2),(SELECT reading_order FROM pages WHERE id=2)
				FROM topics WHERE slug='go'`).Scan(&name, &pages, &active, &imports, &assignedID, &savedURL, &oldActive, &title, &order); err != nil {
				t.Fatal(err)
			}
			if name != "Go Reference" || pages != 3 || active != 2 || imports != 2 || assignedID != 1 || savedURL != "https://docs.example.org/context" || oldActive != 0 || title != "Modules Reference" || order != 1 {
				t.Fatal("single-document replacement changed existing import/history behavior", name, pages, active, imports, assignedID, savedURL, oldActive, title, order)
			}
		})
	}
}

func snapshotSeedData(t *testing.T, conn *sql.DB) map[string]string {
	t.Helper()
	state := make(map[string]string)
	for _, table := range []string{"topics", "pages", "imports", "daily_readings", "topic_search_runs", "topic_search_results"} {
		state[table] = snapshotSeedTable(t, conn, table)
	}
	return state
}

func snapshotSeedTable(t *testing.T, conn *sql.DB, table string) string {
	t.Helper()
	rows, err := conn.Query("SELECT * FROM " + table + " ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		if err := encoder.Encode(values); err != nil {
			t.Fatal(err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return data.String()
}
