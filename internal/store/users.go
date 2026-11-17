package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

type User struct {
	ID                int64  `json:"id"`
	Email             string `json:"email"`
	PasswordHash      string `json:"-"`
	Role              string `json:"role"`
	Status            string `json:"status"`
	VM2APIUserID      string `json:"vm2api_user_id"`
	VM2APIPoolID      string `json:"vm2api_pool_id"`
	VM2APIUsername    string `json:"vm2api_username"`
	VM2APIPasswordEnc string `json:"-"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
	// Admin-only fields (003_user_admin.sql).
	Note        string `json:"-"`
	LastLoginAt string `json:"-"`
	LastLoginIP string `json:"-"`
}

type Users struct{ db *DB }

func NewUsers(db *DB) *Users { return &Users{db: db} }

const userColumns = `id, email, password_hash, role, status,
	COALESCE(vm2api_user_id,''), COALESCE(vm2api_username,''), COALESCE(vm2api_password_enc,''),
	created_at, updated_at, note, COALESCE(last_login_at,''), COALESCE(last_login_ip,''), COALESCE(vm2api_pool_id,'')`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.Status,
		&u.VM2APIUserID, &u.VM2APIUsername, &u.VM2APIPasswordEnc, &u.CreatedAt, &u.UpdatedAt,
		&u.Note, &u.LastLoginAt, &u.LastLoginIP, &u.VM2APIPoolID)
	return u, err
}

func (r *Users) Create(ctx context.Context, email, passwordHash, role string) (User, error) {
	return createUser(ctx, r.db.DB, email, passwordHash, role)
}

// CreateTx inserts a user inside the caller transaction.
func (r *Users) CreateTx(ctx context.Context, tx *sql.Tx, email, passwordHash, role string) (User, error) {
	return createUser(ctx, tx, email, passwordHash, role)
}

type userWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func createUser(ctx context.Context, q userWriter, email, passwordHash, role string) (User, error) {
	now := Now()
	res, err := q.ExecContext(ctx, `INSERT INTO users (email, password_hash, role, status, created_at, updated_at)
		VALUES (?, ?, ?, 'active', ?, ?)`, strings.ToLower(strings.TrimSpace(email)), passwordHash, role, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return User{}, ErrEmailTaken
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	id, _ := res.LastInsertId()
	return scanUser(q.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

var ErrEmailTaken = errors.New("email already registered")

func (r *Users) GetByID(ctx context.Context, id int64) (User, error) {
	u, err := scanUser(r.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}

func (r *Users) GetByEmail(ctx context.Context, email string) (User, error) {
	u, err := scanUser(r.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ?`,
		strings.ToLower(strings.TrimSpace(email))))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user by email: %w", err)
	}
	return u, nil
}

func (r *Users) Count(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM users`).Scan(&n)
	return n, err
}

func (r *Users) CountAdmins(ctx context.Context) (int64, error) {
	var n int64
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM users WHERE role='admin' AND status='active'`).Scan(&n)
	return n, err
}

// List returns users matching q (email substring) with paging.
func (r *Users) List(ctx context.Context, q string, page, pageSize int) ([]User, int64, error) {
	return r.Search(ctx, UserFilter{Q: q}, page, pageSize)
}

// UserFilter narrows Search; empty fields match everything.
type UserFilter struct {
	Q      string // substring of the email, or "#<id>" / "<id>" for an exact id
	Role   string
	Status string
}

func (r *Users) Search(ctx context.Context, filter UserFilter, page, pageSize int) ([]User, int64, error) {
	var clauses []string
	var args []any
	if q := strings.TrimSpace(filter.Q); q != "" {
		if id, err := strconv.ParseInt(strings.TrimPrefix(q, "#"), 10, 64); err == nil && id > 0 {
			clauses = append(clauses, `(id = ? OR email LIKE ? ESCAPE '\')`)
			args = append(args, id, "%"+likeEscape(strings.ToLower(q))+"%")
		} else {
			clauses = append(clauses, `(email LIKE ? ESCAPE '\' OR note LIKE ? ESCAPE '\')`)
			args = append(args, "%"+likeEscape(strings.ToLower(q))+"%", "%"+likeEscape(q)+"%")
		}
	}
	if filter.Role != "" {
		clauses = append(clauses, `role = ?`)
		args = append(args, filter.Role)
	}
	if filter.Status != "" {
		clauses = append(clauses, `status = ?`)
		args = append(args, filter.Status)
	}
	where := ""
	if len(clauses) > 0 {
		where = " WHERE " + strings.Join(clauses, " AND ")
	}
	var total int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM users`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := r.db.QueryContext(ctx, `SELECT `+userColumns+` FROM users`+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

func (r *Users) SetPassword(ctx context.Context, id int64, hash string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE users SET password_hash=?, updated_at=? WHERE id=?`, hash, Now(), id)
	return err
}

