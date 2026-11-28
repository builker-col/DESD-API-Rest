package db

import (
	"context"
	"fmt"
	"math/rand"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

const (
	RoleAdmin     = "admin"
	RoleEncargado = "encargado"
	RoleUser      = "user"
)

func IsValidRole(role string) bool {
	switch role {
	case RoleAdmin, RoleEncargado, RoleUser:
		return true
	default:
		return false
	}
}

type User struct {
	ID              int       `json:"id"`
	Username        string    `json:"username"`
	Password        string    `json:"-"`
	Role            string    `json:"role"`
	IsAdmin         bool      `json:"is_admin"`
	IsActive        bool      `json:"is_active"`
	AssignedUserIDs []int     `json:"assigned_user_ids,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func (u *User) EffectiveRole() string {
	if u == nil {
		return RoleUser
	}
	if u.IsAdmin || u.Role == RoleAdmin {
		return RoleAdmin
	}
	if u.Role == RoleEncargado {
		return RoleEncargado
	}
	return RoleUser
}

func (u *User) IsRoleAdmin() bool {
	return u.EffectiveRole() == RoleAdmin
}

func (u *User) IsRoleEncargado() bool {
	return u.EffectiveRole() == RoleEncargado
}

type ResultStatus struct {
	ID                  int       `json:"id"`
	ResultID            int       `json:"result_id"`
	Status              string    `json:"status"`
	TotalFiles          *int      `json:"total_files"`
	TotalFilesProcessed *int      `json:"total_files_processed"`
	TotalItemsExpected  *int      `json:"total_items_expected,omitempty"`
	TotalItemsProcessed int       `json:"total_items_processed"`
	Models              *string   `json:"models"`
	CurrentFile         *string   `json:"current_file"`
	LastUpdated         time.Time `json:"last_updated"`
}

type Result struct {
	ID           int           `json:"id"`
	CollectionID string        `json:"collection_id"`
	UserID       int           `json:"user_id"`
	Username     string        `json:"username,omitempty"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
	Status       *ResultStatus `json:"status"`
}

type AllowedIP struct {
	ID        int       `json:"id"`
	IP        string    `json:"ip"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type ResultFile struct {
	ID        int       `json:"id"`
	ResultID  int       `json:"result_id"`
	TaskID    string    `json:"task_id"`
	Filename  string    `json:"filename"`
	FilePath  string    `json:"file_path"`
	Source    string    `json:"source"`
	Pages     int       `json:"pages"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

type FileReportRow struct {
	UserID      int
	Username    string
	UserRole    string
	TaskID      string
	Filename    string
	FilePath    string
	Source      string
	Models      string
	Status      string
	Pages       int
	SizeBytes   int64
	AlertsCount int
	ProcessedAt time.Time
}

type Store struct {
	Pool *pgxpool.Pool

	ipMu        sync.RWMutex
	ipExpiresAt time.Time
	exactIPs    map[string]bool
	cidrNets    []netip.Prefix
}

func New(ctx context.Context, connStr string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 16
	cfg.MinConns = 2

	var pool *pgxpool.Pool
	for attempt := 1; attempt <= 30; attempt++ {
		pool, err = pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			if pingErr := pool.Ping(ctx); pingErr == nil {
				s := &Store{Pool: pool}
				if schemaErr := s.EnsureSchema(ctx); schemaErr != nil {
					pool.Close()
					return nil, schemaErr
				}
				return s, nil
			} else {
				pool.Close()
				err = pingErr
			}
		}
		fmt.Printf("[api] Esperando PostgreSQL (intento %d/30): %v\n", attempt, err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("no se pudo conectar a PostgreSQL: %w", err)
}

func (s *Store) Ping(ctx context.Context) error {
	return s.Pool.Ping(ctx)
}

func (s *Store) Close() {
	if s.Pool != nil {
		s.Pool.Close()
	}
}

func (s *Store) EnsureSchema(ctx context.Context) error {
	stmts := []string{
		`DO $$ BEGIN
			CREATE TYPE resultstatusenum AS ENUM ('UPLOADING', 'PENDING', 'RUNNING', 'COMPLETED', 'FAILED');
		EXCEPTION WHEN duplicate_object THEN null;
		END $$;`,
		`CREATE TABLE IF NOT EXISTS "user" (
			id SERIAL PRIMARY KEY,
			username VARCHAR(250) NOT NULL UNIQUE,
			password VARCHAR(250) NOT NULL,
			role VARCHAR(50) NOT NULL DEFAULT 'user',
			is_admin BOOLEAN NOT NULL DEFAULT FALSE,
			is_active BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMP WITHOUT TIME ZONE NOT NULL DEFAULT NOW()
		);`,
		`ALTER TABLE "user"
			ADD COLUMN IF NOT EXISTS role VARCHAR(50) NOT NULL DEFAULT 'user';`,
		`UPDATE "user" SET role = 'admin' WHERE is_admin = TRUE AND role <> 'admin';`,
		`UPDATE "user" SET is_admin = TRUE WHERE role = 'admin' AND is_admin = FALSE;`,
		`CREATE INDEX IF NOT EXISTS ix_user_username ON "user" (username);`,
		`CREATE INDEX IF NOT EXISTS ix_user_role ON "user" (role);`,
		`CREATE TABLE IF NOT EXISTS encargado_users (
			encargado_id INTEGER NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
			user_id INTEGER NOT NULL REFERENCES "user"(id) ON DELETE CASCADE,
			created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL DEFAULT NOW(),
			PRIMARY KEY (encargado_id, user_id),
			CHECK (encargado_id <> user_id)
		);`,
		`CREATE INDEX IF NOT EXISTS ix_encargado_users_encargado_id ON encargado_users (encargado_id);`,
		`CREATE INDEX IF NOT EXISTS ix_encargado_users_user_id ON encargado_users (user_id);`,
		`CREATE TABLE IF NOT EXISTS allowed_ips (
			id SERIAL PRIMARY KEY,
			ip VARCHAR(250) NOT NULL UNIQUE,
			is_active BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMP WITHOUT TIME ZONE NOT NULL DEFAULT NOW()
		);`,
		`CREATE TABLE IF NOT EXISTS result (
			id SERIAL PRIMARY KEY,
			collection_id VARCHAR(250) NOT NULL,
			user_id INTEGER NOT NULL REFERENCES "user"(id),
			created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMP WITHOUT TIME ZONE NOT NULL DEFAULT NOW()
		);`,
		`CREATE INDEX IF NOT EXISTS ix_result_collection_id ON result (collection_id);`,
		`CREATE INDEX IF NOT EXISTS ix_result_user_id ON result (user_id);`,
		`CREATE TABLE IF NOT EXISTS result_status (
			id SERIAL PRIMARY KEY,
			result_id INTEGER NOT NULL REFERENCES result(id),
			status resultstatusenum NOT NULL DEFAULT 'UPLOADING',
			total_files INTEGER,
			total_files_processed INTEGER DEFAULT 0,
			total_items_expected INTEGER,
			total_items_processed INTEGER NOT NULL DEFAULT 0,
			models VARCHAR(250),
			current_file VARCHAR(250),
			last_updated TIMESTAMP WITHOUT TIME ZONE NOT NULL DEFAULT NOW()
		);`,
		`ALTER TABLE result_status
			ADD COLUMN IF NOT EXISTS total_items_expected INTEGER,
			ADD COLUMN IF NOT EXISTS total_items_processed INTEGER NOT NULL DEFAULT 0;`,
		`CREATE TABLE IF NOT EXISTS result_item (
			id SERIAL PRIMARY KEY,
			result_id INTEGER REFERENCES result(id) ON DELETE CASCADE,
			task_id VARCHAR(250) NOT NULL,
			data JSONB NOT NULL,
			created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL DEFAULT NOW()
		);`,
		`CREATE INDEX IF NOT EXISTS ix_result_item_result_id ON result_item (result_id);`,
		`CREATE INDEX IF NOT EXISTS ix_result_item_task_id ON result_item (task_id);`,
		`CREATE TABLE IF NOT EXISTS result_file (
			id SERIAL PRIMARY KEY,
			result_id INTEGER NOT NULL REFERENCES result(id) ON DELETE CASCADE,
			task_id VARCHAR(250) NOT NULL,
			filename VARCHAR(500) NOT NULL,
			file_path TEXT NOT NULL DEFAULT '',
			source VARCHAR(50) NOT NULL DEFAULT 'api',
			pages INTEGER NOT NULL DEFAULT 0,
			size_bytes BIGINT NOT NULL DEFAULT 0,
			created_at TIMESTAMP WITHOUT TIME ZONE NOT NULL DEFAULT NOW()
		);`,
		`CREATE INDEX IF NOT EXISTS ix_result_file_result_id ON result_file (result_id);`,
		`CREATE INDEX IF NOT EXISTS ix_result_file_task_id ON result_file (task_id);`,
	}

	for _, stmt := range stmts {
		if _, err := s.Pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("error ejecutando migración: %w", err)
		}
	}

	var userCount int
	if err := s.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM "user"`).Scan(&userCount); err == nil && userCount == 0 {
		hash, err := bcrypt.GenerateFromPassword([]byte("admin"), 10)
		if err == nil {
			_, _ = s.Pool.Exec(ctx, `
				INSERT INTO "user" (username, password, role, is_admin, is_active, created_at, updated_at)
				VALUES ('admin', $1, 'admin', TRUE, TRUE, NOW(), NOW())
				ON CONFLICT (username) DO NOTHING
			`, string(hash))
			fmt.Println("[api] Usuario inicial 'admin' creado automáticamente.")
		}
	}

	for _, ip := range []string{"127.0.0.1", "::1", "172.16.0.0/12", "192.168.0.0/16", "10.0.0.0/8"} {
		_, _ = s.Pool.Exec(ctx, `
			INSERT INTO allowed_ips (ip, is_active, created_at, updated_at)
			VALUES ($1, TRUE, NOW(), NOW())
			ON CONFLICT (ip) DO NOTHING
		`, ip)
	}

	return nil
}

// --- IP Allowlist Cache ---

func (s *Store) InvalidateIPCache() {
	s.ipMu.Lock()
	s.ipExpiresAt = time.Time{}
	s.ipMu.Unlock()
}

func (s *Store) IsIPAllowed(ctx context.Context, rawIP string) (bool, error) {
	ipStr := strings.TrimSpace(rawIP)
	if ipStr == "" {
		return false, nil
	}

	s.ipMu.RLock()
	valid := time.Now().Before(s.ipExpiresAt)
	exact := s.exactIPs
	nets := s.cidrNets
	s.ipMu.RUnlock()

	if !valid {
		if err := s.refreshIPCache(ctx); err != nil {
			return false, err
		}
		s.ipMu.RLock()
		exact = s.exactIPs
		nets = s.cidrNets
		s.ipMu.RUnlock()
	}

	if exact[ipStr] {
		return true, nil
	}
	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		return false, nil
	}
	addr = addr.Unmap()
	if exact[addr.String()] {
		return true, nil
	}
	for _, pfx := range nets {
		if pfx.Contains(addr) {
			return true, nil
		}
	}
	return false, nil
}

func (s *Store) refreshIPCache(ctx context.Context) error {
	rows, err := s.Pool.Query(ctx, `SELECT ip FROM allowed_ips WHERE is_active = TRUE`)
	if err != nil {
		return err
	}
	defer rows.Close()

	exact := make(map[string]bool)
	var nets []netip.Prefix
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			continue
		}
		ip = strings.TrimSpace(ip)
		if strings.Contains(ip, "/") {
			if pfx, err := netip.ParsePrefix(ip); err == nil {
				nets = append(nets, pfx)
			}
		} else {
			exact[ip] = true
		}
	}

	s.ipMu.Lock()
	s.exactIPs = exact
	s.cidrNets = nets
	s.ipExpiresAt = time.Now().Add(15 * time.Second)
	s.ipMu.Unlock()
	return rows.Err()
}

// --- Users & Roles ---

func (s *Store) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	var u User
	err := s.Pool.QueryRow(ctx, `
		SELECT id, username, password, role, is_admin, is_active, created_at, updated_at
		FROM "user" WHERE username = $1 LIMIT 1
	`, username).Scan(&u.ID, &u.Username, &u.Password, &u.Role, &u.IsAdmin, &u.IsActive, &u.CreatedAt, &u.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.Role = u.EffectiveRole()
	u.IsAdmin = u.Role == RoleAdmin
	if u.IsRoleEncargado() {
		if ids, err := s.GetAssignedUserIDs(ctx, u.ID); err == nil {
			u.AssignedUserIDs = ids
		}
	}
	return &u, nil
}

func (s *Store) GetUserByID(ctx context.Context, id int) (*User, error) {
	var u User
	err := s.Pool.QueryRow(ctx, `
		SELECT id, username, password, role, is_admin, is_active, created_at, updated_at
		FROM "user" WHERE id = $1 LIMIT 1
	`, id).Scan(&u.ID, &u.Username, &u.Password, &u.Role, &u.IsAdmin, &u.IsActive, &u.CreatedAt, &u.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.Role = u.EffectiveRole()
	u.IsAdmin = u.Role == RoleAdmin
	if u.IsRoleEncargado() {
		if ids, err := s.GetAssignedUserIDs(ctx, u.ID); err == nil {
			u.AssignedUserIDs = ids
		}
	}
	return &u, nil
}

func (s *Store) ResolveUserID(ctx context.Context, username string) (int, error) {
	u, err := s.GetUserByUsername(ctx, username)
	if err != nil {
		return 0, err
	}
	if u != nil {
		return u.ID, nil
	}
	var id int
	err = s.Pool.QueryRow(ctx, `SELECT id FROM "user" ORDER BY id ASC LIMIT 1`).Scan(&id)
	return id, err
}

func (s *Store) ListUsers(ctx context.Context, roleFilter string) ([]User, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if roleFilter != "" {
		rows, err = s.Pool.Query(ctx, `
			SELECT id, username, password, role, is_admin, is_active, created_at, updated_at
			FROM "user" WHERE role = $1 ORDER BY id ASC
		`, roleFilter)
	} else {
		rows, err = s.Pool.Query(ctx, `
			SELECT id, username, password, role, is_admin, is_active, created_at, updated_at
			FROM "user" ORDER BY id ASC
		`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	users := make([]User, 0)
	encargadoIdx := make(map[int]int)
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Password, &u.Role, &u.IsAdmin, &u.IsActive, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		u.Role = u.EffectiveRole()
		u.IsAdmin = u.Role == RoleAdmin
		if u.IsRoleEncargado() {
			encargadoIdx[u.ID] = len(users)
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(encargadoIdx) > 0 {
		encIDs := make([]int, 0, len(encargadoIdx))
		for id := range encargadoIdx {
			encIDs = append(encIDs, id)
		}
		aRows, err := s.Pool.Query(ctx, `
			SELECT encargado_id, user_id
			FROM encargado_users
			WHERE encargado_id = ANY($1)
			ORDER BY user_id ASC
		`, encIDs)
		if err == nil {
			defer aRows.Close()
			for aRows.Next() {
				var encID, uid int
				if err := aRows.Scan(&encID, &uid); err == nil {
					if idx, ok := encargadoIdx[encID]; ok {
						users[idx].AssignedUserIDs = append(users[idx].AssignedUserIDs, uid)
					}
				}
			}
		}
	}

	return users, nil
}

func (s *Store) CreateUser(ctx context.Context, username, rawPassword, role string, isActive bool) (*User, error) {
	if role == "" {
		role = RoleUser
	}
	if !IsValidRole(role) {
		return nil, fmt.Errorf("invalid role: %s", role)
	}
	isAdmin := role == RoleAdmin

	hash, err := bcrypt.GenerateFromPassword([]byte(rawPassword), 10)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var u User
	err = s.Pool.QueryRow(ctx, `
		INSERT INTO "user" (username, password, role, is_admin, is_active, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $6)
		RETURNING id, username, password, role, is_admin, is_active, created_at, updated_at
	`, username, string(hash), role, isAdmin, isActive, now).Scan(
		&u.ID, &u.Username, &u.Password, &u.Role, &u.IsAdmin, &u.IsActive, &u.CreatedAt, &u.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *Store) UpdateUser(ctx context.Context, id int, username, rawPassword, role *string, isActive *bool) (*User, error) {
	existing, err := s.GetUserByID(ctx, id)
	if err != nil || existing == nil {
		return nil, err
	}
	prevRole := existing.EffectiveRole()
	if username != nil {
		existing.Username = *username
	}
	if rawPassword != nil && *rawPassword != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(*rawPassword), 10)
		if err != nil {
			return nil, err
		}
		existing.Password = string(hash)
	}
	if role != nil {
		if !IsValidRole(*role) {
			return nil, fmt.Errorf("invalid role: %s", *role)
		}
		existing.Role = *role
		existing.IsAdmin = *role == RoleAdmin
	}
	if isActive != nil {
		existing.IsActive = *isActive
	}
	existing.UpdatedAt = time.Now()

	err = s.Pool.QueryRow(ctx, `
		UPDATE "user"
		SET username = $1, password = $2, role = $3, is_admin = $4, is_active = $5, updated_at = $6
		WHERE id = $7
		RETURNING id, username, password, role, is_admin, is_active, created_at, updated_at
	`, existing.Username, existing.Password, existing.Role, existing.IsAdmin, existing.IsActive, existing.UpdatedAt, id).Scan(
		&existing.ID, &existing.Username, &existing.Password, &existing.Role, &existing.IsAdmin, &existing.IsActive, &existing.CreatedAt, &existing.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if prevRole == RoleEncargado && existing.Role != RoleEncargado {
		_, _ = s.Pool.Exec(ctx, `DELETE FROM encargado_users WHERE encargado_id = $1`, id)
		existing.AssignedUserIDs = nil
	} else if existing.IsRoleEncargado() {
		if ids, err := s.GetAssignedUserIDs(ctx, id); err == nil {
			existing.AssignedUserIDs = ids
		}
	}
	return existing, nil
}

func (s *Store) DeleteUser(ctx context.Context, id int) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, id)
	return tag.RowsAffected() > 0, err
}

// --- Encargado <-> Users Assignments ---

func (s *Store) GetAssignedUserIDs(ctx context.Context, encargadoID int) ([]int, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT user_id FROM encargado_users WHERE encargado_id = $1 ORDER BY user_id ASC
	`, encargadoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int, 0)
	for rows.Next() {
		var uid int
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		ids = append(ids, uid)
	}
	return ids, rows.Err()
}

func (s *Store) ListAssignedUsers(ctx context.Context, encargadoID int) ([]User, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT u.id, u.username, u.password, u.role, u.is_admin, u.is_active, u.created_at, u.updated_at
		FROM encargado_users eu
		JOIN "user" u ON u.id = eu.user_id
		WHERE eu.encargado_id = $1
		ORDER BY u.id ASC
	`, encargadoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Password, &u.Role, &u.IsAdmin, &u.IsActive, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, err
		}
		u.Role = u.EffectiveRole()
		u.IsAdmin = u.Role == RoleAdmin
		users = append(users, u)
	}
	return users, rows.Err()
}

func (s *Store) SetAssignedUsers(ctx context.Context, encargadoID int, userIDs []int) ([]User, error) {
	seen := make(map[int]bool, len(userIDs))
	unique := make([]int, 0, len(userIDs))
	for _, uid := range userIDs {
		if uid == encargadoID {
			return nil, fmt.Errorf("encargado cannot be assigned to themselves")
		}
		if uid <= 0 {
			return nil, fmt.Errorf("invalid user_id: %d", uid)
		}
		if !seen[uid] {
			seen[uid] = true
			unique = append(unique, uid)
		}
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if len(unique) > 0 {
		var existingCount int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM "user" WHERE id = ANY($1)`, unique).Scan(&existingCount); err != nil {
			return nil, err
		}
		if existingCount != len(unique) {
			return nil, fmt.Errorf("one or more assigned user_ids do not exist")
		}
	}

	if _, err := tx.Exec(ctx, `DELETE FROM encargado_users WHERE encargado_id = $1`, encargadoID); err != nil {
		return nil, err
	}
	for _, uid := range unique {
		if _, err := tx.Exec(ctx, `
			INSERT INTO encargado_users (encargado_id, user_id, created_at)
			VALUES ($1, $2, NOW())
			ON CONFLICT (encargado_id, user_id) DO NOTHING
		`, encargadoID, uid); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.ListAssignedUsers(ctx, encargadoID)
}

