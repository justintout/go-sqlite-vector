package vector

import (
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"slices"
	"strings"
	"sync"

	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// indexChunkCapacity is the number of vectors stored in each chunk row.
// Searches read whole chunks, so larger chunks mean fewer SQLite rows to
// step through per query.
const indexChunkCapacity = 1024

const (
	indexColEmbedding = iota
	indexColDistance
	indexColK
)

const (
	indexPlanScan = iota
	indexPlanRowID
	indexPlanKNN
)

type vectorIndex struct {
	conn      *sqlite.Conn
	cfg       *config
	quantized bool
	db        string
	name      string
}

func indexConnect(cfg *config, create bool) sqlite.VTableConnectFunc {
	return func(conn *sqlite.Conn, opts *sqlite.VTableConnectOptions) (sqlite.VTable, *sqlite.VTableConfig, error) {
		vi := &vectorIndex{conn: conn, cfg: cfg, db: opts.DatabaseName, name: opts.VTableName}
		arg := strings.TrimSpace(strings.Join(opts.Args, ","))
		switch arg {
		case "", "float32":
		case "int8":
			vi.quantized = true
		default:
			return nil, nil, fmt.Errorf("vector_index: unknown storage type %q, expected float32 or int8", arg)
		}
		if vi.quantized && !cfg.quantEnabled {
			return nil, nil, fmt.Errorf("vector_index: int8 storage requires Register with WithQuantRange")
		}
		var err error
		if create {
			err = vi.create()
		} else {
			err = vi.check()
		}
		if err != nil {
			return nil, nil, err
		}
		return vi, &sqlite.VTableConfig{
			Declaration: "CREATE TABLE x(embedding BLOB, distance REAL HIDDEN, k INTEGER HIDDEN)",
		}, nil
	}
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func (vi *vectorIndex) shadow(suffix string) string {
	return quoteIdent(vi.db) + "." + quoteIdent(vi.name+"_"+suffix)
}

func (vi *vectorIndex) storageType() string {
	if vi.quantized {
		return "int8"
	}
	return "float32"
}

// vectorSize is the number of bytes one stored vector occupies in a chunk.
// int8 chunks store raw quantized values without the quantized blob header.
func (vi *vectorIndex) vectorSize() int {
	if vi.quantized {
		return vi.cfg.dim
	}
	return vi.cfg.dim * 4
}

func (vi *vectorIndex) exec(query string, args ...any) error {
	return sqlitex.Execute(vi.conn, query, &sqlitex.ExecOptions{Args: args})
}

func (vi *vectorIndex) create() error {
	var qmin, qmax any
	if vi.quantized {
		qmin, qmax = float64(vi.cfg.quantMin), float64(vi.cfg.quantMax)
	}
	stmts := []struct {
		query string
		args  []any
	}{
		{"CREATE TABLE " + vi.shadow("info") + " (dim INTEGER NOT NULL, type TEXT NOT NULL, quant_min REAL, quant_max REAL)", nil},
		{"INSERT INTO " + vi.shadow("info") + " VALUES (?, ?, ?, ?)", []any{vi.cfg.dim, vi.storageType(), qmin, qmax}},
		// Chunk sizes live apart from chunk data because updating any column
		// rewrites the whole row, blobs included. UNIQUE (size, id) indexes
		// size for finding a chunk with free slots; SQLite's automatic index
		// follows the table through renames.
		{"CREATE TABLE " + vi.shadow("chunks") + " (id INTEGER PRIMARY KEY, size INTEGER NOT NULL, UNIQUE (size, id))", nil},
		{"CREATE TABLE " + vi.shadow("data") + " (id INTEGER PRIMARY KEY, ids BLOB NOT NULL, vectors BLOB NOT NULL)", nil},
		{"CREATE TABLE " + vi.shadow("rowids") + " (rowid INTEGER PRIMARY KEY, chunk INTEGER NOT NULL, slot INTEGER NOT NULL)", nil},
	}
	for _, s := range stmts {
		if err := vi.exec(s.query, s.args...); err != nil {
			return fmt.Errorf("vector_index: create %s: %w", vi.name, err)
		}
	}
	return nil
}

func (vi *vectorIndex) check() error {
	var found bool
	var dim int
	var typ string
	var qmin, qmax float32
	err := sqlitex.Execute(vi.conn, "SELECT dim, type, quant_min, quant_max FROM "+vi.shadow("info"), &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			dim = stmt.ColumnInt(0)
			typ = stmt.ColumnText(1)
			qmin = float32(stmt.ColumnFloat(2))
			qmax = float32(stmt.ColumnFloat(3))
			return nil
		},
	})
	if err != nil {
		return fmt.Errorf("vector_index: connect %s: %w", vi.name, err)
	}
	switch {
	case !found:
		return fmt.Errorf("vector_index: connect %s: missing metadata", vi.name)
	case dim != vi.cfg.dim:
		return fmt.Errorf("vector_index: table %s has dimension %d, Register was called with %d", vi.name, dim, vi.cfg.dim)
	case typ != vi.storageType():
		return fmt.Errorf("vector_index: table %s stores %s, declaration says %s", vi.name, typ, vi.storageType())
	case vi.quantized && (qmin != vi.cfg.quantMin || qmax != vi.cfg.quantMax):
		return fmt.Errorf("vector_index: table %s was created with quantization range [%g, %g], Register was called with [%g, %g]",
			vi.name, qmin, qmax, vi.cfg.quantMin, vi.cfg.quantMax)
	}
	return nil
}

