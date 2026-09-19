package transfer

// 整理历史记录模块（对应 transfer_history.py）。使用 sqlite 存储。数据库文件：db/data/transfer.db

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const historyTable = "transfer_history"
const historyColumns = "id, file_id, file_name, source_pid, target_pid, target_path, media_title, media_year, media_type, tmdb_id, season, episode, file_size, version, status, error_msg, transfer_type, transfer_time"

// HistoryRecord 整理历史记录。
type HistoryRecord struct {
	ID           int
	FileID       string
	FileName     string
	SourcePID    int
	TargetPID    int
	TargetPath   string
	MediaTitle   string
	MediaYear    string
	MediaType    string
	TMDBID       sql.NullInt64
	Season       sql.NullInt64
	Episode      sql.NullInt64
	FileSize     int64
	Version      int
	Status       string
	ErrorMsg     string
	TransferType string
	TransferTime string
}

// TransferHistory 整理历史记录 CRUD。
type TransferHistory struct {
	DBPath string
	db     *sql.DB
}

// NewTransferHistory 创建历史记录管理器。
func NewTransferHistory(dbPath string) (*TransferHistory, error) {
	if dbPath == "" {
		dbPath = "data/transfer.db"
	}
	if err := os.MkdirAll(dirOf(dbPath), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite 单写
	// 设置 busy_timeout 避免 concurrent write 时报 "database is locked"
	_, _ = db.Exec("PRAGMA busy_timeout=5000")
	// WAL 模式允许并发读
	_, _ = db.Exec("PRAGMA journal_mode=WAL")
	h := &TransferHistory{DBPath: dbPath, db: db}
	if err := h.initTable(); err != nil {
		db.Close()
		return nil, err
	}
	return h, nil
}

// initTable 初始化表结构（含兼容旧库的列补全）。
func (h *TransferHistory) initTable() error {
	_, err := h.db.Exec(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		file_id TEXT NOT NULL UNIQUE,
		file_name TEXT,
		source_pid INTEGER,
		target_pid INTEGER,
		target_path TEXT,
		media_title TEXT,
		media_year TEXT,
		media_type TEXT,
		tmdb_id INTEGER,
		season INTEGER,
		episode INTEGER,
		file_size INTEGER DEFAULT 0,
		version INTEGER DEFAULT 0,
		status TEXT,
		error_msg TEXT,
		transfer_type TEXT,
		transfer_time TEXT
	)`, historyTable))
	if err != nil {
		return err
	}
	// 兼容已有数据库：补齐缺失列
	h.ensureColumn("file_size", "INTEGER DEFAULT 0")
	h.ensureColumn("version", "INTEGER DEFAULT 0")
	_, _ = h.db.Exec(fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_status ON %s(status)", historyTable, historyTable))
	_, _ = h.db.Exec(fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_transfer_time ON %s(transfer_time)", historyTable, historyTable))
	_, _ = h.db.Exec(fmt.Sprintf("CREATE INDEX IF NOT EXISTS idx_%s_episode ON %s(tmdb_id, season, episode)", historyTable, historyTable))
	return nil
}

func (h *TransferHistory) ensureColumn(col, ddl string) {
	rows, err := h.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", historyTable))
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			continue
		}
		if name == col {
			return
		}
	}
	_, _ = h.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", historyTable, col, ddl))
}

// Close 关闭数据库。
func (h *TransferHistory) Close() error {
	return h.db.Close()
}

// Begin 开启一个事务，供批量写入使用。
func (h *TransferHistory) Begin() (*sql.Tx, error) {
	return h.db.Begin()
}

// Exists 检查文件是否已整理过。
func (h *TransferHistory) Exists(fileID string) bool {
	var one int
	err := h.db.QueryRow(fmt.Sprintf("SELECT 1 FROM %s WHERE file_id=?", historyTable), fileID).Scan(&one)
	return err == nil
}

// GetByFileID 按 file_id 查询完整记录，返回 nil 表示不存在。
func (h *TransferHistory) GetByFileID(fileID string) *HistoryRecord {
	rows, err := h.db.Query(fmt.Sprintf(`SELECT %s FROM %s WHERE file_id=?`, historyColumns, historyTable), fileID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	records := scanRecords(rows)
	if len(records) > 0 {
		return records[0]
	}
	return nil
}

// Add 新增或更新历史记录。
func (h *TransferHistory) Add(rec HistoryRecord) error {
	now := time.Now().Format(time.RFC3339)
	if rec.FileSize == 0 && rec.FileID != "" {
		// 保持默认
	}
	_, err := h.db.Exec(fmt.Sprintf(`INSERT OR REPLACE INTO %s
		(file_id, file_name, source_pid, target_pid, target_path,
		 media_title, media_year, media_type, tmdb_id, season, episode,
		 status, error_msg, transfer_type, transfer_time, file_size, version)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, historyTable),
		rec.FileID, rec.FileName, rec.SourcePID, rec.TargetPID, rec.TargetPath,
		rec.MediaTitle, rec.MediaYear, rec.MediaType,
		nullInt64(rec.TMDBID), nullInt64(rec.Season), nullInt64(rec.Episode),
		rec.Status, rec.ErrorMsg, rec.TransferType, now,
		rec.FileSize, rec.Version)
	return err
}