func (r *Users) SetStatus(ctx context.Context, id int64, status string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE users SET status=?, updated_at=? WHERE id=?`, status, Now(), id)
	return err
}

func (r *Users) SetRole(ctx context.Context, id int64, role string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE users SET role=?, updated_at=? WHERE id=?`, role, Now(), id)
	return err
}

func (r *Users) SetVM2APIIdentity(ctx context.Context, id int64, userID, username, passwordEnc string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE users SET vm2api_user_id=?, vm2api_username=?, vm2api_password_enc=?, updated_at=? WHERE id=?`,
		userID, username, passwordEnc, Now(), id)
	return err
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// SetEmail changes the sign-in email; ErrEmailTaken when another user has it.
func (r *Users) SetEmail(ctx context.Context, id int64, email string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE users SET email=?, updated_at=? WHERE id=?`, strings.ToLower(strings.TrimSpace(email)), Now(), id)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrEmailTaken
	}
	return err
}

func (r *Users) SetNote(ctx context.Context, id int64, note string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE users SET note=?, updated_at=? WHERE id=?`, note, Now(), id)
	return err
}

// RecordLogin stamps a successful sign-in.
func (r *Users) RecordLogin(ctx context.Context, id int64, ip string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE users SET last_login_at=?, last_login_ip=? WHERE id=?`, Now(), truncate(ip, 64), id)
	return err
}

// UserRecords counts what ties a user to the business: a user with any of
// these cannot be deleted, only disabled.
type UserRecords struct {
	Orders        int64 `json:"orders"`
	Subscriptions int64 `json:"subscriptions"`
	Resources     int64 `json:"resources"`
	// Tickets counts tickets the user opened, ticket messages they wrote
	// (admin replies included) and files they uploaded: support history is
	// kept, so such a user is disabled rather than deleted.
	Tickets int64 `json:"tickets"`
}

func (records UserRecords) Any() bool {
	return records.Orders > 0 || records.Subscriptions > 0 || records.Resources > 0 || records.Tickets > 0
}

// userRecordsQuery counts every row that blocks deleting user ? (the id is
// bound six times).
const userRecordsQuery = `SELECT
		(SELECT COUNT(1) FROM orders WHERE user_id = ?),
		(SELECT COUNT(1) FROM subscriptions WHERE user_id = ?),
		(SELECT COUNT(1) FROM resources WHERE user_id = ?),
		(SELECT COUNT(1) FROM tickets WHERE user_id = ?)
		+ (SELECT COUNT(1) FROM ticket_messages WHERE author_id = ?)
		+ (SELECT COUNT(1) FROM ticket_attachments WHERE uploader_id = ?)`

func (r *Users) Records(ctx context.Context, id int64) (UserRecords, error) {
	var out UserRecords
	err := r.db.QueryRowContext(ctx, userRecordsQuery, id, id, id, id, id, id).Scan(&out.Orders, &out.Subscriptions, &out.Resources, &out.Tickets)
	if err != nil {
		return out, fmt.Errorf("count user records: %w", err)
	}
	return out, nil
}

// ErrUserHasRecords blocks deleting a user with orders, subscriptions, resources or ticket history.
var ErrUserHasRecords = errors.New("user has business records")

// Delete removes a user with no business records, with its API key rows and
// sessions. Upstream (vm2api) keys must already be deleted by the caller.
func (r *Users) Delete(ctx context.Context, id int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete user: %w", err)
	}
	defer tx.Rollback()
	var records UserRecords
	if err := tx.QueryRowContext(ctx, userRecordsQuery, id, id, id, id, id, id).Scan(&records.Orders, &records.Subscriptions, &records.Resources, &records.Tickets); err != nil {
		return fmt.Errorf("count user records: %w", err)
	}
	if records.Any() {
		return ErrUserHasRecords
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM api_keys WHERE user_id = ?`, id); err != nil {
		return fmt.Errorf("delete user keys: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return fmt.Errorf("delete user sessions: %w", err)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}