func (vi *vectorIndex) BestIndex(in *sqlite.IndexInputs) (*sqlite.IndexOutputs, error) {
	out := &sqlite.IndexOutputs{
		ConstraintUsage: make([]sqlite.IndexConstraintUsage, len(in.Constraints)),
		EstimatedCost:   1e9,
		EstimatedRows:   1e6,
	}
	match, k, limit, offset, rowid := -1, -1, -1, -1, -1
	for i, c := range in.Constraints {
		if !c.Usable {
			continue
		}
		switch {
		case c.Op == sqlite.IndexConstraintMatch && c.Column == indexColEmbedding:
			match = i
		case c.Op == sqlite.IndexConstraintEq && c.Column == indexColK:
			k = i
		case c.Op == sqlite.IndexConstraintLimit:
			limit = i
		case c.Op == sqlite.IndexConstraintOffset:
			offset = i
		case c.Op == sqlite.IndexConstraintEq && c.Column == -1:
			rowid = i
		}
	}
	switch {
	case match >= 0:
		out.ID.Num = indexPlanKNN
		out.ConstraintUsage[match] = sqlite.IndexConstraintUsage{ArgvIndex: 1, Omit: true}
		// LIMIT and OFFSET are not omitted: SQLite still applies them to the
		// rows the search returns.
		switch {
		case k >= 0:
			out.ID.String = "k"
			out.ConstraintUsage[k] = sqlite.IndexConstraintUsage{ArgvIndex: 2, Omit: true}
		case limit >= 0 && offset >= 0:
			out.ID.String = "limit,offset"
			out.ConstraintUsage[limit] = sqlite.IndexConstraintUsage{ArgvIndex: 2}
			out.ConstraintUsage[offset] = sqlite.IndexConstraintUsage{ArgvIndex: 3}
		case limit >= 0:
			out.ID.String = "limit"
			out.ConstraintUsage[limit] = sqlite.IndexConstraintUsage{ArgvIndex: 2}
		}
		out.OrderByConsumed = len(in.OrderBy) == 1 && in.OrderBy[0].Column == indexColDistance && !in.OrderBy[0].Desc
		out.EstimatedCost = 1e3
		out.EstimatedRows = 100
	case rowid >= 0:
		out.ID.Num = indexPlanRowID
		out.ConstraintUsage[rowid] = sqlite.IndexConstraintUsage{ArgvIndex: 1, Omit: true}
		out.EstimatedCost = 1
		out.EstimatedRows = 1
		out.IndexFlags = sqlite.IndexScanUnique
	}
	return out, nil
}

