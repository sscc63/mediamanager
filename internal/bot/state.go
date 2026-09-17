package bot

// 用户状态管理（对应 UserStateManager：SELECTING_FILE / CONFIRM_DELETE / ASK_POST 等会话状态）。

import (
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// applySQLiteLimits 统一收敛 sqlite 连接池。
// modernc/sqlite 是纯 Go 实现，每条连接都带独立的 page cache，
// 连接数不设上限（database/sql 默认 0 = 无限）会让内存随并发无界增长。
// 注意：带 per-connection PRAGMA（如 busy_timeout / WAL）的库不要设 ConnMaxIdleTime，
// 否则连接被回收后新连接不会带上那些 PRAGMA。
func applySQLiteLimits(db *sql.DB) {
	if db == nil {
		return
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(time.Hour)
}

// UserState 用户会话状态。
type UserState struct {
	State string
	Data  string
}

// UserStateManager 基于 SQLite 的用户状态管理器。
type UserStateManager struct {
	dbPath string
	db     *sql.DB
	mu     sync.Mutex
}

// NewUserStateManager 创建状态管理器并初始化表。
func NewUserStateManager(dbPath string) *UserStateManager {
	if dbPath == "" {
		dbPath = "data/user_states.db"
	}
	if dir := filepath.Dir(dbPath); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	m := &UserStateManager{dbPath: dbPath}
	m.init()
	return m
}

func (m *UserStateManager) init() {
	db, err := sql.Open("sqlite", m.dbPath)
	if err != nil {
		return
	}
	applySQLiteLimits(db)
	m.db = db
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS user_states (
		user_id INTEGER PRIMARY KEY,
		state TEXT,
		data TEXT)`)
}

// dbConn 惰性取连接。加锁是必要的：并发首次调用会各开一个 *sql.DB，
// 其中一个随即失去引用，其 connectionOpener goroutine 会把它永久留在内存里。
func (m *UserStateManager) dbConn() *sql.DB {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.db != nil {
		return m.db
	}
	db, err := sql.Open("sqlite", m.dbPath)
	if err != nil {
		return nil
	}
	applySQLiteLimits(db)
	m.db = db
	return db
}

// SetState 保存用户状态。
func (m *UserStateManager) SetState(userID int64, state, data string) {
	db := m.dbConn()
	if db == nil {
		return
	}
	_, _ = db.Exec("INSERT OR REPLACE INTO user_states VALUES (?, ?, ?)", userID, state, data)
}

// GetState 获取用户状态。
func (m *UserStateManager) GetState(userID int64) (string, string) {
	db := m.dbConn()
	if db == nil {
		return "", ""
	}
	var state, data string
	err := db.QueryRow("SELECT state, data FROM user_states WHERE user_id=?", userID).Scan(&state, &data)
	if err != nil {
		return "", ""
	}
	return state, data
}

// ClearState 清除用户状态。
func (m *UserStateManager) ClearState(userID int64) {
	db := m.dbConn()
	if db == nil {
		return
	}
	_, _ = db.Exec("DELETE FROM user_states WHERE user_id=?", userID)
}

// Close 关闭数据库。
func (m *UserStateManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.db != nil {
		_ = m.db.Close()
		m.db = nil
	}
}