// UpdateByID 按主键 id 原地更新一条记录。
// 用于手动重识别成功后，把原来的"识别失败"行直接改写为成功结果，避免残留双条记录。
func (h *TransferHistory) UpdateByID(id int, rec HistoryRecord) error {
	_, err := h.db.Exec(fmt.Sprintf(`UPDATE %s SET
		file_id=?, file_name=?, source_pid=?, target_pid=?, target_path=?,
		media_title=?, media_year=?, media_type=?, tmdb_id=?, season=?, episode=?,
		status=?, error_msg=?, transfer_type=?, transfer_time=?, file_size=?, version=?
		WHERE id=?`, historyTable),
		rec.FileID, rec.FileName, rec.SourcePID, rec.TargetPID, rec.TargetPath,
		rec.MediaTitle, rec.MediaYear, rec.MediaType,
		nullInt64(rec.TMDBID), nullInt64(rec.Season), nullInt64(rec.Episode),
		rec.Status, rec.ErrorMsg, rec.TransferType,
		time.Now().Format(time.RFC3339), rec.FileSize, rec.Version, id)
	return err
}

// FindSameEpisode 查询同一集已成功整理的历史记录。
// tmdb_id 优先；查不到时 fallback 到 media_title，以命中 catalog 写入的无 tmdb_id 旧记录。
func (h *TransferHistory) FindSameEpisode(tmdbID, season, episode int, mediaTitle string) []*HistoryRecord {
	if tmdbID > 0 {
		rows, err := h.db.Query(fmt.Sprintf(`SELECT %s FROM %s
			WHERE tmdb_id=? AND season=? AND episode=? AND status='success'
			ORDER BY version DESC, file_size DESC`, historyColumns, historyTable), tmdbID, season, episode)
		if err == nil {
			defer rows.Close()
			if records := scanRecords(rows); len(records) > 0 {
				return records
			}
		}
		// tmdb_id 查不到，fallback title
	}
	if mediaTitle != "" {
		rows, err := h.db.Query(fmt.Sprintf(`SELECT %s FROM %s
			WHERE media_title=? AND season=? AND episode=? AND status='success'
			ORDER BY version DESC, file_size DESC`, historyColumns, historyTable), mediaTitle, season, episode)
		if err != nil {
			return nil
		}
		defer rows.Close()
		return scanRecords(rows)
	}
	return nil
}