func (vi *vectorIndex) Open() (sqlite.VTableCursor, error) {
	return &indexCursor{vi: vi}, nil
}

func (vi *vectorIndex) Disconnect() error { return nil }

func (vi *vectorIndex) Destroy() error {
	for _, suffix := range []string{"info", "chunks", "data", "rowids"} {
		if err := vi.exec("DROP TABLE " + vi.shadow(suffix)); err != nil {
			return fmt.Errorf("vector_index: destroy %s: %w", vi.name, err)
		}
	}
	return nil
}

func (vi *vectorIndex) Rename(newName string) error {
	for _, suffix := range []string{"info", "chunks", "data", "rowids"} {
		if err := vi.exec("ALTER TABLE " + vi.shadow(suffix) + " RENAME TO " + quoteIdent(newName+"_"+suffix)); err != nil {
			return fmt.Errorf("vector_index: rename %s: %w", vi.name, err)
		}
	}
	vi.name = newName
	return nil
}

// encodeInput validates a float32 embedding blob and converts it to the
// stored representation.
func (vi *vectorIndex) encodeInput(v sqlite.Value, what string) ([]byte, error) {
	if v.Type() != sqlite.TypeBlob {
		return nil, fmt.Errorf("vector_index: %s must be a float32 blob, got %v", what, v.Type())
	}
	b := v.Blob()
	if len(b) != vi.cfg.dim*4 {
		return nil, fmt.Errorf("vector_index: %s: expected %d bytes (dim=%d), got %d", what, vi.cfg.dim*4, vi.cfg.dim, len(b))
	}
	if !vi.quantized {
		return slices.Clone(b), nil
	}
	floats, _ := BlobToFloat32(b)
	return quantize(floats, vi.cfg.quantMin, vi.cfg.quantMax)[2:], nil
}

func (vi *vectorIndex) locate(rowid int64) (chunk int64, slot int, ok bool, err error) {
	err = sqlitex.Execute(vi.conn, "SELECT chunk, slot FROM "+vi.shadow("rowids")+" WHERE rowid = ?", &sqlitex.ExecOptions{
		Args: []any{rowid},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			chunk, slot, ok = stmt.ColumnInt64(0), stmt.ColumnInt(1), true
			return nil
		},
	})
	return chunk, slot, ok, err
}

func (vi *vectorIndex) readAt(column string, chunk int64, offset int, buf []byte) error {
	blob, err := vi.conn.OpenBlob(vi.db, vi.name+"_data", column, chunk, false)
	if err != nil {
		return err
	}
	defer blob.Close()
	if _, err := blob.Seek(int64(offset), io.SeekStart); err != nil {
		return err
	}
	_, err = io.ReadFull(blob, buf)
	return err
}

func (vi *vectorIndex) writeAt(column string, chunk int64, offset int, data []byte) error {
	blob, err := vi.conn.OpenBlob(vi.db, vi.name+"_data", column, chunk, true)
	if err != nil {
		return err
	}
	if _, err := blob.Seek(int64(offset), io.SeekStart); err != nil {
		blob.Close()
		return err
	}
	if _, err := blob.Write(data); err != nil {
		blob.Close()
		return err
	}
	return blob.Close()
}

func (vi *vectorIndex) readVector(chunk int64, slot int) ([]byte, error) {
	buf := make([]byte, vi.vectorSize())
	if err := vi.readAt("vectors", chunk, slot*len(buf), buf); err != nil {
		return nil, err
	}
	if vi.quantized {
		return append([]byte{0x00, 0x01}, buf...), nil
	}
	return buf, nil
}