func (s *Store) AssignUserToEncargado(ctx context.Context, encargadoID, userID int) error {
	if encargadoID == userID {
		return fmt.Errorf("encargado cannot be assigned to themselves")
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO encargado_users (encargado_id, user_id, created_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (encargado_id, user_id) DO NOTHING
	`, encargadoID, userID)
	return err
}

func (s *Store) UnassignUserFromEncargado(ctx context.Context, encargadoID, userID int) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		DELETE FROM encargado_users WHERE encargado_id = $1 AND user_id = $2
	`, encargadoID, userID)
	return tag.RowsAffected() > 0, err
}

func (s *Store) IsUserAssignedToEncargado(ctx context.Context, encargadoID, userID int) (bool, error) {
	var exists bool
	err := s.Pool.QueryRow(ctx, `
		SELECT EXISTS(SELECT 1 FROM encargado_users WHERE encargado_id = $1 AND user_id = $2)
	`, encargadoID, userID).Scan(&exists)
	return exists, err
}

func (s *Store) CanUserAccessOwner(ctx context.Context, viewer *User, ownerUserID int) (bool, error) {
	if viewer == nil {
		return false, nil
	}
	if viewer.IsRoleAdmin() {
		return true, nil
	}
	if viewer.ID == ownerUserID {
		return true, nil
	}
	if viewer.IsRoleEncargado() {
		return s.IsUserAssignedToEncargado(ctx, viewer.ID, ownerUserID)
	}
	return false, nil
}

// --- Allowed IPs ---

func (s *Store) ListAllowedIPs(ctx context.Context) ([]AllowedIP, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, ip, is_active, created_at, updated_at FROM allowed_ips ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AllowedIP, 0)
	for rows.Next() {
		var a AllowedIP
		if err := rows.Scan(&a.ID, &a.IP, &a.IsActive, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetAllowedIP(ctx context.Context, id int) (*AllowedIP, error) {
	var a AllowedIP
	err := s.Pool.QueryRow(ctx, `SELECT id, ip, is_active, created_at, updated_at FROM allowed_ips WHERE id = $1`, id).
		Scan(&a.ID, &a.IP, &a.IsActive, &a.CreatedAt, &a.UpdatedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return &a, err
}

func (s *Store) CreateAllowedIP(ctx context.Context, ip string, isActive bool) (*AllowedIP, error) {
	now := time.Now()
	var a AllowedIP
	err := s.Pool.QueryRow(ctx, `
		INSERT INTO allowed_ips (ip, is_active, created_at, updated_at)
		VALUES ($1, $2, $3, $3)
		RETURNING id, ip, is_active, created_at, updated_at
	`, ip, isActive, now).Scan(&a.ID, &a.IP, &a.IsActive, &a.CreatedAt, &a.UpdatedAt)
	if err == nil {
		s.InvalidateIPCache()
	}
	return &a, err
}

func (s *Store) UpdateAllowedIP(ctx context.Context, id int, ip *string, isActive *bool) (*AllowedIP, error) {
	existing, err := s.GetAllowedIP(ctx, id)
	if err != nil || existing == nil {
		return nil, err
	}
	if ip != nil {
		existing.IP = *ip
	}
	if isActive != nil {
		existing.IsActive = *isActive
	}
	existing.UpdatedAt = time.Now()
	err = s.Pool.QueryRow(ctx, `
		UPDATE allowed_ips SET ip = $1, is_active = $2, updated_at = $3
		WHERE id = $4
		RETURNING id, ip, is_active, created_at, updated_at
	`, existing.IP, existing.IsActive, existing.UpdatedAt, id).Scan(&existing.ID, &existing.IP, &existing.IsActive, &existing.CreatedAt, &existing.UpdatedAt)
	if err == nil {
		s.InvalidateIPCache()
	}
	return existing, err
}

func (s *Store) DeleteAllowedIP(ctx context.Context, id int) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM allowed_ips WHERE id = $1`, id)
	if err == nil {
		s.InvalidateIPCache()
	}
	return tag.RowsAffected() > 0, err
}

// --- Tasks ---

func (s *Store) TaskExists(ctx context.Context, taskID string) (bool, error) {
	var id int
	err := s.Pool.QueryRow(ctx, `SELECT id FROM result WHERE collection_id = $1 LIMIT 1`, taskID).Scan(&id)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) AllocateUniqueTaskID(ctx context.Context, requested string) (string, error) {
	candidate := strings.TrimSpace(requested)
	if candidate == "" {
		candidate = fmt.Sprintf("task_%s_%04d", time.Now().Format("20060102_150405"), rand.Intn(10000))
	}
	exists, err := s.TaskExists(ctx, candidate)
	if err != nil || !exists {
		return candidate, err
	}
	base := candidate
	for i := 0; i < 20; i++ {
		candidate = fmt.Sprintf("%s_%s_%03d", base, time.Now().Format("150405"), rand.Intn(1000))
		exists, err = s.TaskExists(ctx, candidate)
		if err != nil || !exists {
			return candidate, err
		}
	}
	return fmt.Sprintf("%s_%d", base, time.Now().UnixNano()), nil
}

func (s *Store) CreateTask(ctx context.Context, taskID string, userID int, totalFiles int, modelsCSV string) (int, error) {
	now := time.Now()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var resultID int
	err = tx.QueryRow(ctx, `
		INSERT INTO result (collection_id, user_id, created_at, updated_at)
		VALUES ($1, $2, $3, $3)
		RETURNING id
	`, taskID, userID, now).Scan(&resultID)
	if err != nil {
		return 0, err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO result_status (
			result_id, status, total_files, total_files_processed,
			total_items_expected, total_items_processed, models, last_updated
		)
		VALUES ($1, 'UPLOADING'::resultstatusenum, $2, 0, NULL, 0, $3, $4)
	`, resultID, totalFiles, modelsCSV, now)
	if err != nil {
		return 0, err
	}

	return resultID, tx.Commit(ctx)
}

func (s *Store) FinalizeIngestion(ctx context.Context, resultID int, totalFiles int, expectedItems int) (string, error) {
	var status string
	err := s.Pool.QueryRow(ctx, `
		UPDATE result_status
		SET total_files = $1,
		    total_items_expected = $2,
		    status = CASE
		        WHEN $2 > 0 AND COALESCE(total_items_processed, 0) >= $2
		            THEN 'COMPLETED'::resultstatusenum
		        WHEN COALESCE(total_items_processed, 0) > 0
		            THEN 'RUNNING'::resultstatusenum
		        ELSE 'PENDING'::resultstatusenum
		    END,
		    last_updated = $3
		WHERE result_id = $4
		RETURNING status::text
	`, totalFiles, expectedItems, time.Now(), resultID).Scan(&status)
	return status, err
}

func (s *Store) SetTaskStatus(ctx context.Context, resultID int, status string, totalFiles int) error {
	_, err := s.Pool.Exec(ctx, `
		UPDATE result_status
		SET status = $1::resultstatusenum, total_files = $2, last_updated = $3
		WHERE result_id = $4
	`, status, totalFiles, time.Now(), resultID)
	return err
}

func (s *Store) GetTaskStatusString(ctx context.Context, taskID string) (string, error) {
	var status string
	err := s.Pool.QueryRow(ctx, `
		SELECT rs.status::text
		FROM result_status rs
		JOIN result r ON r.id = rs.result_id
		WHERE r.collection_id = $1
		LIMIT 1
	`, taskID).Scan(&status)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	return status, err
}

func (s *Store) GetTaskByCollectionID(ctx context.Context, taskID string) (*Result, error) {
	var r Result
	var username *string
	var rsID, rsResultID, rsItemsProc *int
	var rsStatus *string
	var rsTotalFiles, rsTotalFilesProc, rsItemsExp *int
	var rsModels, rsCurrFile *string
	var rsLastUpd *time.Time

	err := s.Pool.QueryRow(ctx, `
		SELECT r.id, r.collection_id, r.user_id, u.username, r.created_at, r.updated_at,
		       rs.id, rs.result_id, rs.status::text, rs.total_files, rs.total_files_processed,
		       rs.total_items_expected, rs.total_items_processed, rs.models, rs.current_file, rs.last_updated
		FROM result r
		LEFT JOIN "user" u ON u.id = r.user_id
		LEFT JOIN result_status rs ON rs.result_id = r.id
		WHERE r.collection_id = $1
		LIMIT 1
	`, taskID).Scan(
		&r.ID, &r.CollectionID, &r.UserID, &username, &r.CreatedAt, &r.UpdatedAt,
		&rsID, &rsResultID, &rsStatus, &rsTotalFiles, &rsTotalFilesProc,
		&rsItemsExp, &rsItemsProc, &rsModels, &rsCurrFile, &rsLastUpd,
	)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if username != nil {
		r.Username = *username
	}
	if rsID != nil && rsStatus != nil {
		st := &ResultStatus{
			ID:                  *rsID,
			ResultID:            *rsResultID,
			Status:              *rsStatus,
			TotalFiles:          rsTotalFiles,
			TotalFilesProcessed: rsTotalFilesProc,
			TotalItemsExpected:  rsItemsExp,
			Models:              rsModels,
			CurrentFile:         rsCurrFile,
		}
		if rsItemsProc != nil {
			st.TotalItemsProcessed = *rsItemsProc
		}
		if rsLastUpd != nil {
			st.LastUpdated = *rsLastUpd
		}
		r.Status = st
	}
	return &r, nil
}

// ListTasks lista tareas filtrando por allowedUserIDs:
//   - nil: sin filtro de usuario (admin viendo todas las tareas)
//   - slice vacío (no nil): retorna lista vacía
//   - 1 o más IDs: filtra por r.user_id IN (...)
func (s *Store) ListTasks(ctx context.Context, allowedUserIDs []int, statusFilter, orderType, orderBy string) ([]Result, error) {
	if allowedUserIDs != nil && len(allowedUserIDs) == 0 {
		return []Result{}, nil
	}

	col := "r.created_at"
	switch orderType {
	case "id":
		col = "r.id"
	case "collection_id":
		col = "r.collection_id"
	case "updated_at":
		col = "r.updated_at"
	}
	dir := "DESC"
	if strings.ToLower(orderBy) == "asc" {
		dir = "ASC"
	}

	var where []string
	var args []any
	argIdx := 1
	if len(allowedUserIDs) == 1 {
		where = append(where, fmt.Sprintf("r.user_id = $%d", argIdx))
		args = append(args, allowedUserIDs[0])
		argIdx++
	} else if len(allowedUserIDs) > 1 {
		where = append(where, fmt.Sprintf("r.user_id = ANY($%d)", argIdx))
		args = append(args, allowedUserIDs)
		argIdx++
	}
	if statusFilter != "" {
		switch strings.ToUpper(statusFilter) {
		case "UPLOADING", "PENDING", "RUNNING", "COMPLETED", "FAILED":
			where = append(where, fmt.Sprintf("rs.status = $%d::resultstatusenum", argIdx))
			args = append(args, strings.ToUpper(statusFilter))
			argIdx++
		}
	}

	query := `
		SELECT r.id, r.collection_id, r.user_id, u.username, r.created_at, r.updated_at,
		       rs.id, rs.result_id, rs.status::text, rs.total_files, rs.total_files_processed,
		       rs.total_items_expected, rs.total_items_processed, rs.models, rs.current_file, rs.last_updated
		FROM result r
		LEFT JOIN "user" u ON u.id = r.user_id
		LEFT JOIN result_status rs ON rs.result_id = r.id
	`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += fmt.Sprintf(" ORDER BY %s %s", col, dir)

	rows, err := s.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Result, 0)
	for rows.Next() {
		var r Result
		var username *string
		var rsID, rsResultID, rsItemsProc *int
		var rsStatus *string
		var rsTotalFiles, rsTotalFilesProc, rsItemsExp *int
		var rsModels, rsCurrFile *string
		var rsLastUpd *time.Time
		if err := rows.Scan(
			&r.ID, &r.CollectionID, &r.UserID, &username, &r.CreatedAt, &r.UpdatedAt,
			&rsID, &rsResultID, &rsStatus, &rsTotalFiles, &rsTotalFilesProc,
			&rsItemsExp, &rsItemsProc, &rsModels, &rsCurrFile, &rsLastUpd,
		); err != nil {
			return nil, err
		}
		if username != nil {
			r.Username = *username
		}
		if rsID != nil && rsStatus != nil {
			st := &ResultStatus{
				ID:                  *rsID,
				ResultID:            *rsResultID,
				Status:              *rsStatus,
				TotalFiles:          rsTotalFiles,
				TotalFilesProcessed: rsTotalFilesProc,
				TotalItemsExpected:  rsItemsExp,
				Models:              rsModels,
				CurrentFile:         rsCurrFile,
			}
			if rsItemsProc != nil {
				st.TotalItemsProcessed = *rsItemsProc
			}
			if rsLastUpd != nil {
				st.LastUpdated = *rsLastUpd
			}
			r.Status = st
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) DeleteTask(ctx context.Context, taskID string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var resID int
	err = tx.QueryRow(ctx, `SELECT id FROM result WHERE collection_id = $1 LIMIT 1`, taskID).Scan(&resID)
	if err == pgx.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	_, _ = tx.Exec(ctx, `DELETE FROM result_item WHERE task_id = $1`, taskID)
	_, _ = tx.Exec(ctx, `DELETE FROM result_file WHERE result_id = $1`, resID)
	_, _ = tx.Exec(ctx, `DELETE FROM result_status WHERE result_id = $1`, resID)
	_, err = tx.Exec(ctx, `DELETE FROM result WHERE id = $1`, resID)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// --- Result Files & Excel Reporting ---

func (s *Store) InsertResultFiles(ctx context.Context, resultID int, taskID string, files []ResultFile) error {
	if len(files) == 0 {
		return nil
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	now := time.Now()
	for _, f := range files {
		src := f.Source
		if src == "" {
			src = "api"
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO result_file (result_id, task_id, filename, file_path, source, pages, size_bytes, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, resultID, taskID, f.Filename, f.FilePath, src, f.Pages, f.SizeBytes, now)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) UpdateResultFilesDir(ctx context.Context, taskID, dirPath string) error {
	if dirPath == "" {
		return nil
	}
	dirPath = strings.TrimRight(dirPath, "/\\")
	_, err := s.Pool.Exec(ctx, `
		UPDATE result_file
		SET file_path = $1 || '/' || filename
		WHERE task_id = $2
	`, dirPath, taskID)
	return err
}

func (s *Store) ListFileReportRows(
	ctx context.Context,
	allowedUserIDs []int,
	statusFilter string,
	fromTime, toTime *time.Time,
) ([]FileReportRow, error) {
	if allowedUserIDs != nil && len(allowedUserIDs) == 0 {
		return []FileReportRow{}, nil
	}

	var where []string
	var args []any
	argIdx := 1

	if len(allowedUserIDs) == 1 {
		where = append(where, fmt.Sprintf("r.user_id = $%d", argIdx))
		args = append(args, allowedUserIDs[0])
		argIdx++
	} else if len(allowedUserIDs) > 1 {
		where = append(where, fmt.Sprintf("r.user_id = ANY($%d)", argIdx))
		args = append(args, allowedUserIDs)
		argIdx++
	}

	if statusFilter != "" {
		switch strings.ToUpper(statusFilter) {
		case "UPLOADING", "PENDING", "RUNNING", "COMPLETED", "FAILED":
			where = append(where, fmt.Sprintf("rs.status = $%d::resultstatusenum", argIdx))
			args = append(args, strings.ToUpper(statusFilter))
			argIdx++
		}
	}

	if fromTime != nil {
		where = append(where, fmt.Sprintf("r.created_at >= $%d", argIdx))
		args = append(args, *fromTime)
		argIdx++
	}
	if toTime != nil {
		where = append(where, fmt.Sprintf("r.created_at <= $%d", argIdx))
		args = append(args, *toTime)
		argIdx++
	}

	whereClause := ""
	if len(where) > 0 {
		whereClause = " WHERE " + strings.Join(where, " AND ")
	}

	// Consulta archivos registrados en result_file + fallback para tareas históricas sin result_file
	query := `
		WITH filtered_tasks AS (
			SELECT r.id AS result_id,
			       r.collection_id AS task_id,
			       r.user_id,
			       COALESCE(u.username, '') AS username,
			       COALESCE(u.role, 'user') AS user_role,
			       COALESCE(rs.models, '') AS models,
			       COALESCE(rs.status::text, 'PENDING') AS status,
			       r.created_at AS task_created_at
			FROM result r
			LEFT JOIN "user" u ON u.id = r.user_id
			LEFT JOIN result_status rs ON rs.result_id = r.id
			` + whereClause + `
		),
		item_alerts AS (
			SELECT ri.task_id,
			       LOWER(COALESCE(ri.data->>'nombre_archivo', ri.data->>'filename', '')) AS fname,
			       COUNT(*) FILTER (
			           WHERE (
			               ri.data->>'nombre_clase' IN ('inclinado', 'rotado', 'con corte de informacion', 'no legible')
			               AND COALESCE(
			                   NULLIF(regexp_replace(COALESCE(ri.data->>'confianza', '0'), '[^0-9.]', '', 'g'), '')::double precision,
			                   0
			               ) >= 80.0
			           ) OR (
			               ri.data ? 'similitud'
			           )
			       )::int AS alerts_count,
			       COUNT(DISTINCT ri.data->>'pagina')::int AS distinct_pages
			FROM result_item ri
			JOIN filtered_tasks ft ON ft.task_id = ri.task_id
			GROUP BY ri.task_id, LOWER(COALESCE(ri.data->>'nombre_archivo', ri.data->>'filename', ''))
		)
		SELECT ft.user_id,
		       ft.username,
		       ft.user_role,
		       ft.task_id,
		       rf.filename,
		       rf.file_path,
		       rf.source,
		       ft.models,
		       ft.status,
		       rf.pages,
		       rf.size_bytes,
		       COALESCE(ia.alerts_count, 0) AS alerts_count,
		       rf.created_at AS processed_at
		FROM filtered_tasks ft
		JOIN result_file rf ON rf.result_id = ft.result_id
		LEFT JOIN item_alerts ia ON ia.task_id = ft.task_id AND ia.fname = LOWER(rf.filename)

		UNION ALL

		SELECT ft.user_id,
		       ft.username,
		       ft.user_role,
		       ft.task_id,
		       COALESCE(NULLIF(ia.fname, ''), ft.task_id) AS filename,
		       '' AS file_path,
		       'api' AS source,
		       ft.models,
		       ft.status,
		       COALESCE(ia.distinct_pages, 0) AS pages,
		       0::bigint AS size_bytes,
		       COALESCE(ia.alerts_count, 0) AS alerts_count,
		       ft.task_created_at AS processed_at
		FROM filtered_tasks ft
		LEFT JOIN item_alerts ia ON ia.task_id = ft.task_id
		WHERE NOT EXISTS (SELECT 1 FROM result_file rf WHERE rf.result_id = ft.result_id)

		ORDER BY processed_at DESC, task_id ASC, filename ASC
	`

	rows, err := s.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]FileReportRow, 0)
	for rows.Next() {
		var r FileReportRow
		if err := rows.Scan(
			&r.UserID, &r.Username, &r.UserRole, &r.TaskID,
			&r.Filename, &r.FilePath, &r.Source, &r.Models, &r.Status,
			&r.Pages, &r.SizeBytes, &r.AlertsCount, &r.ProcessedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