// FindSameMovie 查询同一部电影已成功整理的历史记录。
// tmdb_id 优先；查不到时 fallback 到 media_title[+year]，以命中 catalog 写入的无 tmdb_id 旧记录。
func (h *TransferHistory) FindSameMovie(tmdbID int, mediaTitle, mediaYear string) []*HistoryRecord {
	if tmdbID > 0 {
		rows, err := h.db.Query(fmt.Sprintf(`SELECT %s FROM %s
			WHERE tmdb_id=? AND media_type='movie' AND status='success'
			ORDER BY version DESC, file_size DESC`, historyColumns, historyTable), tmdbID)
		if err == nil {
			defer rows.Close()
			if records := scanRecords(rows); len(records) > 0 {
				return records
			}
		}
		// tmdb_id 查不到，fallback title
	}
	if mediaTitle != "" {
		sqlStr := fmt.Sprintf(`SELECT %s FROM %s
			WHERE media_title=? AND media_type='movie' AND status='success'`, historyColumns, historyTable)
		args := []any{mediaTitle}
		if mediaYear != "" {
			sqlStr += " AND media_year=?"
			args = append(args, mediaYear)
		}
		sqlStr += " ORDER BY version DESC, file_size DESC"
		rows, err := h.db.Query(sqlStr, args...)
		if err != nil {
			return nil
		}
		defer rows.Close()
		return scanRecords(rows)
	}
	return nil
}

// List 查询历史记录列表。
func (h *TransferHistory) List(limit, offset int, status, keyword string) []*HistoryRecord {
	sqlStr := fmt.Sprintf("SELECT %s FROM %s", historyColumns, historyTable)
	var where []string
	var args []any
	if status != "" {
		where = append(where, "status=?")
		args = append(args, status)
	}
	if keyword != "" {
		where = append(where, "(media_title LIKE ? OR file_name LIKE ? OR target_path LIKE ?)")
		kw := "%" + keyword + "%"
		args = append(args, kw, kw, kw)
	}
	if len(where) > 0 {
		sqlStr += " WHERE " + strings.Join(where, " AND ")
	}
	sqlStr += " ORDER BY transfer_time DESC LIMIT ? OFFSET ?"
	args = append(args, limit, offset)
	rows, err := h.db.Query(sqlStr, args...)
	if err != nil {
		log.Printf("[transfer_history] List 查询失败: %v", err)
		return nil
	}
	defer rows.Close()
	return scanRecords(rows)
}

// Count 统计记录数。
func (h *TransferHistory) Count(status string) int {
	sqlStr := fmt.Sprintf("SELECT COUNT(*) FROM %s", historyTable)
	args := []any{}
	if status != "" {
		sqlStr += " WHERE status=?"
		args = append(args, status)
	}
	var n int
	if err := h.db.QueryRow(sqlStr, args...).Scan(&n); err != nil {
		return 0
	}
	return n
}

// CountFilter 按状态 + 关键词统计记录数（与 List 的过滤条件保持一致，用于分页总数）。
func (h *TransferHistory) CountFilter(status, keyword string) int {
	sqlStr := fmt.Sprintf("SELECT COUNT(*) FROM %s", historyTable)
	var where []string
	var args []any
	if status != "" {
		where = append(where, "status=?")
		args = append(args, status)
	}
	if keyword != "" {
		where = append(where, "(media_title LIKE ? OR file_name LIKE ? OR target_path LIKE ?)")
		kw := "%" + keyword + "%"
		args = append(args, kw, kw, kw)
	}
	if len(where) > 0 {
		sqlStr += " WHERE " + strings.Join(where, " AND ")
	}
	var n int
	if err := h.db.QueryRow(sqlStr, args...).Scan(&n); err != nil {
		return 0
	}
	return n
}

// MediaStats 汇总成功整理记录中的媒体容量和媒体数量。
// 统计在 SQLite 内完成，只返回汇总值，避免把历史明细加载到内存。
type MediaStats struct {
	MovieCount int   `json:"movie_count"`
	MovieSize  int64 `json:"movie_size"`
	TVCount    int   `json:"tv_count"`
	TVSize     int64 `json:"tv_size"`
	TotalSize  int64 `json:"total_size"`
}