func (vi *vectorIndex) Update(params sqlite.VTableUpdateParams) (int64, error) {
	emb := params.Columns[indexColEmbedding]
	if params.IsInsert() {
		vec, err := vi.encodeInput(emb, "embedding")
		if err != nil {
			return 0, err
		}
		return vi.insert(params.NewRowID, vec)
	}

	oldID, newID := params.OldRowID.Int64(), params.NewRowID.Int64()
	chunk, slot, _, err := vi.locate(oldID)
	if err != nil {
		return 0, err
	}
	var vec []byte
	if emb.NoChange() {
		if newID == oldID {
			return oldID, nil
		}
		if vec, err = vi.readVector(chunk, slot); err != nil {
			return 0, err
		}
		if vi.quantized {
			vec = vec[2:]
		}
	} else if vec, err = vi.encodeInput(emb, "embedding"); err != nil {
		return 0, err
	}
	if newID == oldID {
		return oldID, vi.writeAt("vectors", chunk, slot*vi.vectorSize(), vec)
	}
	if err := vi.checkRowIDFree(newID); err != nil {
		return 0, err
	}
	if err := vi.DeleteRow(params.OldRowID); err != nil {
		return 0, err
	}
	return vi.insert(params.NewRowID, vec)
}

func (vi *vectorIndex) checkRowIDFree(rowid int64) error {
	_, _, exists, err := vi.locate(rowid)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("vector_index: UNIQUE constraint failed: %s.rowid (%d): %w", vi.name, rowid, sqlite.ResultConstraintPrimaryKey.ToError())
	}
	return nil
}

func (vi *vectorIndex) insert(rowid sqlite.Value, vec []byte) (int64, error) {
	if rowid.Type() != sqlite.TypeNull {
		if err := vi.checkRowIDFree(rowid.Int64()); err != nil {
			return 0, err
		}
	}

	var chunk int64
	var size int
	var found bool
	err := sqlitex.Execute(vi.conn, "SELECT id, size FROM "+vi.shadow("chunks")+" WHERE size < ? LIMIT 1", &sqlitex.ExecOptions{
		Args: []any{indexChunkCapacity},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			chunk, size, found = stmt.ColumnInt64(0), stmt.ColumnInt(1), true
			return nil
		},
	})
	if err != nil {
		return 0, err
	}
	if !found {
		if err := vi.exec("INSERT INTO " + vi.shadow("chunks") + " (size) VALUES (0)"); err != nil {
			return 0, err
		}
		chunk = vi.conn.LastInsertRowID()
		err := vi.exec("INSERT INTO "+vi.shadow("data")+" (id, ids, vectors) VALUES (?, zeroblob(?), zeroblob(?))",
			chunk, indexChunkCapacity*8, indexChunkCapacity*vi.vectorSize())
		if err != nil {
			return 0, err
		}
	}

	if rowid.Type() == sqlite.TypeNull {
		err = vi.exec("INSERT INTO "+vi.shadow("rowids")+" (chunk, slot) VALUES (?, ?)", chunk, size)
	} else {
		err = vi.exec("INSERT INTO "+vi.shadow("rowids")+" (rowid, chunk, slot) VALUES (?, ?, ?)", rowid.Int64(), chunk, size)
	}
	if err != nil {
		return 0, err
	}
	id := vi.conn.LastInsertRowID()

	if err := vi.writeAt("ids", chunk, size*8, binary.LittleEndian.AppendUint64(nil, uint64(id))); err != nil {
		return 0, err
	}
	if err := vi.writeAt("vectors", chunk, size*vi.vectorSize(), vec); err != nil {
		return 0, err
	}
	return id, vi.exec("UPDATE "+vi.shadow("chunks")+" SET size = size + 1 WHERE id = ?", chunk)
}

