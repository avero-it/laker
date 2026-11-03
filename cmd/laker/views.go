package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/rs/zerolog"

	"github.com/avero-it/laker/intern/core/domain"
)

var (
	identRE = regexp.MustCompile(`[^a-z0-9]+`)
	errStop = errors.New("ERR092: stop")
)

// commandViews prints the DuckDB SQL that turns the lake into views: every
// message, the file history, the extracted texts and, for each database
// table, its latest rows, its history and a macro for the rows at a date.
func commandViews(configPath string, withSecrets bool) {
	cfg := mustConfig(configPath)
	lake, err := buildLake(cfg, zerolog.Nop())
	if err != nil {
		fatalf("ERR090: %v", err)
	}

	var root string
	var b strings.Builder
	switch l := cfg.Common.Lake; l.Use {
	case "s3":
		root = "s3://" + l.S3.Bucket
		host := strings.TrimPrefix(strings.TrimPrefix(l.S3.Endpoint, "https://"), "http://")
		style := "vhost"
		if l.S3.PathStyle {
			style = "path"
		}
		// without the keys, DuckDB reads them from AWS_ACCESS_KEY_ID and
		// AWS_SECRET_ACCESS_KEY
		creds := "PROVIDER credential_chain, CHAIN 'env'"
		if withSecrets {
			creds = fmt.Sprintf("KEY_ID '%s', SECRET '%s'", l.S3.AccessKey, l.S3.SecretKey)
		}
		fmt.Fprintf(&b, "INSTALL httpfs;\nLOAD httpfs;\nINSTALL aws;\nLOAD aws;\n")
		fmt.Fprintf(&b, "CREATE OR REPLACE SECRET laker (TYPE s3, %s, ENDPOINT '%s', REGION '%s', URL_STYLE '%s', USE_SSL %v);\n\n",
			creds, host, l.S3.Region, style, strings.HasPrefix(l.S3.Endpoint, "https://"))
	default:
		abs, err := filepath.Abs(l.FS.Path)
		if err != nil {
			fatalf("ERR091: %v", err)
		}
		root = abs
	}

	read := func(prefix string) string {
		return fmt.Sprintf("read_parquet('%s/%s/**/*.parquet', union_by_name = true)", root, prefix)
	}
	// DuckDB refuses a view over no file: only what the lake holds
	has := func(prefix string) bool {
		found := false
		_ = lake.List(context.Background(), prefix+"/", func(string) error { found = true; return errStop })
		return found
	}
	if has("mail/messages") {
		fmt.Fprintf(&b, "-- every message, once per folder it is in\nCREATE OR REPLACE VIEW mail AS SELECT * FROM %s;\n", read("mail/messages"))
		fmt.Fprintf(&b, "-- every message once\nCREATE OR REPLACE VIEW mail_messages AS SELECT * EXCLUDE (folder, uid, uidvalidity, flags) FROM mail WHERE sha256 <> '' QUALIFY row_number() OVER (PARTITION BY sha256 ORDER BY seen_at) = 1;\n\n")
	}
	if has("chat/messages") {
		fmt.Fprintf(&b, "-- every chat message row: originals, edits and deletions\nCREATE OR REPLACE VIEW chat_history AS SELECT * EXCLUDE (chat_key, msg_key), coalesce(nullif(chat_key, ''), chat_id::VARCHAR) AS chat_key, coalesce(nullif(msg_key, ''), msg_id::VARCHAR) AS msg_key FROM %s;\n", read("chat/messages"))
		fmt.Fprintf(&b, "-- every chat message once, in its last version (op = 'delete': deleted at the source)\nCREATE OR REPLACE VIEW chats AS SELECT * FROM chat_history QUALIFY row_number() OVER (PARTITION BY source, chat_key, msg_key ORDER BY seen_at DESC, run_id DESC) = 1;\n\n")
	}
	if has("files/inventory") {
		fmt.Fprintf(&b, "-- every change seen in the scanned trees\nCREATE OR REPLACE VIEW files_history AS SELECT * FROM %s;\n", read("files/inventory"))
		fmt.Fprintf(&b, "-- the files present now\nCREATE OR REPLACE VIEW files AS SELECT * FROM files_history QUALIFY row_number() OVER (PARTITION BY source, path ORDER BY seen_at DESC, run_id DESC) = 1 AND op <> 'delete';\n\n")
	}
	if has("files/text") {
		fmt.Fprintf(&b, "-- texts extracted from the files\nCREATE OR REPLACE VIEW files_text AS SELECT * FROM %s;\n\n", read("files/text"))
	}

	tables := map[string]bool{}
	_ = lake.List(context.Background(), "db/", func(k string) error {
		if source, table, ok := domain.DBTableOf(k); ok {
			tables[source+"/"+table] = true
		}
		return nil
	})
	names := make([]string, 0, len(tables))
	for t := range tables {
		names = append(names, t)
	}
	sort.Strings(names)
	for _, t := range names {
		source, table, _ := strings.Cut(t, "/")
		v := "db_" + strings.Trim(identRE.ReplaceAllString(strings.ToLower(source+"_"+table), "_"), "_")
		meta := fmt.Sprintf("%s, %s, %s, %s", domain.DBOp, domain.DBRowHash, domain.DBSchemaHash, domain.DBRunID)
		latest := fmt.Sprintf("QUALIFY row_number() OVER (PARTITION BY %s ORDER BY %s DESC, %s DESC) = 1", domain.DBPK, domain.DBSeenAt, domain.DBRunID)
		fmt.Fprintf(&b, "-- %s: %s\n", source, table)
		fmt.Fprintf(&b, "CREATE OR REPLACE VIEW %s_history AS SELECT * FROM %s;\n", v, read("db/"+source+"/"+table+"/changes"))
		fmt.Fprintf(&b, "CREATE OR REPLACE VIEW %s AS SELECT * EXCLUDE (%s) FROM (SELECT * FROM %s_history %s) WHERE %s <> 'delete';\n",
			v, meta, v, latest, domain.DBOp)
		fmt.Fprintf(&b, "CREATE OR REPLACE MACRO %s_at(ts) AS TABLE SELECT * EXCLUDE (%s) FROM (SELECT * FROM %s_history WHERE %s <= ts::TIMESTAMPTZ %s) WHERE %s <> 'delete';\n\n",
			v, meta, v, domain.DBSeenAt, latest, domain.DBOp)
	}
	fmt.Print(b.String())
}