func (h *TransferHistory) MediaStats() MediaStats {
	var stats MediaStats
	err := h.db.QueryRow(fmt.Sprintf(`
		SELECT
			COUNT(DISTINCT CASE WHEN media_type='movie' AND media_title <> ''
				THEN media_title || '|' || COALESCE(media_year, '') END),
			COALESCE(SUM(CASE WHEN media_type='movie' THEN file_size ELSE 0 END), 0),
			COUNT(DISTINCT CASE WHEN media_type IN ('tv', 'series') AND media_title <> ''
				THEN media_title || '|' || COALESCE(media_year, '') END),
			COALESCE(SUM(CASE WHEN media_type IN ('tv', 'series') THEN file_size ELSE 0 END), 0)
		FROM %s
		WHERE status='success'`, historyTable)).Scan(
		&stats.MovieCount, &stats.MovieSize, &stats.TVCount, &stats.TVSize,
	)
	if err != nil {
		log.Printf("[transfer_history] MediaStats 查询失败: %v", err)
		return MediaStats{}
	}
	stats.TotalSize = stats.MovieSize + stats.TVSize
	return stats
}

// Delete 删除单条记录。
func (h *TransferHistory) Delete(historyID int) bool {
	res, err := h.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE id=?", historyTable), historyID)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// DeleteByFileID 按 file_id 删除记录。
func (h *TransferHistory) DeleteByFileID(fileID string) bool {
	res, err := h.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE file_id=?", historyTable), fileID)
	if err != nil {
		return false
	}
	n, _ := res.RowsAffected()
	return n > 0
}

// DeleteByMedia 按媒体信息删除整理历史。
func (h *TransferHistory) DeleteByMedia(title, year, mediaType string) int {
	sqlStr := fmt.Sprintf("DELETE FROM %s WHERE media_title LIKE ?", historyTable)
	args := []any{"%" + title + "%"}
	if mediaType != "" {
		sqlStr += " AND media_type=?"
		args = append(args, mediaType)
	}
	if year != "" {
		sqlStr += " AND media_year=?"
		args = append(args, year)
	}
	res, err := h.db.Exec(sqlStr, args...)
	if err != nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

// GetByIDs 按 ID 列表查询记录。
func (h *TransferHistory) GetByIDs(ids []int) []*HistoryRecord {
	if len(ids) == 0 {
		return nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := h.db.Query(fmt.Sprintf("SELECT %s FROM %s WHERE id IN (%s)", historyColumns, historyTable, strings.Join(placeholders, ",")), args...)
	if err != nil {
		log.Printf("[transfer_history] GetByIDs 查询失败: %v", err)
		return nil
	}
	defer rows.Close()
	return scanRecords(rows)
}

// DeleteBatch 按 ID 列表批量删除，返回实际删除条数。
func (h *TransferHistory) DeleteBatch(ids []int) int {
	if len(ids) == 0 {
		return 0
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	res, err := h.db.Exec(fmt.Sprintf("DELETE FROM %s WHERE id IN (%s)", historyTable, strings.Join(placeholders, ",")), args...)
	if err != nil {
		log.Printf("[transfer_history] DeleteBatch 失败: %v", err)
		return 0
	}
	n, _ := res.RowsAffected()
	return int(n)
}

func scanRecords(rows *sql.Rows) []*HistoryRecord {
	var out []*HistoryRecord
	for rows.Next() {
		var r HistoryRecord
		var tmdbID, season, episode sql.NullInt64
		err := rows.Scan(
			&r.ID, &r.FileID, &r.FileName, &r.SourcePID, &r.TargetPID, &r.TargetPath,
			&r.MediaTitle, &r.MediaYear, &r.MediaType, &tmdbID, &season, &episode,
			&r.FileSize, &r.Version, &r.Status, &r.ErrorMsg, &r.TransferType, &r.TransferTime,
		)
		if err != nil {
			log.Printf("[transfer_history] scanRecords 行扫描失败: %v", err)
			continue
		}
		r.TMDBID = tmdbID
		r.Season = season
		r.Episode = episode
		out = append(out, &r)
	}
	return out
}

func nullInt64(n sql.NullInt64) any {
	if !n.Valid {
		return nil
	}
	return n.Int64
}

func intOr0(n sql.NullInt64) int {
	if !n.Valid {
		return 0
	}
	return int(n.Int64)
}

var _ = strconv.Itoa