// DeleteRow removes a vector by moving the chunk's last vector into its slot,
// which keeps every chunk's occupied slots contiguous for scanning.
func (vi *vectorIndex) DeleteRow(rowid sqlite.Value) error {
	id := rowid.Int64()
	chunk, slot, ok, err := vi.locate(id)
	if err != nil || !ok {
		return err
	}
	var size int
	err = sqlitex.Execute(vi.conn, "SELECT size FROM "+vi.shadow("chunks")+" WHERE id = ?", &sqlitex.ExecOptions{
		Args: []any{chunk},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			size = stmt.ColumnInt(0)
			return nil
		},
	})
	if err != nil {
		return err
	}

	if last := size - 1; slot != last {
		vs := vi.vectorSize()
		movedID := make([]byte, 8)
		movedVec := make([]byte, vs)
		if err := vi.readAt("ids", chunk, last*8, movedID); err != nil {
			return err
		}
		if err := vi.readAt("vectors", chunk, last*vs, movedVec); err != nil {
			return err
		}
		if err := vi.writeAt("ids", chunk, slot*8, movedID); err != nil {
			return err
		}
		if err := vi.writeAt("vectors", chunk, slot*vs, movedVec); err != nil {
			return err
		}
		err := vi.exec("UPDATE "+vi.shadow("rowids")+" SET slot = ? WHERE rowid = ?", slot, int64(binary.LittleEndian.Uint64(movedID)))
		if err != nil {
			return err
		}
	}

	if size == 1 {
		if err = vi.exec("DELETE FROM "+vi.shadow("chunks")+" WHERE id = ?", chunk); err == nil {
			err = vi.exec("DELETE FROM "+vi.shadow("data")+" WHERE id = ?", chunk)
		}
	} else {
		err = vi.exec("UPDATE "+vi.shadow("chunks")+" SET size = size - 1 WHERE id = ?", chunk)
	}
	if err != nil {
		return err
	}
	return vi.exec("DELETE FROM "+vi.shadow("rowids")+" WHERE rowid = ?", id)
}

type indexResult struct {
	dist float64
	id   int64
}

// resultHeap is a max-heap on dist that keeps the k nearest results seen.
type resultHeap []indexResult

func (h *resultHeap) push(r indexResult, k int) {
	s := *h
	if len(s) < k {
		s = append(s, r)
		for i := len(s) - 1; i > 0; {
			p := (i - 1) / 2
			if s[p].dist >= s[i].dist {
				break
			}
			s[p], s[i] = s[i], s[p]
			i = p
		}
		*h = s
		return
	}
	if r.dist >= s[0].dist {
		return
	}
	s[0] = r
	for i := 0; ; {
		l, big := 2*i+1, i
		if l < len(s) && s[l].dist > s[big].dist {
			big = l
		}
		if l+1 < len(s) && s[l+1].dist > s[big].dist {
			big = l + 1
		}
		if big == i {
			break
		}
		s[i], s[big] = s[big], s[i]
		i = big
	}
}

type indexChunk struct {
	ids     []byte
	vectors []byte
}

type indexCursor struct {
	vi *vectorIndex

	// k-NN and rowid plans materialize results; the scan plan steps stmt.
	knn     bool
	k       int64
	results []indexResult
	pos     int

	stmt *sqlite.Stmt
	eof  bool
}

func (cur *indexCursor) Filter(id sqlite.IndexID, argv []sqlite.Value) error {
	if err := cur.Close(); err != nil {
		return err
	}
	cur.knn, cur.results, cur.pos = false, nil, 0
	switch id.Num {
	case indexPlanKNN:
		cur.knn = true
		return cur.search(id.String, argv)
	case indexPlanRowID:
		rowid := argv[0].Int64()
		if _, _, ok, err := cur.vi.locate(rowid); err != nil {
			return err
		} else if ok {
			cur.results = []indexResult{{id: rowid}}
		}
		return nil
	default:
		stmt, _, err := cur.vi.conn.PrepareTransient("SELECT rowid, chunk, slot FROM " + cur.vi.shadow("rowids") + " ORDER BY rowid")
		if err != nil {
			return err
		}
		cur.stmt = stmt
		return cur.Next()
	}
}

