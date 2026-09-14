package vector

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// indexTestRows spans more than two chunks of indexTestChunkSize so searches,
// deletes, and slot moves cross chunk boundaries.
const (
	indexTestChunkSize = 1024
	indexTestRows      = 2*indexTestChunkSize + 500
)

func indexTestVector(i int) []float32 {
	return []float32{float32(i % 97), float32(i % 89), float32(i % 83), float32(i % 79)}
}

type indexRow struct {
	id   int64
	dist float64
}

func queryRows(t *testing.T, conn *sqlite.Conn, query string, args ...any) []indexRow {
	t.Helper()
	var rows []indexRow
	err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{
		Args: args,
		ResultFunc: func(stmt *sqlite.Stmt) error {
			rows = append(rows, indexRow{stmt.ColumnInt64(0), stmt.ColumnFloat(1)})
			return nil
		},
	})
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return rows
}

func execArgs(t *testing.T, conn *sqlite.Conn, query string, args ...any) {
	t.Helper()
	if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// openIndexTestConn creates a vector_index table "idx" of the given storage
// type and a plain reference table "ref" holding the same vectors.
func openIndexTestConn(t *testing.T, storage string) *sqlite.Conn {
	t.Helper()
	conn := openTestConn(t)
	if err := Register(conn, 4, WithQuantRange(0, 100)); err != nil {
		t.Fatal(err)
	}
	execArgs(t, conn, fmt.Sprintf("CREATE VIRTUAL TABLE idx USING vector_index(%s, chunk_size=%d)", storage, indexTestChunkSize))
	execArgs(t, conn, "CREATE TABLE ref (id INTEGER PRIMARY KEY, e BLOB, q BLOB)")
	execArgs(t, conn, "BEGIN")
	for i := range indexTestRows {
		b := Float32ToBlob(indexTestVector(i))
		execArgs(t, conn, "INSERT INTO idx (rowid, embedding) VALUES (?, ?)", i, b)
		execArgs(t, conn, "INSERT INTO ref (id, e, q) VALUES (?, ?, vector_quantize(?))", i, b, b)
	}
	execArgs(t, conn, "COMMIT")
	return conn
}

func refDistance(storage string) string {
	if storage == "int8" {
		return "vector_distance_q(q, vector_quantize(?1))"
	}
	return "vector_distance(e, ?1)"
}

func assertRowsEqual(t *testing.T, got, want []indexRow) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d rows, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestVectorIndexSearch(t *testing.T) {
	query := Float32ToBlob([]float32{50, 40, 30, 20})
	tests := []struct {
		name  string
		query string
		ref   string
	}{
		{
			name:  "k constraint",
			query: "SELECT rowid, distance FROM idx WHERE embedding MATCH ?1 AND k = 10",
			ref:   "SELECT id, %DIST% AS d FROM ref ORDER BY d, id LIMIT 10",
		},
		{
			name:  "LIMIT",
			query: "SELECT rowid, distance FROM idx WHERE embedding MATCH ?1 ORDER BY distance LIMIT 25",
			ref:   "SELECT id, %DIST% AS d FROM ref ORDER BY d, id LIMIT 25",
		},
		{
			name:  "LIMIT with OFFSET",
			query: "SELECT rowid, distance FROM idx WHERE embedding MATCH ?1 ORDER BY distance LIMIT 10 OFFSET 5",
			ref:   "SELECT id, %DIST% AS d FROM ref ORDER BY d, id LIMIT 10 OFFSET 5",
		},
		{
			name:  "k larger than table",
			query: "SELECT rowid, distance FROM idx WHERE embedding MATCH ?1 AND k = 100000",
			ref:   "SELECT id, %DIST% AS d FROM ref ORDER BY d, id",
		},
		{
			name:  "k zero",
			query: "SELECT rowid, distance FROM idx WHERE embedding MATCH ?1 AND k = 0",
			ref:   "SELECT id, %DIST% AS d FROM ref WHERE 0",
		},
		{
			name:  "join with k",
			query: "SELECT r.id, v.distance FROM idx v JOIN ref r ON r.id = v.rowid WHERE v.embedding MATCH ?1 AND v.k = 10 ORDER BY v.distance",
			ref:   "SELECT id, %DIST% AS d FROM ref ORDER BY d, id LIMIT 10",
		},
		{
			name:  "join with LIMIT in subquery",
			query: "SELECT r.id, v.distance FROM (SELECT rowid, distance FROM idx WHERE embedding MATCH ?1 ORDER BY distance LIMIT 10) v JOIN ref r ON r.id = v.rowid ORDER BY v.distance",
			ref:   "SELECT id, %DIST% AS d FROM ref ORDER BY d, id LIMIT 10",
		},
	}
	for _, storage := range []string{"float32", "int8"} {
		conn := openIndexTestConn(t, storage)
		for _, tt := range tests {
			t.Run(storage+"/"+tt.name, func(t *testing.T) {
				got := queryRows(t, conn, tt.query, query)
				want := queryRows(t, conn, strings.ReplaceAll(tt.ref, "%DIST%", refDistance(storage)), query)
				assertRowsEqual(t, got, want)
			})
		}
	}
}

func TestVectorIndexWrites(t *testing.T) {
	tests := []struct {
		name string
		idx  []string
		ref  []string
	}{
		{
			name: "delete from middle of chunks",
			idx:  []string{"DELETE FROM idx WHERE rowid % 7 = 3"},
			ref:  []string{"DELETE FROM ref WHERE id % 7 = 3"},
		},
		{
			name: "delete entire first chunk",
			idx:  []string{"DELETE FROM idx WHERE rowid < 1100"},
			ref:  []string{"DELETE FROM ref WHERE id < 1100"},
		},
		{
			name: "delete then insert reuses space",
			idx:  []string{"DELETE FROM idx WHERE rowid < 300", "INSERT INTO idx (rowid, embedding) VALUES (100000, X'0000c8420000c8420000c8420000c842')"},
			ref:  []string{"DELETE FROM ref WHERE id < 300", "INSERT INTO ref (id, e, q) VALUES (100000, X'0000c8420000c8420000c8420000c842', vector_quantize(X'0000c8420000c8420000c8420000c842'))"},
		},
		{
			name: "update embedding",
			idx:  []string{"UPDATE idx SET embedding = X'00004842000020420000f0410000a041' WHERE rowid IN (5, 1500, 2400)"},
			ref:  []string{"UPDATE ref SET e = X'00004842000020420000f0410000a041', q = vector_quantize(X'00004842000020420000f0410000a041') WHERE id IN (5, 1500, 2400)"},
		},
		{
			name: "change rowid",
			idx:  []string{"UPDATE idx SET rowid = rowid + 1000000 WHERE rowid IN (7, 2000)"},
			ref:  []string{"UPDATE ref SET id = id + 1000000 WHERE id IN (7, 2000)"},
		},
		{
			name: "rolled back insert is not visible",
			idx:  []string{"BEGIN", "INSERT INTO idx (rowid, embedding) VALUES (200000, X'0000c8420000c8420000c8420000c842')", "ROLLBACK"},
		},
		{
			name: "insert without rowid assigns one",
			idx:  []string{"INSERT INTO idx (embedding) VALUES (X'000080400000804000008040000080C0')"},
			ref:  []string{"INSERT INTO ref (e, q) VALUES (X'000080400000804000008040000080C0', vector_quantize(X'000080400000804000008040000080C0'))"},
		},
	}
	query := Float32ToBlob([]float32{50, 50, 50, 50})
	for _, storage := range []string{"float32", "int8"} {
		for _, tt := range tests {
			t.Run(storage+"/"+tt.name, func(t *testing.T) {
				conn := openIndexTestConn(t, storage)
				for _, q := range tt.idx {
					execArgs(t, conn, q)
				}
				for _, q := range tt.ref {
					execArgs(t, conn, q)
				}
				got := queryRows(t, conn, "SELECT rowid, distance FROM idx WHERE embedding MATCH ?1 AND k = 1000000", query)
				want := queryRows(t, conn, "SELECT id, "+refDistance(storage)+" AS d FROM ref ORDER BY d, id", query)
				assertRowsEqual(t, got, want)

				col := "e"
				if storage == "int8" {
					col = "q"
				}
				var mismatches, rows int
				err := sqlitex.Execute(conn, "SELECT i.embedding, r."+col+" FROM idx i JOIN ref r ON r.id = i.rowid", &sqlitex.ExecOptions{
					ResultFunc: func(stmt *sqlite.Stmt) error {
						rows++
						a := make([]byte, stmt.ColumnLen(0))
						b := make([]byte, stmt.ColumnLen(1))
						stmt.ColumnBytes(0, a)
						stmt.ColumnBytes(1, b)
						if !bytes.Equal(a, b) {
							mismatches++
						}
						return nil
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				if rows != len(want) || mismatches != 0 {
					t.Fatalf("stored embeddings: %d rows (want %d), %d mismatches", rows, len(want), mismatches)
				}
			})
		}
	}
}

func TestVectorIndexErrors(t *testing.T) {
	tests := []struct {
		name    string
		setup   string
		query   string
		wantErr string
	}{
		{
			name:    "unknown argument",
			query:   "CREATE VIRTUAL TABLE bad USING vector_index(float64)",
			wantErr: `unknown argument "float64"`,
		},
		{
			name:    "zero chunk_size",
			query:   "CREATE VIRTUAL TABLE bad USING vector_index(chunk_size=0)",
			wantErr: `chunk_size must be a positive integer, got "0"`,
		},
		{
			name:    "non-integer chunk_size",
			query:   "CREATE VIRTUAL TABLE bad USING vector_index(chunk_size=big)",
			wantErr: `chunk_size must be a positive integer, got "big"`,
		},
		{
			name:    "int8 without quantization range",
			setup:   "noquant",
			query:   "CREATE VIRTUAL TABLE bad USING vector_index(int8)",
			wantErr: "int8 storage requires Register with WithQuantRange",
		},
		{
			name:    "insert wrong length",
			query:   "INSERT INTO idx (rowid, embedding) VALUES (1, X'00000000')",
			wantErr: "embedding: expected 16 bytes (dim=4), got 4",
		},
		{
			name:    "insert NULL embedding",
			query:   "INSERT INTO idx (rowid, embedding) VALUES (1, NULL)",
			wantErr: "embedding must be a float32 blob",
		},
		{
			name:    "duplicate rowid",
			query:   "INSERT INTO idx (rowid, embedding) VALUES (0, X'00000000000000000000000000000000')",
			wantErr: "UNIQUE constraint failed",
		},
		{
			name:    "update to existing rowid keeps row",
			query:   "UPDATE idx SET rowid = 0 WHERE rowid = 1",
			wantErr: "UNIQUE constraint failed",
		},
		{
			name:    "MATCH without k or LIMIT",
			query:   "SELECT rowid FROM idx WHERE embedding MATCH X'00000000000000000000000000000000'",
			wantErr: "search requires k = ? or LIMIT",
		},
		{
			name:    "LIMIT through a join",
			query:   "SELECT r.id FROM idx v JOIN ref r ON r.id = v.rowid WHERE v.embedding MATCH X'00000000000000000000000000000000' LIMIT 5",
			wantErr: "search requires k = ? or LIMIT",
		},
		{
			name:    "MATCH wrong length",
			query:   "SELECT rowid FROM idx WHERE embedding MATCH X'0000' AND k = 5",
			wantErr: "MATCH query: expected 16 bytes (dim=4), got 2",
		},
		{
			name:    "negative k",
			query:   "SELECT rowid FROM idx WHERE embedding MATCH X'00000000000000000000000000000000' AND k = -1",
			wantErr: "k must be non-negative",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := openTestConn(t)
			if tt.setup == "noquant" {
				if err := Register(conn, 4); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := Register(conn, 4, WithQuantRange(0, 100)); err != nil {
					t.Fatal(err)
				}
				execArgs(t, conn, "CREATE VIRTUAL TABLE idx USING vector_index()")
				execArgs(t, conn, "CREATE TABLE ref (id INTEGER PRIMARY KEY)")
				execArgs(t, conn, "INSERT INTO idx (rowid, embedding) VALUES (0, X'00000000000000000000000000000000'), (1, X'0000803f0000803f0000803f0000803f')")
				execArgs(t, conn, "INSERT INTO ref (id) VALUES (0)")
			}
			err := sqlitex.ExecuteTransient(conn, tt.query, nil)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
			if tt.setup != "noquant" {
				rows := queryRows(t, conn, "SELECT rowid, 0 FROM idx ORDER BY rowid")
				assertRowsEqual(t, rows, []indexRow{{0, 0}, {1, 0}})
			}
		})
	}
}

func TestVectorIndexChunkSize(t *testing.T) {
	tests := []struct {
		name string
		dim  int
		args string
		want int
	}{
		{name: "float32 default at dim 4", dim: 4, args: "", want: 65536},
		{name: "int8 default at dim 4", dim: 4, args: "int8", want: 262144},
		{name: "float32 default at dim 1536", dim: 1536, args: "float32", want: 170},
		{name: "int8 default at dim 1536", dim: 1536, args: "int8", want: 682},
		{name: "default when vector exceeds target", dim: 300000, args: "float32", want: 1},
		{name: "explicit chunk_size", dim: 4, args: "chunk_size = 7", want: 7},
		{name: "explicit chunk_size with storage type", dim: 4, args: "int8, chunk_size=7", want: 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn := openTestConn(t)
			if err := Register(conn, tt.dim, WithQuantRange(0, 100)); err != nil {
				t.Fatal(err)
			}
			execArgs(t, conn, "CREATE VIRTUAL TABLE idx USING vector_index("+tt.args+")")
			rows := queryRows(t, conn, "SELECT chunk_size, 0 FROM idx_info")
			if len(rows) != 1 || rows[0].id != int64(tt.want) {
				t.Fatalf("chunk_size = %+v, want %d", rows, tt.want)
			}
			if tt.want != 7 {
				return
			}
			v := Float32ToBlob([]float32{1, 2, 3, 4})
			for i := range 20 {
				execArgs(t, conn, "INSERT INTO idx (rowid, embedding) VALUES (?, ?)", i, v)
			}
			rows = queryRows(t, conn, "SELECT count(*), max(size) FROM idx_chunks")
			if len(rows) != 1 || rows[0] != (indexRow{3, 7}) {
				t.Fatalf("chunks (count, max size) = %+v, want (3, 7)", rows)
			}
		})
	}
}

func TestVectorIndexSchema(t *testing.T) {
	shadowCount := func(t *testing.T, conn *sqlite.Conn, prefix string) int {
		t.Helper()
		var n int
		err := sqlitex.Execute(conn, "SELECT count(*) FROM sqlite_schema WHERE name LIKE ?", &sqlitex.ExecOptions{
			Args:       []any{prefix + "\\_%"},
			ResultFunc: func(stmt *sqlite.Stmt) error { n = stmt.ColumnInt(0); return nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	open := func(t *testing.T, path string, dim int, opts ...Option) *sqlite.Conn {
		t.Helper()
		conn, err := sqlite.OpenConn(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := Register(conn, dim, opts...); err != nil {
			t.Fatal(err)
		}
		return conn
	}
	query := Float32ToBlob([]float32{1, 1, 1, 1})

	t.Run("reopen uses stored vectors", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "idx.db")
		conn := open(t, path, 4, WithQuantRange(0, 100))
		execArgs(t, conn, "CREATE VIRTUAL TABLE idx USING vector_index(int8)")
		execArgs(t, conn, "INSERT INTO idx (rowid, embedding) VALUES (1, ?), (2, ?)", Float32ToBlob([]float32{1, 1, 1, 1}), Float32ToBlob([]float32{9, 9, 9, 9}))
		conn.Close()

		conn = open(t, path, 4, WithQuantRange(0, 100))
		defer conn.Close()
		rows := queryRows(t, conn, "SELECT rowid, distance FROM idx WHERE embedding MATCH ?1 AND k = 1", query)
		if len(rows) != 1 || rows[0].id != 1 {
			t.Fatalf("rows = %+v, want rowid 1", rows)
		}
	})

	for _, tt := range []struct {
		name    string
		dim     int
		opts    []Option
		wantErr string
	}{
		{"reopen with different dimension", 8, []Option{WithQuantRange(0, 100)}, "has dimension 4, Register was called with 8"},
		{"reopen with different quantization range", 4, []Option{WithQuantRange(-1, 1)}, "was created with quantization range [0, 100]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "idx.db")
			conn := open(t, path, 4, WithQuantRange(0, 100))
			execArgs(t, conn, "CREATE VIRTUAL TABLE idx USING vector_index(int8)")
			conn.Close()

			conn = open(t, path, tt.dim, tt.opts...)
			defer conn.Close()
			err := sqlitex.ExecuteTransient(conn, "SELECT rowid FROM idx", nil)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}

	t.Run("rename moves shadow tables", func(t *testing.T) {
		conn := open(t, ":memory:", 4)
		defer conn.Close()
		execArgs(t, conn, "CREATE VIRTUAL TABLE idx USING vector_index()")
		execArgs(t, conn, "INSERT INTO idx (rowid, embedding) VALUES (1, ?)", query)
		execArgs(t, conn, "ALTER TABLE idx RENAME TO docs")
		if n := shadowCount(t, conn, "idx"); n != 0 {
			t.Fatalf("%d idx_ shadow objects remain", n)
		}
		rows := queryRows(t, conn, "SELECT rowid, distance FROM docs WHERE embedding MATCH ?1 AND k = 1", query)
		if len(rows) != 1 || rows[0].id != 1 {
			t.Fatalf("rows = %+v, want rowid 1", rows)
		}
		execArgs(t, conn, "CREATE VIRTUAL TABLE idx USING vector_index()")
	})

	t.Run("drop removes shadow tables", func(t *testing.T) {
		conn := open(t, ":memory:", 4)
		defer conn.Close()
		execArgs(t, conn, "CREATE VIRTUAL TABLE idx USING vector_index()")
		execArgs(t, conn, "DROP TABLE idx")
		if n := shadowCount(t, conn, "idx"); n != 0 {
			t.Fatalf("%d idx_ shadow objects remain", n)
		}
	})
}
