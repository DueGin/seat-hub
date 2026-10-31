package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type User struct {
	ID                int64  `json:"id"`
	Email             string `json:"email"`
	PasswordHash      string `json:"-"`
	Role              string `json:"role"`
	Status            string `json:"status"`
	VM2APIUserID      string `json:"vm2api_user_id"`
	VM2APIUsername    string `json:"vm2api_username"`
	VM2APIPasswordEnc string `json:"-"`
	CreatedAt         string `json:"created_at"`
	UpdatedAt         string `json:"updated_at"`
}

type Users struct{ db *DB }

func NewUsers(db *DB) *Users { return &Users{db: db} }

const userColumns = `id, email, password_hash, role, status,
	COALESCE(vm2api_user_id,''), COALESCE(vm2api_username,''), COALESCE(vm2api_password_enc,''),
	created_at, updated_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.Status,
		&u.VM2APIUserID, &u.VM2APIUsername, &u.VM2APIPasswordEnc, &u.CreatedAt, &u.UpdatedAt)
	return u, err
}

func (r *Users) Create(ctx context.Context, email, passwordHash, role string) (User, error) {
	now := Now()
	res, err := r.db.ExecContext(ctx, `INSERT INTO users (email, password_hash, role, status, created_at, updated_at)
		VALUES (?, ?, ?, 'active', ?, ?)`, strings.ToLower(strings.TrimSpace(email)), passwordHash, role, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return User{}, ErrEmailTaken
		}
		return User{}, fmt.Errorf("create user: %w", err)
	}
	id, _ := res.LastInsertId()
	return r.GetByID(ctx, id)
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
	where, args := "", []any{}
	if q != "" {
		where = ` WHERE email LIKE ?`
		args = append(args, "%"+strings.ToLower(q)+"%")
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