func (cur *indexCursor) search(mode string, argv []sqlite.Value) error {
	vi := cur.vi
	switch mode {
	case "k":
		cur.k = argv[1].Int64()
		if cur.k < 0 {
			return fmt.Errorf("vector_index: k must be non-negative, got %d", cur.k)
		}
	case "limit", "limit,offset":
		cur.k = argv[1].Int64()
		if cur.k < 0 {
			return fmt.Errorf("vector_index: search requires k = ? or a non-negative LIMIT")
		}
		if mode == "limit,offset" {
			cur.k += max(argv[2].Int64(), 0)
		}
	default:
		return fmt.Errorf("vector_index: search requires k = ? or LIMIT")
	}
	query, err := vi.encodeInput(argv[0], "MATCH query")
	if err != nil {
		return err
	}
	if cur.k == 0 {
		return nil
	}

	dist := l2Squared
	if vi.quantized {
		scale := float64(vi.cfg.quantMax-vi.cfg.quantMin) / 255
		dist = func(a, b []byte) float64 { return float64(l2SquaredInt8(a, b)) * scale * scale }
	}
	vs := vi.vectorSize()
	k := int(cur.k)

	workers := runtime.GOMAXPROCS(0)
	heaps := make([]resultHeap, workers)
	jobs := make(chan *indexChunk)
	pool := sync.Pool{New: func() any { return new(indexChunk) }}
	var wg sync.WaitGroup
	for w := range heaps {
		wg.Add(1)
		go func(h *resultHeap) {
			defer wg.Done()
			for c := range jobs {
				for i := 0; i < len(c.ids)/8; i++ {
					d := dist(c.vectors[i*vs:(i+1)*vs], query)
					h.push(indexResult{d, int64(binary.LittleEndian.Uint64(c.ids[i*8:]))}, k)
				}
				pool.Put(c)
			}
		}(&heaps[w])
	}
	err = sqlitex.Execute(vi.conn, "SELECT c.size, d.ids, d.vectors FROM "+vi.shadow("chunks")+" c JOIN "+vi.shadow("data")+" d ON d.id = c.id", &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			n := stmt.ColumnInt(0)
			c := pool.Get().(*indexChunk)
			c.ids = slices.Grow(c.ids[:0], n*8)[:n*8]
			c.vectors = slices.Grow(c.vectors[:0], n*vs)[:n*vs]
			stmt.ColumnBytes(1, c.ids)
			stmt.ColumnBytes(2, c.vectors)
			jobs <- c
			return nil
		},
	})
	close(jobs)
	wg.Wait()
	if err != nil {
		return err
	}

	var all resultHeap
	for _, h := range heaps {
		for _, r := range h {
			all.push(r, k)
		}
	}
	slices.SortFunc(all, func(a, b indexResult) int {
		switch {
		case a.dist < b.dist:
			return -1
		case a.dist > b.dist:
			return 1
		case a.id < b.id:
			return -1
		case a.id > b.id:
			return 1
		}
		return 0
	})
	cur.results = all
	return nil
}

func (cur *indexCursor) Next() error {
	if cur.stmt == nil {
		cur.pos++
		return nil
	}
	row, err := cur.stmt.Step()
	cur.eof = !row
	return err
}

func (cur *indexCursor) EOF() bool {
	if cur.stmt == nil {
		return cur.pos >= len(cur.results)
	}
	return cur.eof
}

func (cur *indexCursor) RowID() (int64, error) {
	if cur.stmt == nil {
		return cur.results[cur.pos].id, nil
	}
	return cur.stmt.ColumnInt64(0), nil
}

func (cur *indexCursor) Column(i int, noChange bool) (sqlite.Value, error) {
	switch i {
	case indexColEmbedding:
		if noChange {
			return sqlite.Unchanged(), nil
		}
		var chunk int64
		var slot int
		if cur.stmt != nil {
			chunk, slot = cur.stmt.ColumnInt64(1), cur.stmt.ColumnInt(2)
		} else {
			var err error
			if chunk, slot, _, err = cur.vi.locate(cur.results[cur.pos].id); err != nil {
				return sqlite.Value{}, err
			}
		}
		vec, err := cur.vi.readVector(chunk, slot)
		if err != nil {
			return sqlite.Value{}, err
		}
		return sqlite.BlobValue(vec), nil
	case indexColDistance:
		if cur.knn {
			return sqlite.FloatValue(cur.results[cur.pos].dist), nil
		}
	case indexColK:
		if cur.knn {
			return sqlite.IntegerValue(cur.k), nil
		}
	}
	return sqlite.Value{}, nil
}

func (cur *indexCursor) Close() error {
	if cur.stmt == nil {
		return nil
	}
	err := cur.stmt.Finalize()
	cur.stmt = nil
	return err
}
