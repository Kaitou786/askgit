package pgsync

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"text/template"

	"github.com/lib/pq"
	"go.uber.org/zap"

	_ "github.com/askgitdev/askgit/pkg/sqlite"
	_ "github.com/mattn/go-sqlite3"
)

type SyncOptions struct {
	Postgres  *sql.DB
	AskGit    *sql.DB
	TableName string
	Query     string
	Logger    *zap.Logger
}

// Sync imports the results of an askgit query into a postgres table.
// CAUTION: will overwrite (DROP!) the specified table and replace it.
func Sync(ctx context.Context, options *SyncOptions) error {
	l := options.Logger.Sugar().With(zap.String("pgTable", options.TableName))

	select {
	default:
	case <-ctx.Done():
		return ctx.Err()
	}

	rows, err := options.AskGit.QueryContext(ctx, options.Query)
	if err != nil {
		return err
	}
	defer rows.Close()

	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return err
	}

	colNames := make([]string, len(colTypes))
	for c := 0; c < len(colTypes); c++ {
		colNames[c] = colTypes[c].Name()
	}

	select {
	default:
	case <-ctx.Done():
		return ctx.Err()
	}

	tx, err := options.Postgres.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return err
	}

	handleErr := func(err error) {
		if err == nil {
			return
		}
		l.Error(err)
		if err := tx.Rollback(); err != nil {
			l.Error(err)
		}
	}

	tempNameNew := fmt.Sprintf("%s_temp", options.TableName)
	tempNameDrop := fmt.Sprintf("%s_drop", options.TableName)

	// create a new temp table
	createSQL, err := createTableFromSQLiteTypes(tempNameNew, colTypes)
	handleErr(err)

	select {
	default:
	case <-ctx.Done():
		return ctx.Err()
	}

	_, err = tx.ExecContext(ctx, createSQL)
	handleErr(err)

	select {
	default:
	case <-ctx.Done():
		return ctx.Err()
	}

	stmt, err := tx.PrepareContext(ctx, pq.CopyIn(tempNameNew, colNames...))
	handleErr(err)

	for rows.Next() {
		select {
		default:
		case <-ctx.Done():
			return ctx.Err()
		}

		values := make([]interface{}, len(colTypes))
		pointers := make([]interface{}, len(colTypes))

		for i := 0; i < len(values); i++ {
			pointers[i] = &values[i]
		}

		err := rows.Scan(pointers...)
		handleErr(err)

		_, err = stmt.ExecContext(ctx, values...)
		handleErr(err)
	}

	select {
	default:
	case <-ctx.Done():
		return ctx.Err()
	}

	_, err = stmt.ExecContext(ctx)
	handleErr(err)

	err = stmt.Close()
	handleErr(err)

	select {
	default:
	case <-ctx.Done():
		return ctx.Err()
	}

	_, err = tx.ExecContext(ctx, fmt.Sprintf(`
		ALTER TABLE IF EXISTS "%s" RENAME to %s;
		ALTER TABLE IF EXISTS "%s" RENAME TO "%s";
		DROP TABLE IF EXISTS "%s";
	`, options.TableName, tempNameDrop, tempNameNew, options.TableName, tempNameDrop))
	handleErr(err)

	select {
	default:
	case <-ctx.Done():
		return ctx.Err()
	}

	err = tx.Commit()
	if err != nil {
		return err
	}

	return nil
}

// sqliteTypeToPostgresType maps SQLite column types to Postgres column types
func sqliteTypeToPostgresType(col *sql.ColumnType) string {
	// TODO(patrickdevivo) expressions do not have a type-affinity in SQLite (unless explicitly cast)
	// which means something like `datetime('now')` will not have a type-affinity and be rendered into postgres
	// as text. Even `CAST(datetime('now') AS "DATETIME")` won't work because "DATETIME" is not a known affinity (becomes numeric).
	// All this means that there's not a way, using a SQL query alone, to coerce a column into a specific postgres type.
	// This matters mainly when there are expressions in a query (column name references will use the declared column type)
	switch col.DatabaseTypeName() {
	case "TEXT":
		return "text"
	case "INT":
		fallthrough
	case "INTEGER":
		return "integer"
	case "DATETIME":
		return "timestamp with time zone"
	case "BOOLEAN":
		return "boolean"
	default:
		return "text"
	}
}

// createTableFromSQLiteTypes produces a postgres CREATE TABLE statement from a set of SQLite columns
func createTableFromSQLiteTypes(tableName string, columns []*sql.ColumnType) (string, error) {
	const declare = `CREATE TABLE {{ .TableName }} (
		{{- range $c, $col := .Columns }}
			{{ quoteIdentifier .Name }} {{ colType $c }}{{ if columnComma $c }},{{ end }}
		{{- end }}
	  )`

	// helper to determine whether we're on the last column (and therefore should avoid a comma ",") in the range
	fns := template.FuncMap{
		"columnComma": func(c int) bool {
			return c < len(columns)-1
		},
		"colType": func(colIndex int) string {
			return sqliteTypeToPostgresType(columns[colIndex])
		},
		"quoteIdentifier": pq.QuoteIdentifier,
	}

	tmpl, err := template.New(fmt.Sprintf("declare_table_func_%s", tableName)).Funcs(fns).Parse(declare)
	if err != nil {
		return "", err
	}

	buf := new(bytes.Buffer)
	err = tmpl.Execute(buf, struct {
		TableName string
		Columns   []*sql.ColumnType
	}{
		pq.QuoteIdentifier(tableName),
		columns,
	})
	if err != nil {
		return "", err
	}

	return buf.String(), nil
}
